package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"picoclaws/internal/archive"
	"picoclaws/internal/assets"
	"picoclaws/internal/platform/aws"
	"picoclaws/internal/platform/telegram/tgutil"
	"picoclaws/internal/transcriber"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mymmrac/telego"
	"github.com/rs/zerolog/log"
	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels/telegram"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// WorkerApp manages the lifecycle of a worker instance, handling
// workspace preparation, agent execution, and message processing.
type WorkerApp struct {
	Agent           *agent.AgentLoop
	Channel         *telegram.TelegramChannel
	Bus             *bus.MessageBus
	Bot             *telego.Bot
	MediaStore      media.MediaStore
	Transcriber     *transcriber.WhisperTranscriber
	BaseDir         string
	BotToken        string
	initPyPath      string
	TaskExecutorURL string
}

type HeartbeatMessage struct {
	Type   string `json:"type"`
	ChatID string `json:"chat_id"`
}

type TaskResultMessage struct {
	Type     string         `json:"type"`
	TaskID   string         `json:"task_id"`
	ChatID   string         `json:"chat_id"`
	Skill    string         `json:"skill"`
	Result   string         `json:"result"`
	IsError  bool           `json:"is_error"`
	Metadata map[string]any `json:"metadata"`
}

func NewWorkerApp(ctx context.Context) (*WorkerApp, error) {
	// Prepend bundled binaries to PATH so tools (like exec) can find BusyBox applets and Python
	currentPath := os.Getenv("PATH")
	assetsBin, _ := filepath.Abs("assets/bin")
	assetsPythonBin, _ := filepath.Abs("assets/python/bin")

	newPath := currentPath
	if _, err := os.Stat(assetsBin); err == nil {
		newPath = assetsBin + ":" + newPath
	}
	if _, err := os.Stat(assetsPythonBin); err == nil {
		newPath = assetsPythonBin + ":" + newPath
	}
	os.Setenv("PATH", newPath)
	log.Ctx(ctx).Debug().Str("path", os.Getenv("PATH")).Msg("Updated PATH with bundled binaries")

	initPyPath := os.Getenv("PYTHONPATH")

	baseDir := os.Getenv("PICOCLAW_HOME")
	if baseDir == "" {
		baseDir = "/tmp/picoclaw"
	}

	configPath := os.Getenv("PICOCLAW_CONFIG")
	if configPath == "" {
		// Try current directory first for better local dev experience
		if _, err := os.Stat("config.json"); err == nil {
			configPath = "config.json"
		} else {
			configPath = filepath.Join(baseDir, "config.json")
		}
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	modelName := cfg.Agents.Defaults.GetModelName()
	if modelName == "" {
		// Use a safe fallback if not configured
		modelName = "gemini-2.0-flash"
	}

	modelCfg, err := cfg.GetModelConfig(modelName)
	if err != nil {
		return nil, fmt.Errorf("failed to find model config for %s (config path: %s): %w", modelName, configPath, err)
	}

	provider, _, err := providers.CreateProviderFromConfig(modelCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM provider: %w", err)
	}

	msgBus := bus.NewMessageBus()

	tgBC := cfg.Channels.GetByType(config.ChannelTelegram)
	if tgBC == nil {
		return nil, fmt.Errorf("telegram channel not configured")
	}
	tgDecoded, err := tgBC.GetDecoded()
	if err != nil {
		return nil, fmt.Errorf("failed to decode telegram settings: %w", err)
	}
	tgSettings, ok := tgDecoded.(*config.TelegramSettings)
	if !ok {
		return nil, fmt.Errorf("invalid telegram settings type")
	}

	bot, err := telego.NewBot(tgSettings.Token.String())
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	// 1. Configure picoclaw logger
	logger.SetLevelFromString(os.Getenv("LOG_LEVEL"))
	if os.Getenv("PICOCLAW_LOG_FILE") != "" {
		logger.ConfigureFromEnv()
	} else if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		// In Lambda, use JSON output for CloudWatch compatibility
		logger.UseJSONOutput()
	}

	tgChan, err := telegram.NewTelegramChannel(tgBC, tgSettings, msgBus)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram channel: %w", err)
	}
	tgChan.SetRunning(true)

	mediaStore := media.NewFileMediaStore()
	tgChan.SetMediaStore(mediaStore)

	agentLoop := agent.NewAgentLoop(cfg, msgBus, provider)
	agentLoop.SetMediaStore(mediaStore)

	tr, err := transcriber.NewTranscriber(cfg, mediaStore)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to initialize transcriber (voice messages will not be transcribed)")
	}

	return &WorkerApp{
		Agent:           agentLoop,
		Channel:         tgChan,
		Bus:             msgBus,
		Bot:             bot,
		MediaStore:      mediaStore,
		Transcriber:     tr,
		BotToken:        tgSettings.Token.String(),
		BaseDir:         baseDir,
		initPyPath:      initPyPath,
		TaskExecutorURL: os.Getenv("TASK_EXECUTOR_URL"),
	}, nil
}

func (a *WorkerApp) Handle(ctx context.Context, sqsEvent events.SQSEvent) error {
	logger := log.Ctx(ctx).With().Str("component", "tg-worker").Logger()
	ctx = logger.WithContext(ctx)

	for _, record := range sqsEvent.Records {
		if err := a.processSQSRecord(ctx, record); err != nil {
			log.Ctx(ctx).Error().Str("message_id", record.MessageId).Err(err).Msg("Failed to process SQS record")
		}
	}
	return nil
}

func (a *WorkerApp) processSQSRecord(ctx context.Context, record events.SQSMessage) error {
	logger := log.Ctx(ctx).With().Str("sqs_message_id", record.MessageId).Logger()
	ctx = logger.WithContext(ctx)

	var msg map[string]any
	if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
		logger.Error().Err(err).Msg("Failed to unmarshal SQS message body")
		return nil
	}

	// 1. Heartbeat check
	handled, err := a.tryHandleHeartbeat(ctx, record)
	if handled {
		return err
	}

	// 2. Task Result check
	handled, err = a.tryHandleTaskResult(ctx, record)
	if handled {
		return err
	}

	// 3. Fallback to Telegram Update
	return a.handleTelegramUpdate(ctx, record)
}

func (a *WorkerApp) tryHandleHeartbeat(ctx context.Context, record events.SQSMessage) (bool, error) {
	var hb HeartbeatMessage
	if err := json.Unmarshal([]byte(record.Body), &hb); err != nil || hb.Type != "heartbeat" {
		return false, nil
	}

	log.Ctx(ctx).Info().Str("chat_id", hb.ChatID).Msg("Processing internal heartbeat message")
	inMsg := &bus.InboundMessage{
		Context: bus.InboundContext{
			Channel: "telegram",
			ChatID:  hb.ChatID,
		},
		ChatID:  hb.ChatID,
		Channel: "telegram",
	}
	return true, a.processAgentTurn(ctx, hb.ChatID, inMsg, true, nil)
}

func (a *WorkerApp) tryHandleTaskResult(ctx context.Context, record events.SQSMessage) (bool, error) {
	var tr TaskResultMessage
	if err := json.Unmarshal([]byte(record.Body), &tr); err != nil || tr.Type != "task_result" {
		return false, nil
	}

	log.Ctx(ctx).Info().
		Str("chat_id", tr.ChatID).
		Str("task_id", tr.TaskID).
		Str("skill", tr.Skill).
		Msg("Processing background task result")

	status := "success"
	if tr.IsError {
		status = "error"
	}

	inMsg := &bus.InboundMessage{
		Context: bus.InboundContext{
			Channel: "telegram",
			ChatID:  tr.ChatID,
		},
		ChatID:  tr.ChatID,
		Channel: "telegram",
		Content: fmt.Sprintf("[Background Task Result]\nSkill: %s\nTask ID: %s\nChat ID: %s\nStatus: %s\nOutput: %s", tr.Skill, tr.TaskID, tr.ChatID, status, tr.Result),
	}
	if tr.Metadata == nil {
		tr.Metadata = make(map[string]any)
	}
	tr.Metadata["_task_id"] = tr.TaskID
	return true, a.processAgentTurn(ctx, tr.ChatID, inMsg, false, tr.Metadata)
}

func (a *WorkerApp) handleTelegramUpdate(ctx context.Context, record events.SQSMessage) error {
	var update telego.Update
	if err := json.Unmarshal([]byte(record.Body), &update); err != nil {
		return fmt.Errorf("unmarshal telegram update: %w", err)
	}

	// Safety check: Skip updates that don't contain a processable message
	if tgutil.ExtractMessage(update) == nil {
		log.Ctx(ctx).Info().
			Int("update_id", update.UpdateID).
			Str("message_id", record.MessageId).
			Str("raw_body", record.Body).
			Msg("Skipping SQS record: no processable Telegram message found")
		return nil
	}

	inMsg, err := tgutil.TranslateUpdate(ctx, a.Bot, update, a.MediaStore, "")
	if err != nil {
		log.Ctx(ctx).Warn().
			Str("raw_body", record.Body).
			Err(err).
			Msg("Failed to translate update. Raw body logged.")
		return fmt.Errorf("translate update: %w", err)
	}

	// Automatic Transcription
	if len(inMsg.Media) > 0 {
		a.handleMediaTranscription(ctx, inMsg)
	}

	// Handle special system commands (e.g., /reset)
	a.handleSpecialCommands(ctx, inMsg)

	return a.processAgentTurn(ctx, inMsg.Context.ChatID, inMsg, false, nil)
}

func (a *WorkerApp) handleMediaTranscription(ctx context.Context, inMsg *bus.InboundMessage) {
	var newMedia []string
	for _, ref := range inMsg.Media {
		path, meta, err := a.MediaStore.ResolveWithMeta(ref)
		if err != nil {
			newMedia = append(newMedia, ref)
			continue
		}

		if strings.HasSuffix(strings.ToLower(meta.Filename), ".ogg") || strings.HasSuffix(strings.ToLower(meta.Filename), ".mp3") {
			if a.Transcriber != nil {
				res, err := a.Transcriber.Transcribe(ctx, path)
				if err == nil && res.Text != "" {
					log.Ctx(ctx).Info().Str("transcript", res.Text).Msg("Voice transcribed successfully")
					if inMsg.Content == "[voice]" || inMsg.Content == "[empty message]" {
						inMsg.Content = res.Text
					} else {
						inMsg.Content = fmt.Sprintf("%s\n\n[voice: %s]", inMsg.Content, res.Text)
					}
				} else {
					errStr := "unknown error"
					if err != nil {
						errStr = err.Error()
					}
					log.Ctx(ctx).Warn().Str("error", errStr).Msg("Voice transcription failed")
					if inMsg.Content == "[voice]" || inMsg.Content == "[empty message]" {
						inMsg.Content = "[Unintelligible voice message]"
					}
				}
			} else {
				log.Ctx(ctx).Warn().Msg("Voice message received but Transcriber is not configured")
			}
			// Don't pass audio to LLM context under ANY circumstances
			continue
		}
		newMedia = append(newMedia, ref)
	}
	inMsg.Media = newMedia
}

func (a *WorkerApp) processAgentTurn(ctx context.Context, chatID string, inMsg *bus.InboundMessage, isHeartbeat bool, taskMetadata map[string]any) error {
	logger := log.Ctx(ctx).With().Str("chat_id", chatID).Logger()
	ctx = logger.WithContext(ctx)

	bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	var s3Storage *aws.S3Storage
	if bucket != "" {
		var err error
		s3Storage, err = aws.NewS3Storage(ctx, bucket)
		if err != nil {
			log.Ctx(ctx).Error().Err(err).Msg("Failed to initialize S3 storage")
		}
	}

	chatWorkspace := a.getWorkspacePath(chatID)

	// 1. Prepare Workspace (Download from S3)
	isNew, err := a.prepareWorkspace(ctx, s3Storage, chatID, chatWorkspace)
	if err != nil {
		return err
	}

	// 2. Setup Context and Environment
	a.restoreTaskMetadata(ctx, chatWorkspace, taskMetadata)
	a.configureEnvironment(chatWorkspace)

	if isHeartbeat {
		inMsg.Content = a.buildHeartbeatPrompt()
	}

	// 3. Create Agent
	agentInst, err := a.createAgent(chatID, chatWorkspace)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	defer agentInst.Close()

	if isNew {
		a.initUserMetadata(chatWorkspace, inMsg)
	}

	// 4. Ingest Media
	a.materializeMedia(ctx, chatWorkspace, inMsg)

	// 5. Execute Turn
	processErr, modified := a.executeTurn(ctx, agentInst, inMsg, chatWorkspace, isHeartbeat)

	// 6. Finalize (Upload to S3)
	if modified {
		a.finalizeWorkspace(ctx, s3Storage, chatID, chatWorkspace)
	} else {
		logger.Info().Msg("Workspace unchanged, skipping S3 upload")
	}

	return processErr
}

func (a *WorkerApp) restoreTaskMetadata(ctx context.Context, chatWorkspace string, taskMetadata map[string]any) {
	if len(taskMetadata) == 0 {
		return
	}

	taskID, ok := taskMetadata["_task_id"].(string)
	if !ok || taskID == "" {
		return
	}

	metaStr, _ := json.Marshal(taskMetadata)
	jobDir := filepath.Join(chatWorkspace, "memory", "jobs")
	_ = os.MkdirAll(jobDir, 0755)
	_ = os.WriteFile(filepath.Join(jobDir, fmt.Sprintf("%s.json", taskID)), metaStr, 0644)
	log.Ctx(ctx).Debug().Str("task_id", taskID).Msg("Restored task metadata to workspace")
}

func (a *WorkerApp) configureEnvironment(chatWorkspace string) {
	pythonPackagesDir := filepath.Join(chatWorkspace, ".python_packages")
	_ = os.MkdirAll(pythonPackagesDir, 0755)

	os.Setenv("HOME", chatWorkspace)
	os.Setenv("PIP_TARGET", pythonPackagesDir)
	os.Setenv("PIP_NO_CACHE_DIR", "1")

	newPyPath := pythonPackagesDir
	if a.initPyPath != "" {
		newPyPath = pythonPackagesDir + ":" + a.initPyPath
	}
	os.Setenv("PYTHONPATH", newPyPath)
}

func (a *WorkerApp) materializeMedia(ctx context.Context, chatWorkspace string, inMsg *bus.InboundMessage) {
	if len(inMsg.Media) == 0 {
		return
	}

	inboxDir := filepath.Join(chatWorkspace, "inbox")
	_ = os.MkdirAll(inboxDir, 0755)
	var attachedFiles []string

	for _, ref := range inMsg.Media {
		path, meta, err := a.MediaStore.ResolveWithMeta(ref)
		if err != nil {
			log.Ctx(ctx).Warn().Err(err).Str("ref", ref).Msg("Failed to resolve media ref for inbox")
			continue
		}

		destName := meta.Filename
		if destName == "" {
			destName = filepath.Base(path)
		}
		destPath := filepath.Join(inboxDir, destName)

		err = func() error {
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			defer src.Close()

			dst, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer dst.Close()

			_, err = io.Copy(dst, src)
			return err
		}()

		if err != nil {
			log.Ctx(ctx).Error().Err(err).Str("src", path).Str("dst", destPath).Msg("Failed to copy media to inbox")
		} else {
			attachedFiles = append(attachedFiles, destName)
		}
	}

	if len(attachedFiles) > 0 {
		hint := "[System: User attached files in inbox/: " + strings.Join(attachedFiles, ", ") + "]"
		if inMsg.Content != "" {
			inMsg.Content += "\n\n" + hint
		} else {
			inMsg.Content = hint
		}
	}
}


func (a *WorkerApp) buildHeartbeatPrompt() string {
	now := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("System: Internal heartbeat check at %s. This is a background task. DO NOT respond directly to the user. Use this time to run tools, monitor logs, or continue ongoing work. If you have nothing to do, simply say 'No tasks'.", now)
}

// handleSpecialCommands processes incoming messages for specific triggers
// like /reset to nudge the agent behaviorally via system instructions.
func (a *WorkerApp) handleSpecialCommands(ctx context.Context, inMsg *bus.InboundMessage) {
	if strings.TrimSpace(inMsg.Content) == "/reset" {
		inMsg.Content = "System: The user has just reset your memory and workspace. " +
			"Greet them briefly according to your personality."
		log.Ctx(ctx).Info().Msg("Special command /reset processed as system nudge")

		// Execute physical reset
		if err := a.resetWorkspace(ctx, inMsg.Context.ChatID); err != nil {
			log.Ctx(ctx).Error().Err(err).Msg("Failed to reset workspace")
		}
	}
}

// resetWorkspace physically deletes the local workspace and S3 archive.
func (a *WorkerApp) resetWorkspace(ctx context.Context, chatID string) error {
	chatWorkspace := a.getWorkspacePath(chatID)
	log.Ctx(ctx).Info().Str("chat_id", chatID).Msg("Performing physical workspace reset")

	// 1. Wipe local
	_ = os.RemoveAll(chatWorkspace)

	// 2. Wipe S3 if configured
	bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	if bucket != "" {
		s3Storage, err := aws.NewS3Storage(ctx, bucket)
		if err != nil {
			return fmt.Errorf("s3 storage init: %w", err)
		}
		s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)
		if err := s3Storage.Delete(ctx, s3Key); err != nil {
			log.Ctx(ctx).Warn().Err(err).Str("key", s3Key).Msg("Failed to delete S3 archive during reset")
		}
	}
	return nil
}

// getWorkspacePath calculates the base workspace path for a given chat.
func (a *WorkerApp) getWorkspacePath(chatID string) string {
	return filepath.Join(a.BaseDir, "chats", chatID)
}

// prepareWorkspace ensures the local environment is ready for the agent to work in.
// It syncs from S3 if needed and restores default assets for fresh workspaces.
// Returns true if the workspace was initialized from scratch (assets restored).
func (a *WorkerApp) prepareWorkspace(ctx context.Context, s3Storage *aws.S3Storage, chatID, chatWorkspace string) (bool, error) {
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	localVersionFile := filepath.Join(chatWorkspace, ".version")

	// 1. Check S3 Metadata (ETag/LastModified)
	var s3Meta *aws.S3Metadata
	if s3Storage != nil {
		if meta, err := s3Storage.GetMetadata(ctx, s3Key); err == nil {
			s3Meta = meta
		}
	}

	// 2. Performance: If /tmp folder exists, compare with S3 metadata
	useWarmWorkspace := false
	if _, err := os.Stat(chatWorkspace); err == nil && s3Meta != nil {
		if verData, err := os.ReadFile(localVersionFile); err == nil {
			if string(verData) == s3Meta.ETag {
				logger.Info().Str("chat_id", chatID).Msg("Reusing warm workspace from /tmp; skipping S3 download")
				useWarmWorkspace = true
			}
		}
	}

	restoredFromS3 := useWarmWorkspace
	if !useWarmWorkspace && s3Storage != nil {
		data, err := s3Storage.Download(ctx, s3Key)
		if err == nil && data != nil {
			if err := archive.Unarchive(data, chatWorkspace); err == nil {
				restoredFromS3 = true
				if s3Meta != nil {
					_ = os.WriteFile(localVersionFile, []byte(s3Meta.ETag), 0o644)
				}
			}
		}
	}

	if !restoredFromS3 {
		logger.Info().Str("chat_id", chatID).Msg("No S3 archive found, wiping local workspace for clean start")
		_ = os.RemoveAll(chatWorkspace) // Ensure clean state
		if err := os.MkdirAll(chatWorkspace, 0755); err != nil {
			return false, fmt.Errorf("failed to create directory: %w", err)
		}
		if err := assets.RestoreWorkspace(chatWorkspace); err != nil {
			return false, fmt.Errorf("failed to restore embedded workspace: %w", err)
		}
		// Fresh workspace doesn't have a version in S3 yet
		_ = os.Remove(localVersionFile)
	}

	if err := os.MkdirAll(filepath.Join(chatWorkspace, "memory"), 0o755); err != nil {
		return false, fmt.Errorf("failed to create memory directory: %w", err)
	}

	return !restoredFromS3, nil
}

func (a *WorkerApp) createAgent(chatID string, workspacePath string) (*agent.AgentInstance, error) {
	globalConfig := a.Agent.GetConfig()
	defaultAgent := a.Agent.GetRegistry().GetDefaultAgent()
	if defaultAgent == nil {
		return nil, fmt.Errorf("default agent not found in registry")
	}

	agentID := "main"
	var agentCfg *config.AgentConfig
	for i := range globalConfig.Agents.List {
		if globalConfig.Agents.List[i].ID == agentID {
			agentCfg = &globalConfig.Agents.List[i]
			break
		}
	}

	if agentCfg == nil {
		agentCfg = &config.AgentConfig{ID: agentID, Default: true}
	}

	requestAgentCfg := *agentCfg
	requestAgentCfg.Workspace = workspacePath

	inst := agent.NewAgentInstance(&requestAgentCfg, &globalConfig.Agents.Defaults, globalConfig, defaultAgent.Provider)
	a.EquipAgent(inst, chatID)
	return inst, nil
}

func (a *WorkerApp) executeTurn(ctx context.Context, agentInst *agent.AgentInstance, inMsg *bus.InboundMessage, chatWorkspaceBase string, isHeartbeat bool) (error, bool) {
	logger := log.Ctx(ctx)

	// 1. Create a timestamp marker to detect CHANGES in the workspace
	markerFile := filepath.Join(os.TempDir(), fmt.Sprintf("marker_%d", time.Now().UnixNano()))
	if f, err := os.OpenFile(markerFile, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString("start")
		_ = f.Sync() // Ensure it's on disk with current timestamp
		f.Close()
	}
	defer os.Remove(markerFile)

	// Outbound collection
	var wg sync.WaitGroup
	outCtx, cancelOut := context.WithCancel(ctx)
	// We don't defer cancelOut here because we want to control the shutdown sequence below

	sendResponse := func(msg bus.OutboundMessage) {
		if _, err := a.Channel.Send(ctx, msg); err != nil {
			logger.Error().Err(err).Str("chat_id", msg.ChatID).Msg("Failed to send response")
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-outCtx.Done():
				// FINAL DRAIN: Try to get any remaining messages sent after agent finished
				for {
					select {
					case msg, ok := <-a.Bus.OutboundChan():
						if ok {
							sendResponse(msg)
							continue
						}
					case msg, ok := <-a.Bus.OutboundMediaChan():
						if ok {
							if msg.ChatID != "main" {
								if _, err := a.Channel.SendMedia(ctx, msg); err != nil {
									logger.Error().Err(err).Msg("Failed to send media")
								}
							}
							continue
						}
					default:
					}
					break
				}
				return
			case msg, ok := <-a.Bus.OutboundChan():
				if ok {
					sendResponse(msg)
				}
			case msg, ok := <-a.Bus.OutboundMediaChan():
				if ok {
					if msg.ChatID != "main" {
						if _, err := a.Channel.SendMedia(ctx, msg); err != nil {
							logger.Error().Err(err).Msg("Failed to send media")
						}
					}
				}
			}
		}
	}()

	driver := &Driver{
		Bus:        a.Bus,
		MediaStore: a.MediaStore,
		Config:     a.Agent.GetConfig(),
	}

	processTimeout := 3 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		// Reserve 30 seconds for graceful shutdown and S3 persistence
		remaining := time.Until(deadline) - 30*time.Second
		if remaining > 0 {
			processTimeout = remaining
		}
	} else if t, err := time.ParseDuration(os.Getenv("WORKER_PROCESS_TIMEOUT")); err == nil {
		processTimeout = t
	}
	processCtx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	stopTyping, _ := a.Channel.StartTyping(ctx, inMsg.ChatID)
	if stopTyping != nil {
		defer stopTyping()
	}

	userMessage := inMsg.Content
	if isHeartbeat {
		heartbeatPath := filepath.Join(chatWorkspaceBase, "HEARTBEAT.md")
		if data, err := os.ReadFile(heartbeatPath); err == nil {
			userMessage = string(data)
		}
	}

	sessionKey := inMsg.ChatID
	// Heartbeat uses the user's session key to see recent context,
	// but we set DisableHistory to true so the heartbeat turn itself
	// isn't recorded and doesn't pollute the chat history.

	_, err := driver.RunAgent(processCtx, agentInst, processOptions{
		SessionKey:           sessionKey,
		Channel:              "telegram",
		ChatID:               inMsg.ChatID,
		MessageID:            inMsg.Context.MessageID,
		UserMessage:          userMessage,
		Media:                inMsg.Media,
		EnableSummary:        true,
		SendResponse:         !isHeartbeat && inMsg.ChatID != "main",
		SenderID:             inMsg.SenderID,
		SenderDisplayName:    inMsg.Sender.DisplayName,
		SuppressToolFeedback: strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_TOOLS")) != "false",
		SuppressReasoning:    strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_REASONING")) != "false",
		DisableHistory:       isHeartbeat,
	})

	// Wait for a small grace period to allow any last-second async messages to enter the bus
	time.Sleep(100 * time.Millisecond)

	// Shutdown the collector and wait for final drain
	cancelOut()
	wg.Wait()

	// 2. SMART DETECTOR: Check if workspace was modified
	modified := true // Default to true for safety
	cmd := exec.CommandContext(ctx, "find", chatWorkspaceBase, "-newer", markerFile)
	cmd.Env = a.buildEnv()
	output, findErr := cmd.Output()
	if findErr == nil {
		// If output is empty, nothing was changed
		if len(strings.TrimSpace(string(output))) == 0 {
			modified = false
		}
	}

	return err, modified
}

func (a *WorkerApp) buildEnv() []string {
	env := os.Environ()
	// Filter out existing HOME, PYTHONPATH, etc. to avoid duplicates
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "HOME=") &&
			!strings.HasPrefix(e, "PYTHONPATH=") &&
			!strings.HasPrefix(e, "PIP_TARGET=") &&
			!strings.HasPrefix(e, "PIP_NO_CACHE_DIR=") {
			filtered = append(filtered, e)
		}
	}

	filtered = append(filtered, "HOME="+os.Getenv("HOME"))
	filtered = append(filtered, "PYTHONPATH="+os.Getenv("PYTHONPATH"))
	filtered = append(filtered, "PIP_TARGET="+os.Getenv("PIP_TARGET"))
	filtered = append(filtered, "PIP_NO_CACHE_DIR="+os.Getenv("PIP_NO_CACHE_DIR"))
	return filtered
}

func (a *WorkerApp) finalizeWorkspace(ctx context.Context, s3Storage *aws.S3Storage, chatID, chatWorkspace string) {
	if s3Storage == nil {
		return
	}
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	if archiveData, err := archive.Archive(chatWorkspace); err == nil {
		if err := s3Storage.Upload(ctx, s3Key, archiveData); err != nil {
			logger.Warn().Err(err).Str("key", s3Key).Msg("Failed to upload workspace to S3")
		} else {
			// Update local .version with new S3 state to ensure next warm hit knows we are fresh
			if meta, err := s3Storage.GetMetadata(ctx, s3Key); err == nil && meta != nil {
				localVersionFile := filepath.Join(chatWorkspace, ".version")
				_ = os.WriteFile(localVersionFile, []byte(meta.ETag), 0o644)
			}
		}
	}
	// We DO NOT RemoveAll here anymore to persist /tmp for the next warm invocation.
}

func (a *WorkerApp) initUserMetadata(workspace string, inMsg *bus.InboundMessage) {
	if inMsg == nil || inMsg.Sender.DisplayName == "" && inMsg.Sender.Username == "" {
		return // No sender info available (e.g. heartbeat)
	}
	userFile := filepath.Join(workspace, "USER.md")

	// Only initialize if the file is still a skeleton or missing
	data, err := os.ReadFile(userFile)
	if err == nil && !strings.Contains(string(data), "(optional)") && !strings.Contains(string(data), "goes here") {
		return // Already personalized or modified
	}

	var sb strings.Builder
	sb.WriteString("# User\n\n")
	sb.WriteString("## Personal Information\n")
	if inMsg.Sender.DisplayName != "" {
		sb.WriteString(fmt.Sprintf("- Name: %s\n", inMsg.Sender.DisplayName))
	}
	if inMsg.Sender.Username != "" {
		sb.WriteString(fmt.Sprintf("- Username: @%s\n", inMsg.Sender.Username))
	}

	if lang, ok := inMsg.Context.Raw["language_code"]; ok && lang != "" {
		sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
	}
	_ = os.WriteFile(userFile, []byte(sb.String()), 0o644)
}

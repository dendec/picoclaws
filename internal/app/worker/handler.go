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
	Agent       *agent.AgentLoop
	Channel     *telegram.TelegramChannel
	Bus         *bus.MessageBus
	Bot         *telego.Bot
	MediaStore  media.MediaStore
	Transcriber *transcriber.WhisperTranscriber
	BaseDir     string
	initPyPath  string
}

type HeartbeatMessage struct {
	Type   string `json:"type"`
	ChatID string `json:"chat_id"`
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
		Agent:       agentLoop,
		Channel:     tgChan,
		Bus:         msgBus,
		Bot:         bot,
		MediaStore:  mediaStore,
		Transcriber: tr,
		BaseDir:     baseDir,
		initPyPath:  initPyPath,
	}, nil
}

func (a *WorkerApp) Handle(ctx context.Context, sqsEvent events.SQSEvent) error {
	logger := log.Ctx(ctx).With().Str("component", "tg-worker").Logger()
	ctx = logger.WithContext(ctx)

	for _, record := range sqsEvent.Records {
		if err := a.processSQSRecord(ctx, record); err != nil {
			logger.Error().Str("message_id", record.MessageId).Err(err).Msg("Failed to process SQS record")
		}
	}
	return nil
}

func (a *WorkerApp) processSQSRecord(ctx context.Context, record events.SQSMessage) error {
	// 1. Try to unmarshal as HeartbeatMessage first
	var hb HeartbeatMessage
	if err := json.Unmarshal([]byte(record.Body), &hb); err == nil && hb.Type == "heartbeat" {
		log.Ctx(ctx).Info().Str("chat_id", hb.ChatID).Msg("Processing internal heartbeat message")

		inMsg := &bus.InboundMessage{
			Context: bus.InboundContext{
				Channel: "telegram",
				ChatID:  hb.ChatID,
			},
			ChatID:  hb.ChatID,
			Channel: "telegram",
		}
		return a.processAgentTurn(ctx, hb.ChatID, inMsg, true)
	}

	// 2. Fallback to Telegram Update
	var update telego.Update
	if err := json.Unmarshal([]byte(record.Body), &update); err != nil {
		return fmt.Errorf("unmarshal telegram update: %w", err)
	}

	inMsg, err := tgutil.TranslateUpdate(ctx, a.Bot, update, a.MediaStore, "")
	if err != nil {
		return fmt.Errorf("translate update: %w", err)
	}

	// Automatic Transcription
	if a.Transcriber != nil && len(inMsg.Media) > 0 {
		for _, ref := range inMsg.Media {
			if strings.HasSuffix(strings.ToLower(ref), ".ogg") {
				path, _, _ := a.MediaStore.ResolveWithMeta(ref)
				if res, err := a.Transcriber.Transcribe(ctx, path); err == nil && res.Text != "" {
					log.Ctx(ctx).Info().Str("transcript", res.Text).Msg("Voice transcribed successfully")
					if inMsg.Content == "[voice]" || inMsg.Content == "[empty message]" {
						inMsg.Content = res.Text
					} else {
						inMsg.Content = fmt.Sprintf("%s\n\n[voice: %s]", inMsg.Content, res.Text)
					}
				}
			}
		}
	}

	// Handle special system commands (e.g., /reset)
	a.handleSpecialCommands(ctx, inMsg)

	return a.processAgentTurn(ctx, inMsg.Context.ChatID, inMsg, false)
}

func (a *WorkerApp) processAgentTurn(ctx context.Context, chatID string, inMsg *bus.InboundMessage, isHeartbeat bool) error {
	logger := log.Ctx(ctx).With().Str("chat_id", chatID).Logger()
	ctx = logger.WithContext(ctx)

	bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	var s3Storage *aws.S3Storage
	if bucket != "" {
		var err error
		s3Storage, err = aws.NewS3Storage(ctx, bucket)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to initialize S3 storage")
		}
	}

	chatWorkspace := a.getWorkspacePath(chatID)

	// 1. Prepare Workspace (S3 + Assets)
	isNew, err := a.prepareWorkspace(ctx, s3Storage, chatID, chatWorkspace)
	if err != nil {
		return err
	}

	// 2. Setup isolated Python environment inside workspace
	pythonPackagesDir := filepath.Join(chatWorkspace, ".python_packages")
	_ = os.MkdirAll(pythonPackagesDir, 0755)

	// Set environment variables for the agent and its tools
	os.Setenv("HOME", chatWorkspace)
	os.Setenv("PIP_TARGET", pythonPackagesDir)
	os.Setenv("PIP_NO_CACHE_DIR", "1")

	// Prepend workspace packages to PYTHONPATH
	newPyPath := pythonPackagesDir
	if a.initPyPath != "" {
		newPyPath = pythonPackagesDir + ":" + a.initPyPath
	}
	os.Setenv("PYTHONPATH", newPyPath)

	logger.Debug().
		Str("home", os.Getenv("HOME")).
		Str("pythonpath", os.Getenv("PYTHONPATH")).
		Str("pip_target", os.Getenv("PIP_TARGET")).
		Msg("Isolated environment configured for workspace")

	// 3. Check for heartbeat tasks (AFTER workspace is ready on disk)
	if isHeartbeat {
		inMsg.Content = a.buildHeartbeatPrompt()
	}

	// 4. Create Isolated Agent
	agentInst, err := a.createAgent(chatWorkspace)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	defer agentInst.Close()

	// 5. Initialize USER.md for fresh workspaces
	if isNew {
		a.initUserMetadata(chatWorkspace, inMsg)
	}

	// 6. Materialize inbound media into workspace inbox
	if len(inMsg.Media) > 0 {
		inboxDir := filepath.Join(chatWorkspace, "inbox")
		_ = os.MkdirAll(inboxDir, 0755)
		for _, ref := range inMsg.Media {
			path, meta, err := a.MediaStore.ResolveWithMeta(ref)
			if err != nil {
				logger.Warn().Err(err).Str("ref", ref).Msg("Failed to resolve media ref for inbox")
				continue
			}

			destName := meta.Filename
			if destName == "" {
				destName = filepath.Base(path)
			}
			destPath := filepath.Join(inboxDir, destName)

			// Copy file from media store to inbox
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
				logger.Error().Err(err).Str("src", path).Str("dst", destPath).Msg("Failed to copy media to inbox")
			} else {
				logger.Debug().Str("file", destName).Msg("Media materialized in inbox")
			}
		}
	}

	// 7. Execute Turn
	processErr, modified := a.executeTurn(ctx, agentInst, inMsg, chatWorkspace, isHeartbeat)

	// 8. Finalize Workspace (S3 + Cleanup)
	if modified {
		a.finalizeWorkspace(ctx, s3Storage, chatID, chatWorkspace)
	} else {
		logger.Info().Msg("Workspace unchanged, skipping S3 upload")
	}

	return processErr
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

func (a *WorkerApp) createAgent(workspacePath string) (*agent.AgentInstance, error) {
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
	a.EquipAgent(inst)
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
							if _, err := a.Channel.SendMedia(ctx, msg); err != nil {
								logger.Error().Err(err).Msg("Failed to send media")
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
					if _, err := a.Channel.SendMedia(ctx, msg); err != nil {
						logger.Error().Err(err).Msg("Failed to send media")
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

	_, err := driver.RunAgent(agent.WithTurnContext(processCtx, a.Agent, agentInst), agentInst, processOptions{
		SessionKey:           inMsg.ChatID,
		Channel:              "telegram",
		ChatID:               inMsg.ChatID,
		UserMessage:          inMsg.Content,
		Media:                inMsg.Media,
		DefaultResponse:      "Thinking...",
		EnableSummary:        true,
		SendResponse:         !isHeartbeat,
		SenderID:             inMsg.SenderID,
		SenderDisplayName:    inMsg.Sender.DisplayName,
		SuppressToolFeedback: strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_TOOLS")) != "false",
		SuppressReasoning:    strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_REASONING")) != "false",
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

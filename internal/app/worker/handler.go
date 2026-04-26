package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"picoclaws/internal/pkg/archive"
	"picoclaws/internal/pkg/assets"
	"picoclaws/internal/platform/aws"
	"picoclaws/internal/platform/telegram/tgutil"

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
	Agent      *agent.AgentLoop
	Channel    *telegram.TelegramChannel
	Bus        *bus.MessageBus
	Bot        *telego.Bot
	MediaStore media.MediaStore
	BaseDir    string
	initPyPath string
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

	return &WorkerApp{
		Agent:      agentLoop,
		Channel:    tgChan,
		Bus:        msgBus,
		Bot:        bot,
		MediaStore: mediaStore,
		BaseDir:    baseDir,
		initPyPath: initPyPath,
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

	chatWorkspaceBase, mainWorkspace := a.getWorkspacePaths(chatID)

	// 1. Prepare Workspace (S3 + Assets)
	isNew, err := a.prepareWorkspace(ctx, s3Storage, chatID, chatWorkspaceBase, mainWorkspace)
	if err != nil {
		return err
	}

	// 2. Setup isolated Python environment inside workspace
	pythonPackagesDir := filepath.Join(chatWorkspaceBase, ".python_packages")
	_ = os.MkdirAll(pythonPackagesDir, 0755)

	// Set environment variables for the agent and its tools
	os.Setenv("HOME", chatWorkspaceBase)
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
		prompt := a.buildHeartbeatPrompt(ctx, chatID)
		if prompt == "" {
			logger.Debug().Msg("No heartbeat tasks found, skipping agent execution")
			return nil
		}
		inMsg.Content = prompt
	}

	// 3. Create Isolated Agent
	agentInst, err := a.createAgent(chatWorkspaceBase)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	defer agentInst.Close()

	// 4. Initialize USER.md for fresh workspaces
	if isNew {
		a.initUserMetadata(mainWorkspace, inMsg)
	}

	// 5. Execute Turn
	processErr, modified := a.executeTurn(ctx, agentInst, inMsg, chatWorkspaceBase)

	// 6. Finalize Workspace (S3 + Cleanup)
	if modified {
		a.finalizeWorkspace(ctx, s3Storage, chatID, chatWorkspaceBase)
	} else {
		logger.Info().Msg("Workspace unchanged, skipping S3 upload")
	}

	return processErr
}

func (a *WorkerApp) buildHeartbeatPrompt(ctx context.Context, chatID string) string {
	_, mainWorkspace := a.getWorkspacePaths(chatID)
	heartbeatPath := filepath.Join(mainWorkspace, "HEARTBEAT.md")

	data, err := os.ReadFile(heartbeatPath)
	if err != nil {
		return ""
	}

	content := string(data)
	// Check if there are actual tasks (using the same marker as original picoclaw)
	if !strings.Contains(content, "Add your heartbeat tasks below this line:") {
		return ""
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(`# Heartbeat Check

Current time: %s

You are a proactive AI assistant. This is a scheduled heartbeat check.
Review the following tasks and execute any necessary actions using available skills.
If there is nothing that requires attention, respond ONLY with: HEARTBEAT_OK

%s
`, now, content)
}

// handleSpecialCommands processes incoming messages for specific triggers
// like /reset to nudge the agent behaviorally via system instructions.
func (a *WorkerApp) handleSpecialCommands(ctx context.Context, inMsg *bus.InboundMessage) {
	if strings.TrimSpace(inMsg.Content) == "/reset" {
		inMsg.Content = "System: The user has just reset your memory and workspace. " +
			"Greet them briefly according to your personality."
		log.Ctx(ctx).Info().Msg("Special command /reset processed as system nudge")
	}
}

// getWorkspacePaths calculates the base and main workspace paths for a given chat.
func (a *WorkerApp) getWorkspacePaths(chatID string) (string, string) {
	chatWorkspaceBase := filepath.Join(a.BaseDir, "chats", chatID)
	mainWorkspace := filepath.Join(chatWorkspaceBase, "main")
	return chatWorkspaceBase, mainWorkspace
}

// prepareWorkspace ensures the local environment is ready for the agent to work in.
// It syncs from S3 if needed and restores default assets for fresh workspaces.
// Returns true if the workspace was initialized from scratch (assets restored).
func (a *WorkerApp) prepareWorkspace(ctx context.Context, s3Storage *aws.S3Storage, chatID, chatWorkspaceBase, mainWorkspace string) (bool, error) {
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	localVersionFile := filepath.Join(chatWorkspaceBase, ".version")

	// 1. Check S3 Metadata (ETag/LastModified)
	var s3Meta *aws.S3Metadata
	if s3Storage != nil {
		if meta, err := s3Storage.GetMetadata(ctx, s3Key); err == nil {
			s3Meta = meta
		}
	}

	// 2. Performance: If /tmp folder exists, compare with S3 metadata
	useWarmWorkspace := false
	if _, err := os.Stat(chatWorkspaceBase); err == nil && s3Meta != nil {
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
			if err := archive.Unarchive(data, chatWorkspaceBase); err == nil {
				restoredFromS3 = true
				if s3Meta != nil {
					_ = os.WriteFile(localVersionFile, []byte(s3Meta.ETag), 0o644)
				}
			}
		}
	}

	if !restoredFromS3 {
		logger.Info().Str("chat_id", chatID).Msg("No S3 archive found (or reset), wiping local workspace for clean start")
		_ = os.RemoveAll(chatWorkspaceBase) // Ensure clean state
		if err := os.MkdirAll(chatWorkspaceBase, 0755); err != nil {
			return false, fmt.Errorf("failed to create directory: %w", err)
		}
		if err := assets.RestoreWorkspace(chatWorkspaceBase); err != nil {
			return false, fmt.Errorf("failed to restore embedded workspace: %w", err)
		}
		// Fresh workspace doesn't have a version in S3 yet
		_ = os.Remove(localVersionFile)
	}

	if err := os.MkdirAll(mainWorkspace, 0o755); err != nil {
		return false, fmt.Errorf("failed to create main workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(chatWorkspaceBase, "memory"), 0o755); err != nil {
		return false, fmt.Errorf("failed to create memory directory: %w", err)
	}
	return !restoredFromS3, nil
}

func (a *WorkerApp) createAgent(chatWorkspaceBase string) (*agent.AgentInstance, error) {
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
	requestAgentCfg.Workspace = chatWorkspaceBase

	inst := agent.NewAgentInstance(&requestAgentCfg, &globalConfig.Agents.Defaults, globalConfig, defaultAgent.Provider)
	a.EquipAgent(inst)
	return inst, nil
}

func (a *WorkerApp) executeTurn(ctx context.Context, agentInst *agent.AgentInstance, inMsg *bus.InboundMessage, chatWorkspaceBase string) (error, bool) {
	logger := log.Ctx(ctx)

	// 1. Create a timestamp marker to detect CHANGES in the workspace
	markerFile := filepath.Join(os.TempDir(), fmt.Sprintf("marker_%d", time.Now().UnixNano()))
	_ = os.WriteFile(markerFile, []byte("start"), 0o644)
	defer os.Remove(markerFile)

	// Outbound collection
	outCtx, cancelOut := context.WithCancel(ctx)
	defer cancelOut()

	sendResponse := func(msg bus.OutboundMessage) {
		if _, err := a.Channel.Send(ctx, msg); err != nil {
			logger.Error().Err(err).Str("chat_id", msg.ChatID).Msg("Failed to send response")
		}
	}

	go func() {
		for {
			select {
			case <-outCtx.Done():
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

	_, err := driver.RunAgent(processCtx, agentInst, processOptions{
		SessionKey:           inMsg.ChatID,
		Channel:              "telegram",
		ChatID:               inMsg.ChatID,
		UserMessage:          inMsg.Content,
		Media:                inMsg.Media,
		DefaultResponse:      "Thinking...",
		EnableSummary:        true,
		SendResponse:         true,
		SenderID:             inMsg.SenderID,
		SenderDisplayName:    inMsg.Sender.DisplayName,
		SuppressToolFeedback: strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_TOOLS")) != "false",
		SuppressReasoning:    strings.ToLower(os.Getenv("PICOCLAW_SUPPRESS_REASONING")) != "false",
	})

	time.Sleep(500 * time.Millisecond) // Drain bus

	// 2. SMART DETECTOR: Check if workspace was modified
	modified := true // Default to true for safety
	cmd := exec.CommandContext(ctx, "find", chatWorkspaceBase, "-newer", markerFile)
	output, findErr := cmd.Output()
	if findErr == nil {
		// If output is empty, nothing was changed
		if len(strings.TrimSpace(string(output))) == 0 {
			modified = false
		}
	}

	return err, modified
}

func (a *WorkerApp) finalizeWorkspace(ctx context.Context, s3Storage *aws.S3Storage, chatID, chatWorkspaceBase string) {
	if s3Storage == nil {
		return
	}
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	if archiveData, err := archive.Archive(chatWorkspaceBase); err == nil {
		if err := s3Storage.Upload(ctx, s3Key, archiveData); err != nil {
			logger.Warn().Err(err).Str("key", s3Key).Msg("Failed to upload workspace to S3")
		} else {
			// Update local .version with new S3 state to ensure next warm hit knows we are fresh
			if meta, err := s3Storage.GetMetadata(ctx, s3Key); err == nil && meta != nil {
				localVersionFile := filepath.Join(chatWorkspaceBase, ".version")
				_ = os.WriteFile(localVersionFile, []byte(meta.ETag), 0o644)
			}
		}
	}
	// We DO NOT RemoveAll here anymore to persist /tmp for the next warm invocation.
}

func (a *WorkerApp) initUserMetadata(mainWorkspace string, inMsg *bus.InboundMessage) {
	if inMsg == nil || inMsg.Sender.DisplayName == "" && inMsg.Sender.Username == "" {
		return // No sender info available (e.g. heartbeat)
	}
	userFile := filepath.Join(mainWorkspace, "USER.md")

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

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
	pcfg "github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
	plog "github.com/sipeed/picoclaw/pkg/logger"
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
}

func NewWorkerApp(ctx context.Context) (*WorkerApp, error) {
	// Prepend bundled binaries to PATH so tools (like exec) can find BusyBox applets
	currentPath := os.Getenv("PATH")
	assetsBin, _ := filepath.Abs("assets/bin")
	if _, err := os.Stat(assetsBin); err == nil {
		os.Setenv("PATH", assetsBin+":"+currentPath)
		log.Ctx(ctx).Debug().Str("path", os.Getenv("PATH")).Msg("Updated PATH with bundled binaries")
	}

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

	cfg, err := pcfg.LoadConfig(configPath)
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

	bot, err := telego.NewBot(cfg.Channels.Telegram.Token())
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	// 1. Configure picoclaw logger
	plog.SetLevelFromString(os.Getenv("LOG_LEVEL"))
	if os.Getenv("PICOCLAW_LOG_FILE") != "" {
		plog.ConfigureFromEnv()
	} else if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		// In Lambda, use JSON output for CloudWatch compatibility
		plog.UseJSONOutput()
	}

	tgChan, err := telegram.NewTelegramChannel(cfg, msgBus)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram channel: %w", err)
	}
	tgChan.SetRunning(true)

	mediaStore := media.NewFileMediaStore()

	agentLoop := agent.NewAgentLoop(cfg, msgBus, provider)
	agentLoop.SetMediaStore(mediaStore)

	return &WorkerApp{
		Agent:      agentLoop,
		Channel:    tgChan,
		Bus:        msgBus,
		Bot:        bot,
		MediaStore: mediaStore,
		BaseDir:    baseDir,
	}, nil
}

func (a *WorkerApp) Handle(ctx context.Context, sqsEvent events.SQSEvent) error {
	logger := log.Ctx(ctx).With().Str("component", "tg-worker").Logger()
	ctx = logger.WithContext(ctx)

	bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	var s3Sync *aws.S3Sync
	if bucket != "" {
		var err error
		s3Sync, err = aws.NewS3Sync(ctx, bucket)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to initialize S3 sync")
		}
	}

	for _, record := range sqsEvent.Records {
		if err := a.processRecord(ctx, record, s3Sync); err != nil {
			logger.Error().
				Str("message_id", record.MessageId).
				Err(err).
				Msg("Failed to process SQS record")
		}
	}
	return nil
}

func (a *WorkerApp) processRecord(ctx context.Context, record events.SQSMessage, s3Sync *aws.S3Sync) error {
	var update telego.Update
	if err := json.Unmarshal([]byte(record.Body), &update); err != nil {
		return fmt.Errorf("unmarshal telegram update: %w", err)
	}

	chatID := tgutil.ExtractChatID(update)
	chatWorkspaceBase, mainWorkspace := a.getWorkspacePaths(chatID)

	// 1. Prepare Workspace (S3 + Assets)
	if err := a.prepareWorkspace(ctx, s3Sync, chatID, chatWorkspaceBase, mainWorkspace); err != nil {
		return err
	}

	// 2. Create Isolated Agent
	agentInst, err := a.createAgent(chatWorkspaceBase)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	defer agentInst.Close()

	// 3. Translate Update into agent-readable message
	inMsg, err := tgutil.TranslateUpdate(ctx, a.Bot, update, a.MediaStore, "")
	if err != nil {
		return fmt.Errorf("translate update: %w", err)
	}

	// 3.1. Handle special system commands (e.g., /reset)
	a.handleSpecialCommands(ctx, inMsg)

	// 4. Execute Turn
	processErr, modified := a.executeTurn(ctx, agentInst, inMsg, chatWorkspaceBase)

	// 5. Finalize Workspace (S3 + Cleanup)
	if modified {
		a.finalizeWorkspace(ctx, s3Sync, chatID, chatWorkspaceBase)
	} else {
		log.Ctx(ctx).Info().Str("chat_id", chatID).Msg("Workspace unchanged, skipping S3 upload")
	}

	return processErr
}

// handleSpecialCommands processes incoming messages for specific triggers
// like /reset to nudge the agent behaviorally via system instructions.
func (a *WorkerApp) handleSpecialCommands(ctx context.Context, inMsg *bus.InboundMessage) {
	if strings.TrimSpace(inMsg.Content) == "/reset" {
		lang := inMsg.Metadata["language_code"]
		if lang == "" {
			lang = "en" // fallback
		}
		inMsg.Content = fmt.Sprintf("System: The user has just reset your memory and workspace. "+
			"Greet them briefly in your characteristic witty style and confirm that you are ready to start fresh from a clean slate. "+
			"Respond in the user's language (language_code: %s).", lang)
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
func (a *WorkerApp) prepareWorkspace(ctx context.Context, s3Sync *aws.S3Sync, chatID, chatWorkspaceBase, mainWorkspace string) error {
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	localVersionFile := filepath.Join(chatWorkspaceBase, ".version")

	// 1. Check S3 Metadata (ETag/LastModified)
	var s3Meta *aws.S3Metadata
	if s3Sync != nil {
		if meta, err := s3Sync.GetMetadata(ctx, s3Key); err == nil {
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
	if !useWarmWorkspace && s3Sync != nil {
		data, err := s3Sync.Download(ctx, s3Key)
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
			return fmt.Errorf("failed to create directory: %w", err)
		}
		if err := assets.RestoreWorkspace(chatWorkspaceBase); err != nil {
			return fmt.Errorf("failed to restore embedded workspace: %w", err)
		}
		// Fresh workspace doesn't have a version in S3 yet
		_ = os.Remove(localVersionFile)
	}

	if err := os.MkdirAll(mainWorkspace, 0o755); err != nil {
		return fmt.Errorf("failed to create main workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(chatWorkspaceBase, "memory"), 0o755); err != nil {
		return fmt.Errorf("failed to create memory directory: %w", err)
	}
	return nil
}

func (a *WorkerApp) createAgent(chatWorkspaceBase string) (*agent.AgentInstance, error) {
	globalConfig := a.Agent.GetConfig()
	defaultAgent := a.Agent.GetRegistry().GetDefaultAgent()
	if defaultAgent == nil {
		return nil, fmt.Errorf("default agent not found in registry")
	}

	agentID := "main"
	var agentCfg *pcfg.AgentConfig
	for i := range globalConfig.Agents.List {
		if globalConfig.Agents.List[i].ID == agentID {
			agentCfg = &globalConfig.Agents.List[i]
			break
		}
	}

	if agentCfg == nil {
		agentCfg = &pcfg.AgentConfig{ID: agentID, Default: true}
	}

	requestAgentCfg := *agentCfg
	requestAgentCfg.Workspace = chatWorkspaceBase

	return agent.NewAgentInstance(&requestAgentCfg, &globalConfig.Agents.Defaults, globalConfig, defaultAgent.Provider), nil
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
		if err := a.Channel.Send(ctx, msg); err != nil {
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
					if err := a.Channel.SendMedia(ctx, msg); err != nil {
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
	if t, err := time.ParseDuration(os.Getenv("WORKER_PROCESS_TIMEOUT")); err == nil {
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

func (a *WorkerApp) finalizeWorkspace(ctx context.Context, s3Sync *aws.S3Sync, chatID, chatWorkspaceBase string) {
	if s3Sync == nil {
		return
	}
	logger := log.Ctx(ctx)
	s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)

	if archiveData, err := archive.Archive(chatWorkspaceBase); err == nil {
		if err := s3Sync.Upload(ctx, s3Key, archiveData); err != nil {
			logger.Warn().Err(err).Str("key", s3Key).Msg("Failed to upload workspace to S3")
		} else {
			// Update local .version with new S3 state to ensure next warm hit knows we are fresh
			if meta, err := s3Sync.GetMetadata(ctx, s3Key); err == nil && meta != nil {
				localVersionFile := filepath.Join(chatWorkspaceBase, ".version")
				_ = os.WriteFile(localVersionFile, []byte(meta.ETag), 0o644)
			}
		}
	}
	// We DO NOT RemoveAll here anymore to persist /tmp for the next warm invocation.
}

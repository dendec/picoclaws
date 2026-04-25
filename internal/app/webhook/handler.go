package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mymmrac/telego"
	"github.com/rs/zerolog/log"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"picoclaws/internal/platform/aws"
	"picoclaws/internal/platform/telegram"
	"picoclaws/internal/platform/telegram/tgutil"
)

// WebhookApp holds dependencies for the Lambda webhook handler.
type WebhookApp struct {
	Bot       *telego.Bot
	SQS       *aws.SQSPublisher
	QueueURL  string
	Validator IPValidator
	AutoSetup bool
	S3Storage *aws.S3Storage
	S3Bucket  string
	setupOnce sync.Once
}

func NewWebhookApp(ctx context.Context) (*WebhookApp, error) {

	queueURL := os.Getenv("QUEUE_URL")
	if queueURL == "" {
		return nil, fmt.Errorf("QUEUE_URL is required")
	}
	autoSetup := strings.ToLower(os.Getenv("AUTO_SETUP")) == "true"

	configPath := os.Getenv("PICOCLAW_CONFIG")
	if configPath == "" {
		if _, err := os.Stat("config.json"); err == nil {
			configPath = "config.json"
		} else {
			// Fallback to system default
			configPath = "/tmp/picoclaw/config.json"
		}
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	tgChan := cfg.Channels.GetByType(config.ChannelTelegram)
	if tgChan == nil {
		return nil, fmt.Errorf("telegram channel not configured")
	}
	tgDecoded, err := tgChan.GetDecoded()
	if err != nil {
		return nil, fmt.Errorf("failed to decode telegram settings: %w", err)
	}
	tgSettings, ok := tgDecoded.(*config.TelegramSettings)
	if !ok {
		return nil, fmt.Errorf("invalid telegram settings type")
	}

	bot, err := telego.NewBot(tgSettings.Token.String())
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w (check token in %s)", err, configPath)
	}

	// 1. Configure picoclaw logger
	logger.SetLevelFromString(os.Getenv("LOG_LEVEL"))
	if os.Getenv("PICOCLAW_LOG_FILE") != "" {
		logger.ConfigureFromEnv()
	}


	validator, err := telegram.NewIPValidator()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IP validator: %w", err)
	}

	s3Bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	if s3Bucket == "" {
		return nil, fmt.Errorf("PICOCLAW_WORKSPACE_BUCKET is required")
	}
	s3Storage, err := aws.NewS3Storage(ctx, s3Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to init S3 storage: %w", err)
	}

	sqsHandler, err := aws.NewSQSPublisher(ctx, queueURL)
	if err != nil {
		return nil, fmt.Errorf("failed to init SQS publisher: %w", err)
	}

	return &WebhookApp{
		Bot:       bot,
		SQS:       sqsHandler,
		QueueURL:  queueURL,
		Validator: validator,
		AutoSetup: autoSetup,
		S3Storage: s3Storage,
		S3Bucket:  s3Bucket,
	}, nil
}

// IPValidator is an interface for validating incoming IP addresses.
type IPValidator interface {
	ValidateRequest(event events.LambdaFunctionURLRequest) (bool, events.LambdaFunctionURLResponse)
}

// Handle processes the incoming Lambda URL request.
func (a *WebhookApp) Handle(ctx context.Context, event events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	logger := log.Ctx(ctx).With().
		Str("remote_ip", event.RequestContext.HTTP.SourceIP).
		Logger()
	ctx = logger.WithContext(ctx)

	log.Info().
		Str("domain", event.RequestContext.DomainName).
		Bool("auto_setup", a.AutoSetup).
		Msg("Handling webhook request")

	if a.AutoSetup {
		a.setupOnce.Do(func() {
			webhookURL := "https://" + event.RequestContext.DomainName + "/"
			log.Info().
				Str("url", webhookURL).
				Msg("Triggering dynamic bot setup")
			if err := a.EnsureSetup(ctx, webhookURL); err != nil {
				log.Error().Err(err).Msg("Failed to run dynamic bot setup")
			}
		})
	}

	if a.Validator != nil {
		if ok, resp := a.Validator.ValidateRequest(event); !ok {
			logger.Warn().Msg("Request rejected by validator")
			return resp, nil
		}
	}

	var update telego.Update
	if err := json.Unmarshal([]byte(event.Body), &update); err != nil {
		logger.Error().Err(err).Msg("Failed to unmarshal telegram update")
		return events.LambdaFunctionURLResponse{StatusCode: 400}, nil
	}

	if err := a.ProcessUpdate(ctx, update); err != nil {
		return events.LambdaFunctionURLResponse{StatusCode: 500}, nil
	}

	return events.LambdaFunctionURLResponse{StatusCode: 200}, nil
}

// handleCommand checks for bot commands and performs immediate actions.
// Returns true if the update was handled as a command.
func (a *WebhookApp) handleCommand(ctx context.Context, update telego.Update) (bool, error) {
	if update.Message == nil {
		return false, nil
	}

	text := strings.TrimSpace(update.Message.Text)
	if !strings.HasPrefix(text, "/") {
		return false, nil
	}

	logger := log.Ctx(ctx)
	cmd := strings.Split(text, " ")[0]

	switch cmd {
	case "/reset":
		chatID := tgutil.ExtractChatID(update)
		s3Key := fmt.Sprintf("workspaces/%s.tar.zst", chatID)
		logger.Info().Str("chat_id", chatID).Str("key", s3Key).Msg("Executing /reset command")

		if err := a.S3Storage.Delete(ctx, s3Key); err != nil {
			logger.Error().Err(err).Msg("Failed to delete workspace from S3")
		}
		// We return true but don't send a message yet; the worker will handle the response
		return true, nil
	}

	return false, nil
}

func (a *WebhookApp) ProcessUpdate(ctx context.Context, update telego.Update) error {
	logger := log.Ctx(ctx).With().Int("update_id", update.UpdateID).Logger()

	// Handle commands first
	handled, err := a.handleCommand(ctx, update)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to handle command")
	}
	if handled {
		logger.Info().Msg("Command handled, continuing to SQS for agent response")
	}

	body, _ := json.Marshal(update)
	chatID := tgutil.ExtractChatID(update)

	if err := a.SQS.SendMessage(ctx, string(body), chatID); err != nil {
		logger.Error().Err(err).Msg("Failed to send message to SQS")
		return err
	}

	logger.Info().Str("chat_id", chatID).Msg("Sent update to SQS")
	return nil
}

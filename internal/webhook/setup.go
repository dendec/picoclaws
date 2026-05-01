package webhook

import (
	"context"
	"errors"

	"github.com/mymmrac/telego"
	"github.com/rs/zerolog/log"
)

// InitEnvironment ensures all local environment requirements are met.
func (a *WebhookApp) InitEnvironment() {
	log.Info().Msg("Webhook environment initialized")
}

// EnsureSetup checks if the bot setup (webhook, commands, name) needs update.
func (a *WebhookApp) EnsureSetup(ctx context.Context, webhookURL string) error {
	log.Info().
		Str("webhook", webhookURL).
		Msg("Running bot setup...")

	if err := a.setupWebhook(ctx, webhookURL); err != nil {
		log.Error().Err(err).Msg("Failed to set webhook")
		return err
	}

	if err := a.setupBotProfile(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to set bot profile")
	}

	return nil
}

func (a *WebhookApp) setupWebhook(ctx context.Context, url string) error {
	if url == "" {
		return errors.New("webhook URL is empty")
	}

	log.Info().Msg("setupWebhook: fetching current webhook info")
	info, err := a.Bot.GetWebhookInfo(ctx)
	if err != nil {
		log.Error().Err(err).Msg("setupWebhook: GetWebhookInfo failed")
		return err
	}
	log.Info().Str("current_url", info.URL).Msg("setupWebhook: current info received")

	if info.URL == url {
		log.Info().Msg("setupWebhook: URL already matches, skipping")
		return nil
	}

	log.Info().Str("new_url", url).Msg("setupWebhook: setting new webhook")
	return a.Bot.SetWebhook(ctx, &telego.SetWebhookParams{
		URL: url,
	})
}

func (a *WebhookApp) setupBotProfile(ctx context.Context) error {
	log.Info().Msg("setupBotProfile: setting commands")
	// Set default commands
	commands := []telego.BotCommand{
		{Command: "start", Description: "Start the agent"},
		{Command: "reset", Description: "Clear conversation history"},
		{Command: "help", Description: "Show help"},
	}

	err := a.Bot.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: commands,
	})
	if err != nil {
		log.Error().Err(err).Msg("setupBotProfile: SetMyCommands failed")
		return err
	}
	log.Info().Msg("setupBotProfile: commands set successfully")

	return nil
}

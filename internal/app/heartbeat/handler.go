package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"picoclaws/internal/platform/aws"
	"github.com/aws/aws-lambda-go/events"
	"github.com/rs/zerolog/log"
)

type HeartbeatApp struct {
	S3  *aws.S3Storage
	SQS *aws.SQSPublisher
}

type HeartbeatMessage struct {
	Type   string `json:"type"`
	ChatID string `json:"chat_id"`
}

func NewHeartbeatApp(ctx context.Context) (*HeartbeatApp, error) {
	bucket := os.Getenv("PICOCLAW_WORKSPACE_BUCKET")
	if bucket == "" {
		return nil, fmt.Errorf("PICOCLAW_WORKSPACE_BUCKET not set")
	}

	queueURL := os.Getenv("QUEUE_URL")
	if queueURL == "" {
		return nil, fmt.Errorf("QUEUE_URL not set")
	}

	s3Storage, err := aws.NewS3Storage(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to init S3 sync: %w", err)
	}

	sqsHandler, err := aws.NewSQSPublisher(ctx, queueURL)
	if err != nil {
		return nil, fmt.Errorf("failed to init SQS publisher: %w", err)
	}

	return &HeartbeatApp{
		S3:  s3Storage,
		SQS: sqsHandler,
	}, nil
}

func (a *HeartbeatApp) Handle(ctx context.Context, event events.CloudWatchEvent) error {
	logger := log.Ctx(ctx).With().Str("component", "heartbeat-dispatcher").Logger()

	// 1. Discover all active workspaces
	keys, err := a.S3.ListKeys(ctx, "workspaces/")
	if err != nil {
		return fmt.Errorf("failed to list workspaces: %w", err)
	}

	logger.Info().Int("count", len(keys)).Msg("Discovered workspaces for heartbeat fan-out")

	for _, key := range keys {
		// Key format: workspaces/<chatID>.tar.zst
		base := filepath.Base(key)
		chatID := strings.TrimSuffix(base, ".tar.zst")
		if chatID == "" || chatID == "." {
			continue
		}

		// 2. Send individual heartbeat message to SQS
		hb := HeartbeatMessage{Type: "heartbeat", ChatID: chatID}
		body, _ := json.Marshal(hb)

		if err := a.SQS.SendMessage(ctx, string(body), chatID); err != nil {
			logger.Warn().Str("chat_id", chatID).Err(err).Msg("Failed to fan-out heartbeat")
		} else {
			logger.Debug().Str("chat_id", chatID).Msg("Heartbeat task sent to SQS")
		}
	}

	return nil
}

package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"time"

	"picoclaws/internal/platform/aws"
	"github.com/aws/aws-lambda-go/events"
	"github.com/rs/zerolog/log"
	"sync"
	"sync/atomic"
)

const (
	// InactivityThreshold defines how long a workspace can remain untouched before we skip heartbeats.
	InactivityThreshold = 2 * time.Hour
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
	metadata, err := a.S3.ListObjects(ctx, "workspaces/")
	if err != nil {
		return fmt.Errorf("failed to list workspaces: %w", err)
	}

	logger.Info().Int("total", len(metadata)).Msg("Discovered workspaces for heartbeat analysis")

	var skipped int64
	var sent int64
	
	const maxConcurrency = 10
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for _, meta := range metadata {
		// Key format: workspaces/<chatID>.tar.zst
		base := filepath.Base(meta.Key)
		chatID := strings.TrimSuffix(base, ".tar.zst")
		if chatID == "" || chatID == "." {
			continue
		}

		// Optimization: skip workspaces that haven't been modified for a while.
		if time.Since(time.Unix(meta.LastModified, 0)) > InactivityThreshold {
			atomic.AddInt64(&skipped, 1)
			continue
		}

		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			
			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			// 2. Send individual heartbeat message to SQS
			hb := HeartbeatMessage{Type: "heartbeat", ChatID: id}
			body, _ := json.Marshal(hb)

			if err := a.SQS.SendMessage(ctx, string(body), id); err != nil {
				logger.Warn().Str("chat_id", id).Err(err).Msg("Failed to fan-out heartbeat")
			} else {
				atomic.AddInt64(&sent, 1)
				logger.Debug().Str("chat_id", id).Msg("Heartbeat task sent to SQS")
			}
		}(chatID)
	}

	wg.Wait()

	logger.Info().Int64("sent", sent).Int64("skipped", skipped).Msg("Heartbeat fan-out complete")

	return nil
}

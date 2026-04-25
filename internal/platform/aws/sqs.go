package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// SQSPublisher handles sending messages to SQS.
type SQSPublisher struct {
	client *sqs.Client
	url    string
}

// NewSQSPublisher creates a new SQSPublisher.
func NewSQSPublisher(ctx context.Context, queueURL string) (*SQSPublisher, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &SQSPublisher{
		client: sqs.NewFromConfig(cfg),
		url:    queueURL,
	}, nil
}

// SendMessage sends a JSON string as a message body to the SQS queue.
func (s *SQSPublisher) SendMessage(ctx context.Context, body string, messageGroupID string) error {
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.url),
		MessageBody: aws.String(body),
	}

	// For FIFO queues
	if messageGroupID != "" {
		input.MessageGroupId = aws.String(messageGroupID)
	}

	_, err := s.client.SendMessage(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to send SQS message: %w", err)
	}
	return nil
}

package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rs/zerolog/log"
)

// S3Sync handles uploading and downloading archives to/from S3.
type S3Sync struct {
	client *s3.Client
	bucket string
}

// NewS3Sync creates a new S3Sync helper.
func NewS3Sync(ctx context.Context, bucket string) (*S3Sync, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &S3Sync{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
	}, nil
}

// Download fetches the archive from S3.
// Returns nil if the object doesn't exist (fresh workspace).
func (s *S3Sync) Download(ctx context.Context, key string) ([]byte, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		if IsNoSuchKey(err) {
			log.Ctx(ctx).Info().Str("key", key).Msg("No existing workspace found in S3")
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get S3 object: %w", err)
	}
	defer output.Body.Close()

	return io.ReadAll(output.Body)
}

// S3Metadata contains basic info about an S3 object.
type S3Metadata struct {
	ETag         string
	LastModified int64
}

// GetMetadata fetches only the metadata (HeadObject) from S3.
func (s *S3Sync) GetMetadata(ctx context.Context, key string) (*S3Metadata, error) {
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if IsNoSuchKey(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to head S3 object: %w", err)
	}

	lm := int64(0)
	if output.LastModified != nil {
		lm = output.LastModified.Unix()
	}

	return &S3Metadata{
		ETag:         aws.ToString(output.ETag),
		LastModified: lm,
	}, nil
}

// Upload puts the archive data to S3.
func (s *S3Sync) Upload(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}
	return nil
}

// Delete removes the object from S3.
func (s *S3Sync) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete S3 object: %w", err)
	}
	return nil
}

// IsNoSuchKey checks if an error is an AWS S3 NoSuchKey or NotFound error.
func IsNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	return false
}

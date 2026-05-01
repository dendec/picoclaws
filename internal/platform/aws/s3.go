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

// S3Storage handles uploading and downloading archives to/from S3.
type S3Storage struct {
	client *s3.Client
	bucket string
}

// NewS3Storage creates a new S3Storage helper.
func NewS3Storage(ctx context.Context, bucket string) (*S3Storage, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &S3Storage{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
	}, nil
}

// Download fetches the archive from S3.
// Returns nil if the object doesn't exist (fresh workspace).
func (s *S3Storage) Download(ctx context.Context, key string) ([]byte, error) {
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
	Key          string
	ETag         string
	LastModified int64
}

// GetMetadata fetches only the metadata (HeadObject) from S3.
func (s *S3Storage) GetMetadata(ctx context.Context, key string) (*S3Metadata, error) {
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
		Key:          key,
		ETag:         aws.ToString(output.ETag),
		LastModified: lm,
	}, nil
}

// Upload puts the archive data to S3.
func (s *S3Storage) Upload(ctx context.Context, key string, data []byte) error {
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
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete S3 object: %w", err)
	}
	return nil
}

// ListObjects returns all object metadata starting with the given prefix.
func (s *S3Storage) ListObjects(ctx context.Context, prefix string) ([]S3Metadata, error) {
	var metadata []S3Metadata
	var continuationToken *string

	for {
		output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list S3 objects: %w", err)
		}

		for _, obj := range output.Contents {
			lm := int64(0)
			if obj.LastModified != nil {
				lm = obj.LastModified.Unix()
			}
			metadata = append(metadata, S3Metadata{
				Key:          aws.ToString(obj.Key),
				ETag:         aws.ToString(obj.ETag),
				LastModified: lm,
			})
		}

		if !aws.ToBool(output.IsTruncated) {
			break
		}
		continuationToken = output.NextContinuationToken
	}

	return metadata, nil
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

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Store is the S3-compatible (MinIO) adapter for Store.
type S3Store struct {
	client  *s3.Client
	presign *s3.PresignClient
}

// New builds an S3Store. When cfg.Endpoint is set the client targets that
// endpoint with path-style addressing (MinIO). When cfg.AccessKeyID is set,
// static credentials are used; otherwise the default AWS credential chain is
// left in place.
func New(cfg Config) (*S3Store, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.Region = region
			o.UsePathStyle = cfg.UsePathStyle || cfg.Endpoint != ""
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
			if cfg.AccessKeyID != "" {
				o.Credentials = credentials.NewStaticCredentialsProvider(
					cfg.AccessKeyID, cfg.SecretAccessKey, "")
			}
		},
	}
	client := s3.New(s3.Options{}, opts...)
	return &S3Store{
		client:  client,
		presign: s3.NewPresignClient(client),
	}, nil
}

var _ Store = (*S3Store)(nil)

func (s *S3Store) GetObject(ctx context.Context, bucket, key string) (*Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s/%s: %w", bucket, key, err)
	}
	obj := &Object{Body: out.Body, ContentLength: -1}
	if out.ContentType != nil {
		obj.ContentType = *out.ContentType
	}
	if out.ContentLength != nil {
		obj.ContentLength = *out.ContentLength
	}
	if out.ETag != nil {
		obj.ETag = *out.ETag
	}
	return obj, nil
}

// GetBytes fetches an entire object into memory.
func (s *S3Store) GetBytes(ctx context.Context, bucket, key string) ([]byte, error) {
	obj, err := s.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

func (s *S3Store) PutObject(ctx context.Context, bucket, key string, body io.Reader, contentType string) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3 put %s/%s: %w", bucket, key, err)
	}
	return nil
}

func (s *S3Store) PutBytes(ctx context.Context, bucket, key string, data []byte) error {
	return s.PutObject(ctx, bucket, key, bytes.NewReader(data), "")
}

func (s *S3Store) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3 delete %s/%s: %w", bucket, key, err)
	}
	return nil
}

// DeleteObjects mirrors the Rust S3Client::bulk_hard_delete_objects: chunked
// DeleteObjects calls (1000 keys/request) returning one result per key.
func (s *S3Store) DeleteObjects(ctx context.Context, bucket string, keys []string) []error {
	if len(keys) == 0 {
		return nil
	}
	const chunkSize = 1000
	results := make([]error, 0, len(keys))
	for start := 0; start < len(keys); start += chunkSize {
		end := min(start+chunkSize, len(keys))
		chunk := keys[start:end]

		ids := make([]s3types.ObjectIdentifier, len(chunk))
		for i, k := range chunk {
			ids[i] = s3types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: ids},
		})
		if err != nil {
			for range chunk {
				results = append(results, fmt.Errorf("batch delete failed: %w", err))
			}
			continue
		}
		errs := map[string]string{}
		for _, e := range out.Errors {
			if e.Key != nil {
				msg := "unknown error"
				if e.Message != nil {
					msg = *e.Message
				}
				errs[*e.Key] = msg
			}
		}
		for _, k := range chunk {
			if msg, bad := errs[k]; bad {
				results = append(results, fmt.Errorf("failed to delete %s: %s", k, msg))
			} else {
				results = append(results, nil)
			}
		}
	}
	return results
}

func (s *S3Store) DeleteByPrefix(ctx context.Context, bucket, prefix string) error {
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("s3 list %s/%s: %w", bucket, prefix, err)
		}
		keys := make([]string, 0, len(out.Contents))
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if len(keys) > 0 {
			for _, derr := range s.DeleteObjects(ctx, bucket, keys) {
				if derr != nil {
					slog.Warn("objectstore: prefix delete object failed", "err", derr)
				}
			}
		}
		if out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
}

func (s *S3Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NotFound", "NoSuchKey", "404":
				return false, nil
			}
		}
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, fmt.Errorf("s3 head %s/%s: %w", bucket, key, err)
	}
	return true, nil
}

func (s *S3Store) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3 presign get %s/%s: %w", bucket, key, err)
	}
	return out.URL, nil
}

func (s *S3Store) PresignPut(ctx context.Context, bucket, key, contentType string, ttl time.Duration) (string, error) {
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	out, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3 presign put %s/%s: %w", bucket, key, err)
	}
	return out.URL, nil
}

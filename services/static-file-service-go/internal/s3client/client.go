// Package s3client mirrors service/s3/client.rs: presigned PUT (2 minutes)
// and GET (1 hour) URLs with the LocalStack URL transform, single/bulk
// deletes, and prefix-based deletion of transformed variants.
package s3client

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Client mirrors S3Client.
type Client struct {
	inner     s3API
	presigner *s3.PresignClient
	bucket    string
	local     localAWS
}

type localAWS struct {
	enabled   bool
	publicURL string // LOCAL_AWS_PUBLIC_URL, may be empty
}

func New(awsCfg aws.Config, bucket, localAWSURL, localAWSPublicURL string) *Client {
	opts := func(o *s3.Options) {
		o.UsePathStyle = localAWSURL != "" // force_path_style(is_local_aws())
	}
	return &Client{
		inner:     s3.NewFromConfig(awsCfg, opts),
		presigner: s3.NewPresignClient(s3.NewFromConfig(awsCfg, opts)),
		bucket:    bucket,
		local:     localAWS{enabled: localAWSURL != "", publicURL: localAWSPublicURL},
	}
}

// transformAWSURL mirrors macro_aws_config::transform_aws_url /
// transform_local_url: LocalStack presigned URLs are rewritten to the
// browser-facing origin.
func (c *Client) transformAWSURL(rawURL string) string {
	if !c.local.enabled {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "4566"
	}

	origin := strings.TrimSuffix(c.local.publicURL, "/")
	if origin == "" {
		origin = "http://localhost:" + port
	}

	if host == "localstack" || host == "localhost" {
		parsed.Scheme, parsed.Host = "", ""
		return origin + parsed.String()
	}
	// hostname should be in the form {asset}.localstack or {asset}.localhost
	asset := strings.TrimSuffix(host, ".localstack")
	if asset == host {
		asset = strings.TrimSuffix(host, ".localhost")
	}
	if asset == host {
		return rawURL
	}
	parsed.Scheme, parsed.Host = "", ""
	return origin + "/" + asset + parsed.String()
}

// PutPresignedURL mirrors put_presigned_url (2 minute expiry).
func (c *Client) PutPresignedURL(ctx context.Context, key, contentType string) (string, error) {
	out, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(2*time.Minute))
	if err != nil {
		return "", fmt.Errorf("failed to create presigned url: %w", err)
	}
	return c.transformAWSURL(out.URL), nil
}

// GetPresignedURL mirrors get_presigned_url (1 hour expiry).
func (c *Client) GetPresignedURL(ctx context.Context, key string) (string, error) {
	out, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(time.Hour))
	if err != nil {
		return "", fmt.Errorf("failed to create presigned URL: %w", err)
	}
	return c.transformAWSURL(out.URL), nil
}

// HardDeleteObject mirrors hard_delete_object.
func (c *Client) HardDeleteObject(ctx context.Context, key string) error {
	_, err := c.inner.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}
	return nil
}

// DeleteObjectsByPrefix mirrors delete_objects_by_prefix: paginate
// ListObjectsV2 and bulk-delete every listed key.
func (c *Client) DeleteObjectsByPrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		out, err := c.inner.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(c.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("failed to list objects: %w", err)
		}
		keys := make([]string, 0, len(out.Contents))
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if len(keys) > 0 {
			c.BulkHardDeleteObjects(ctx, keys)
		}
		if out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// BulkHardDeleteObjects mirrors bulk_hard_delete_objects: DeleteObjects in
// chunks of 1000; the result slice lines up index-for-index with keys.
func (c *Client) BulkHardDeleteObjects(ctx context.Context, keys []string) []error {
	if len(keys) == 0 {
		return nil
	}
	const chunkSize = 1000 // S3 DeleteObjects limit
	results := make([]error, 0, len(keys))
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		ids := make([]types.ObjectIdentifier, 0, len(chunk))
		for _, k := range chunk {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := c.inner.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids},
		})
		if err != nil {
			for range chunk {
				results = append(results, fmt.Errorf("batch delete failed: %w", err))
			}
			continue
		}
		errByKey := make(map[string]string, len(out.Errors))
		for _, e := range out.Errors {
			msg := "unknown error"
			if e.Message != nil {
				msg = *e.Message
			}
			if e.Key != nil {
				errByKey[*e.Key] = msg
			}
		}
		for _, k := range chunk {
			if msg, ok := errByKey[k]; ok {
				results = append(results, fmt.Errorf("failed to delete %s: %s", k, msg))
			} else {
				results = append(results, nil)
			}
		}
	}
	return results
}

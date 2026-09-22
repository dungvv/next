// Package events mirrors api/event: the SQS long-poll loop that receives S3
// event notifications and marks file metadata uploaded.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/dungvv/next/services/static-file-service-go/internal/s3key"
)

// S3EventNotification mirrors message::S3EventNotification.
type S3EventNotification struct {
	Records []S3EventRecord `json:"Records"`
}

type S3EventRecord struct {
	EventVersion string `json:"eventVersion"`
	EventSource  string `json:"eventSource"`
	AWSRegion    string `json:"awsRegion"`
	EventTime    string `json:"eventTime"`
	EventName    string `json:"eventName"`
	UserIdentity struct {
		PrincipalID string `json:"principalId"`
	} `json:"userIdentity"`
	RequestParameters struct {
		SourceIPAddress string `json:"sourceIPAddress"`
	} `json:"requestParameters"`
	ResponseElements struct {
		RequestID string `json:"x-amz-request-id"`
		ID2       string `json:"x-amz-id-2"`
	} `json:"responseElements"`
	S3 struct {
		SchemaVersion   string `json:"s3SchemaVersion"`
		ConfigurationID string `json:"configurationId"`
		Bucket          struct {
			Name          string `json:"name"`
			OwnerIdentity struct {
				PrincipalID string `json:"principalId"`
			} `json:"ownerIdentity"`
			ARN string `json:"arn"`
		} `json:"bucket"`
		Object struct {
			Key       string  `json:"key"`
			Size      int64   `json:"size"`
			ETag      string  `json:"eTag"`
			VersionID *string `json:"versionId"`
			Sequencer string  `json:"sequencer"`
		} `json:"object"`
	} `json:"s3"`
}

// MetadataStore is the subset of dynamodb.Client the event loop needs.
type MetadataStore interface {
	MarkUploaded(ctx context.Context, id string) error
}

// handleS3Create mirrors s3_create::handle_s3_create: skip transformed-variant
// keys (more than one '/'), parse the file id, mark the metadata uploaded.
func handleS3Create(ctx context.Context, ev S3EventRecord, metadata MetadataStore) error {
	if strings.Count(ev.S3.Object.Key, "/") > 1 {
		return nil
	}
	key, err := s3key.FromS3Key(ev.S3.Object.Key)
	if err != nil {
		slog.Error("unexpected s3 key format", "key", ev.S3.Object.Key, "error", err)
		return err
	}
	if err := metadata.MarkUploaded(ctx, key.FileID); err != nil {
		return err
	}
	return nil
}

func isObjectCreated(name string) bool {
	switch name {
	case "ObjectCreated:*",
		"ObjectCreated:Put",
		"ObjectCreated:Post",
		"ObjectCreated:Copy",
		"ObjectCreated:CompleteMultipartUpload":
		return true
	}
	return false
}

// HandleMessage mirrors s3_message::handle_s3_message.
func HandleMessage(ctx context.Context, msg sqstypes.Message, metadata MetadataStore) {
	if msg.Body == nil {
		slog.Warn("what the freak (no message body)")
		return
	}
	var notification S3EventNotification
	if err := json.Unmarshal([]byte(*msg.Body), &notification); err != nil {
		slog.Error("failed to deserialize sqs message", "error", err)
		return
	}
	for _, ev := range notification.Records {
		if !isObjectCreated(ev.EventName) {
			continue
		}
		if err := handleS3Create(ctx, ev, metadata); err != nil {
			slog.Error("failed to handle S3::CreateObject:*", "error", err)
		}
	}
}

// PollS3Events mirrors poll::poll_s3_events: long-poll the queue forever,
// backing off 20s on receive errors. It never returns unless ctx is done.
func PollS3Events(ctx context.Context, sqsClient *sqs.Client, queueURL string, metadata MetadataStore) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := poll(ctx, sqsClient, queueURL, metadata); err != nil {
			time.Sleep(20 * time.Second)
		}
	}
}

func poll(ctx context.Context, sqsClient *sqs.Client, queueURL string, metadata MetadataStore) error {
	out, err := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:        aws.String(queueURL),
		WaitTimeSeconds: 20,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("sqs polling request failed", "error", err)
		return err
	}
	for _, msg := range out.Messages {
		if msg.ReceiptHandle == nil {
			continue
		}
		if _, err := sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl:      aws.String(queueURL),
			ReceiptHandle: msg.ReceiptHandle,
		}); err != nil {
			slog.Error("failed to delete sqs message", "error", err)
		}
		HandleMessage(ctx, msg, metadata)
	}
	return nil
}

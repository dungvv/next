// Package dynamodb mirrors service/dynamodb: the file metadata table keyed by
// file_id, including serde_dynamo-compatible extension_data handling.
package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/dungvv/next/services/static-file-service-go/internal/jsonav"
)

// MetadataObject mirrors service/dynamodb/model.rs MetadataObject.
type MetadataObject struct {
	FileID        string          `json:"file_id"`
	OwnerID       string          `json:"owner_id"`
	ContentType   string          `json:"content_type"`
	IsUploaded    bool            `json:"is_uploaded"`
	LastAccessed  time.Time       `json:"last_accessed"`
	ExtensionData json.RawMessage `json:"extension_data"`
	FileName      string          `json:"file_name"`
	S3Key         string          `json:"s3_key"`
}

// NotFoundError mirrors DeleteError::NotFound.
type NotFoundError struct{ Message string }

func (e *NotFoundError) Error() string { return "Not found: " + e.Message }

// Client mirrors DynamodbClient.
type Client struct {
	inner dynamodbAPI
	table string
}

type dynamodbAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	BatchGetItem(ctx context.Context, in *dynamodb.BatchGetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
}

func New(inner dynamodbAPI, table string) *Client {
	return &Client{inner: inner, table: table}
}

func (c *Client) PutMetadata(ctx context.Context, m MetadataObject) error {
	item, err := marshalMetadata(m)
	if err != nil {
		return fmt.Errorf("failed to convert metadata object: %w", err)
	}
	_, err = c.inner.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("could not put item, dynamodb: %w", err)
	}
	return nil
}

func (c *Client) GetMetadata(ctx context.Context, id string) (*MetadataObject, error) {
	out, err := c.inner.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.table),
		Key: map[string]types.AttributeValue{
			"file_id": &types.AttributeValueMemberS{Value: id},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item from metadata table: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	m, err := unmarshalMetadata(out.Item)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize metadata: %w", err)
	}
	return m, nil
}

func (c *Client) DeleteMetadata(ctx context.Context, id string) error {
	_, err := c.inner.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.table),
		Key: map[string]types.AttributeValue{
			"file_id": &types.AttributeValueMemberS{Value: id},
		},
	})
	if err != nil {
		var rnfe *types.ResourceNotFoundException
		if errors.As(err, &rnfe) {
			return &NotFoundError{Message: rnfe.ErrorMessage()}
		}
		return err
	}
	return nil
}

func (c *Client) MarkUploaded(ctx context.Context, id string) error {
	_, err := c.inner.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.table),
		Key: map[string]types.AttributeValue{
			"file_id": &types.AttributeValueMemberS{Value: id},
		},
		UpdateExpression: aws.String("SET is_uploaded = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberBOOL{Value: true},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to mark file as uploaded: %w", err)
	}
	return nil
}

// BulkGetMetadata mirrors bulk_get_metadata: BatchGetItem in chunks of 100.
func (c *Client) BulkGetMetadata(ctx context.Context, fileIDs []string) (map[string]MetadataObject, error) {
	results := make(map[string]MetadataObject, len(fileIDs))
	const chunkSize = 100 // DynamoDB BatchGetItem limit
	for start := 0; start < len(fileIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		chunk := fileIDs[start:end]
		keys := make([]map[string]types.AttributeValue, 0, len(chunk))
		for _, id := range chunk {
			keys = append(keys, map[string]types.AttributeValue{
				"file_id": &types.AttributeValueMemberS{Value: id},
			})
		}
		out, err := c.inner.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
			RequestItems: map[string]types.KeysAndAttributes{
				c.table: {Keys: keys},
			},
		})
		if err != nil {
			return nil, err
		}
		for _, item := range out.Responses[c.table] {
			m, err := unmarshalMetadata(item)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize metadata: %w", err)
			}
			results[m.FileID] = *m
		}
	}
	return results, nil
}

// BulkDeleteMetadata mirrors bulk_delete_metadata: BatchWriteItem in chunks of
// 25; entries unprocessed by DynamoDB are reported as per-item errors. The
// result slice lines up index-for-index with fileIDs.
func (c *Client) BulkDeleteMetadata(ctx context.Context, fileIDs []string) []error {
	results := make([]error, len(fileIDs))
	const batchSize = 25 // DynamoDB BatchWriteItem limit
	for start := 0; start < len(fileIDs); start += batchSize {
		end := start + batchSize
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		chunk := fileIDs[start:end]
		writes := make([]types.WriteRequest, 0, len(chunk))
		for _, id := range chunk {
			writes = append(writes, types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{
					Key: map[string]types.AttributeValue{
						"file_id": &types.AttributeValueMemberS{Value: id},
					},
				},
			})
		}
		out, err := c.inner.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{c.table: writes},
		})
		if err != nil {
			for i := range chunk {
				results[start+i] = fmt.Errorf("batch delete failed: %w", err)
			}
			continue
		}
		for _, wr := range out.UnprocessedItems[c.table] {
			if wr.DeleteRequest == nil {
				continue
			}
			av, ok := wr.DeleteRequest.Key["file_id"].(*types.AttributeValueMemberS)
			if !ok {
				continue
			}
			for i, id := range chunk {
				if id == av.Value {
					results[start+i] = errors.New("unprocessed by DynamoDB")
				}
			}
		}
	}
	return results
}

// marshalMetadata mirrors serde_dynamo::to_item(MetadataObject): all scalars
// plus extension_data stored as a DynamoDB Map (NULL when absent) and
// last_accessed as an RFC3339 string.
func marshalMetadata(m MetadataObject) (map[string]types.AttributeValue, error) {
	extAV, err := jsonav.FromJSON(m.ExtensionData)
	if err != nil {
		return nil, err
	}
	return map[string]types.AttributeValue{
		"file_id":        &types.AttributeValueMemberS{Value: m.FileID},
		"owner_id":       &types.AttributeValueMemberS{Value: m.OwnerID},
		"content_type":   &types.AttributeValueMemberS{Value: m.ContentType},
		"is_uploaded":    &types.AttributeValueMemberBOOL{Value: m.IsUploaded},
		"last_accessed":  &types.AttributeValueMemberS{Value: m.LastAccessed.UTC().Format(time.RFC3339Nano)},
		"extension_data": extAV,
		"file_name":      &types.AttributeValueMemberS{Value: m.FileName},
		"s3_key":         &types.AttributeValueMemberS{Value: m.S3Key},
	}, nil
}

func unmarshalMetadata(item map[string]types.AttributeValue) (*MetadataObject, error) {
	m := &MetadataObject{}
	m.FileID = avString(item["file_id"])
	m.OwnerID = avString(item["owner_id"])
	m.ContentType = avString(item["content_type"])
	if b, ok := item["is_uploaded"].(*types.AttributeValueMemberBOOL); ok {
		m.IsUploaded = b.Value
	}
	if s := avString(item["last_accessed"]); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			m.LastAccessed = t.UTC()
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			m.LastAccessed = t.UTC()
		} else {
			return nil, fmt.Errorf("last_accessed: %w", err)
		}
	}
	ext, err := jsonav.ToJSON(item["extension_data"])
	if err != nil {
		return nil, err
	}
	m.ExtensionData = ext
	m.FileName = avString(item["file_name"])
	m.S3Key = avString(item["s3_key"])
	return m, nil
}

func avString(av types.AttributeValue) string {
	if s, ok := av.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

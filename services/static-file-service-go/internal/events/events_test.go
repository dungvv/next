package events

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeStore struct{ marked []string }

func (f *fakeStore) MarkUploaded(_ context.Context, id string) error {
	f.marked = append(f.marked, id)
	return nil
}

const samplePut = `{"Records":[{"eventVersion":"2.1","eventSource":"aws:s3","awsRegion":"us-east-1","eventTime":"2025-01-30T20:41:13.232Z","eventName":"ObjectCreated:Put","userIdentity":{"principalId":"AWS:X"},"requestParameters":{"sourceIPAddress":"1.2.3.4"},"responseElements":{"x-amz-request-id":"R1","x-amz-id-2":"id2"},"s3":{"s3SchemaVersion":"1.0","configurationId":"c1","bucket":{"name":"static-files-dev","ownerIdentity":{"principalId":"P"},"arn":"arn:aws:s3:::static-files-dev"},"object":{"key":"file/9c1d3a03-639e-4cc7-8a3a-681b19e70a64","size":84434,"eTag":"a258","sequencer":"00679"}}}]}`

func TestHandleMessageMarksUploaded(t *testing.T) {
	store := &fakeStore{}
	HandleMessage(context.Background(), sqstypes.Message{Body: aws.String(samplePut)}, store)
	want := "9c1d3a03-639e-4cc7-8a3a-681b19e70a64"
	if len(store.marked) != 1 || store.marked[0] != want {
		t.Fatalf("marked=%v want [%s]", store.marked, want)
	}
}

func TestHandleMessageSkipsVariantsAndRemovals(t *testing.T) {
	store := &fakeStore{}
	body := `{"Records":[
		{"eventName":"ObjectCreated:Put","s3":{"object":{"key":"file/abc/format=webp,width=300"}}},
		{"eventName":"ObjectRemoved:Delete","s3":{"object":{"key":"file/abc"}}}
	]}`
	HandleMessage(context.Background(), sqstypes.Message{Body: aws.String(body)}, store)
	if len(store.marked) != 0 {
		t.Fatalf("marked=%v want []", store.marked)
	}
}

func TestIsObjectCreated(t *testing.T) {
	for name, want := range map[string]bool{
		"ObjectCreated:Put":                     true,
		"ObjectCreated:*":                       true,
		"ObjectCreated:CompleteMultipartUpload": true,
		"ObjectCreated:Copy":                    true,
		"ObjectCreated:Post":                    true,
		"ObjectRemoved:Delete":                  false,
		"TestEvent":                             false,
	} {
		if got := isObjectCreated(name); got != want {
			t.Errorf("isObjectCreated(%q)=%v want %v", name, got, want)
		}
	}
}

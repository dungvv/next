package dynamodb

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMetadataRoundTrip(t *testing.T) {
	m := MetadataObject{
		FileID:        "id-1",
		OwnerID:       "macro|user@macro.com",
		ContentType:   "application/pdf",
		IsUploaded:    true,
		LastAccessed:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ExtensionData: json.RawMessage(`{"a":1,"b":["x",true,null]}`),
		FileName:      "f.pdf",
		S3Key:         "file/id-1",
	}
	item, err := marshalMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := item["extension_data"].(*types.AttributeValueMemberM); !ok {
		t.Fatalf("extension_data should be M, got %T", item["extension_data"])
	}
	got, err := unmarshalMetadata(item)
	if err != nil {
		t.Fatal(err)
	}
	if got.FileID != m.FileID || got.OwnerID != m.OwnerID || got.ContentType != m.ContentType ||
		got.IsUploaded != m.IsUploaded || got.FileName != m.FileName || got.S3Key != m.S3Key {
		t.Fatalf("mismatch %+v", got)
	}
	if !got.LastAccessed.Equal(m.LastAccessed) {
		t.Fatalf("last_accessed %v != %v", got.LastAccessed, m.LastAccessed)
	}
	var ext map[string]any
	if err := json.Unmarshal(got.ExtensionData, &ext); err != nil {
		t.Fatal(err)
	}
	if ext["a"] == nil || ext["b"] == nil {
		t.Fatalf("bad extension_data %s", got.ExtensionData)
	}
}

func TestMetadataNullExtensionData(t *testing.T) {
	m := MetadataObject{FileID: "id-2", FileName: "f", S3Key: "file/id-2", LastAccessed: time.Now().UTC()}
	item, err := marshalMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := item["extension_data"].(*types.AttributeValueMemberNULL); !ok {
		t.Fatalf("absent extension_data should serialize as NULL, got %T", item["extension_data"])
	}
	got, err := unmarshalMetadata(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ExtensionData) != 0 {
		t.Fatalf("expected empty extension_data, got %s", got.ExtensionData)
	}
}

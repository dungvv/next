// Package s3key mirrors crates/s3_key StaticFileKey: keys in the static file
// bucket are `file/{file_id}` (original) or `file/{file_id}/{transform_key}`
// (transformed variant such as `format=webp,width=300`).
package s3key

import (
	"fmt"
	"strings"
)

const staticFilePrefix = "file"

// Key is a parsed static-file S3 key.
type Key struct {
	FileID       string
	TransformKey string // empty for the original upload
}

// New returns the key for the original upload of fileID.
func New(fileID string) Key { return Key{FileID: fileID} }

// FromS3Key mirrors StaticFileKey::from_s3_key.
func FromS3Key(key string) (Key, error) {
	rest, ok := strings.CutPrefix(key, staticFilePrefix+"/")
	if !ok {
		return Key{}, fmt.Errorf("expected key to start with '%s/'", staticFilePrefix)
	}
	if rest == "" {
		return Key{}, fmt.Errorf("file_id is empty")
	}
	fileID, transformKey, hasTransform := strings.Cut(rest, "/")
	if fileID == "" {
		return Key{}, fmt.Errorf("file_id is empty")
	}
	if hasTransform && transformKey == "" {
		return Key{}, fmt.Errorf("transform_key is empty")
	}
	return Key{FileID: fileID, TransformKey: transformKey}, nil
}

// String reconstructs the S3 key (StaticFileKey::to_key).
func (k Key) String() string {
	if k.TransformKey == "" {
		return staticFilePrefix + "/" + k.FileID
	}
	return staticFilePrefix + "/" + k.FileID + "/" + k.TransformKey
}

// VariantPrefix mirrors StaticFileKey::variant_prefix: `file/{file_id}/`.
func (k Key) VariantPrefix() string {
	return staticFilePrefix + "/" + k.FileID + "/"
}

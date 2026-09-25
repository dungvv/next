package staticfile

import (
	"fmt"
	"strings"
)

// staticFilePrefix mirrors s3_key::STATIC_FILE_PREFIX.
const staticFilePrefix = "file"

// StaticFileKey ports s3_key::StaticFileKey.
//
//	Original: file/{file_id}
//	Variant:  file/{file_id}/{transform_key}
type StaticFileKey struct {
	FileID       string
	TransformKey string // empty for the original object
}

func NewStaticFileKey(fileID string) StaticFileKey {
	return StaticFileKey{FileID: fileID}
}

// Key reconstructs the S3 key string.
func (k StaticFileKey) Key() string {
	if k.TransformKey == "" {
		return staticFilePrefix + "/" + k.FileID
	}
	return staticFilePrefix + "/" + k.FileID + "/" + k.TransformKey
}

// VariantPrefix returns the prefix covering all variants: file/{file_id}/
func (k StaticFileKey) VariantPrefix() string {
	return staticFilePrefix + "/" + k.FileID + "/"
}

// ParseStaticFileKey parses an S3 key from the static file bucket.
func ParseStaticFileKey(key string) (StaticFileKey, error) {
	rest, ok := strings.CutPrefix(key, staticFilePrefix+"/")
	if !ok {
		return StaticFileKey{}, fmt.Errorf("expected key to start with '%s/'", staticFilePrefix)
	}
	if rest == "" {
		return StaticFileKey{}, fmt.Errorf("file_id is empty")
	}
	fileID, transform, hasSlash := strings.Cut(rest, "/")
	if !hasSlash {
		return StaticFileKey{FileID: rest}, nil
	}
	if fileID == "" {
		return StaticFileKey{}, fmt.Errorf("file_id is empty")
	}
	if transform == "" {
		return StaticFileKey{}, fmt.Errorf("transform_key is empty")
	}
	return StaticFileKey{FileID: fileID, TransformKey: transform}, nil
}

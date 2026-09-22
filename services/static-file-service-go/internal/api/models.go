package api

import "encoding/json"

// PutFileRequest mirrors model::api::PutFileRequest.
type PutFileRequest struct {
	FileName      string          `json:"file_name"`
	ContentType   *string         `json:"content_type"`
	ExtensionData json.RawMessage `json:"extension_data"`
}

// PutFileResponse mirrors model::api::PutFileResponse.
type PutFileResponse struct {
	UploadURL    string `json:"upload_url"`
	FileLocation string `json:"file_location"`
	ID           string `json:"id"`
}

// FileMetadata mirrors models_sfs::FileMetadata (GetFileMetadataResponse).
type FileMetadata struct {
	FileID        string          `json:"file_id"`
	ContentType   string          `json:"content_type"`
	IsUploaded    bool            `json:"is_uploaded"`
	ExtensionData json.RawMessage `json:"extension_data"`
	FileName      string          `json:"file_name"`
	OwnerID       string          `json:"owner_id"`
	S3Key         string          `json:"s3_key"`
}

// BulkDeleteRequest mirrors model::api::BulkDeleteRequest.
type BulkDeleteRequest struct {
	FileIDs []string `json:"file_ids"`
}

// maxFileIDs mirrors BulkDeleteRequest::max_file_ids (the DynamoDB
// BatchGetItem limit used to fetch metadata).
const maxFileIDs = 100

// DeleteResult mirrors model::api::DeleteResult.
type DeleteResult struct {
	FileID  string  `json:"file_id"`
	Success bool    `json:"success"`
	Error   *string `json:"error"`
}

// BulkDeleteResponse mirrors model::api::BulkDeleteResponse.
type BulkDeleteResponse struct {
	Total     int            `json:"total"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Results   []DeleteResult `json:"results"`
}

// ErrorResponse mirrors model::response::ErrorResponse.
type ErrorResponse struct {
	Message string `json:"message"`
}

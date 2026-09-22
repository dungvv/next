package api

import "net/http"

// openAPIDoc mirrors swagger::ApiDoc::openapi() from the Rust service: the
// documented file endpoints, request/response schemas, and auth semantics.
const openAPIDoc = `{
  "openapi": "3.1.0",
  "info": {
    "title": "macro static file service",
    "version": "0.1.0",
    "termsOfService": "https://macro.com/terms"
  },
  "tags": [{"name": "macro static file service", "description": "Static File Service"}],
  "paths": {
    "/file/{file_id}": {
      "get": {
        "tags": ["s3::file"],
        "summary": "cdn routes to s3 (documentation only)",
        "parameters": [{"name": "file_id", "in": "path", "required": true, "schema": {"type": "string"}, "description": "Unique identifier of the file to retrieve"}],
        "responses": {
          "200": {"description": "File contents retrieved successfully"},
          "404": {"description": "File not found"}
        }
      },
      "delete": {
        "operationId": "handle_delete_file",
        "parameters": [{"name": "file_id", "in": "path", "required": true, "schema": {"type": "string"}, "description": "File ID"}],
        "responses": {
          "200": {"description": "Deleted", "content": {"text/plain": {}}},
          "401": {"description": "Unauthorized"},
          "403": {"description": "Forbidden"},
          "404": {"description": "Not found"}
        }
      }
    },
    "/api/file": {
      "put": {
        "operationId": "put_presigned_url",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/PutFileRequest"}}}},
        "responses": {
          "200": {"description": "Presigned PUT URL", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/PutFileResponse"}}}},
          "401": {"description": "Unauthorized"},
          "415": {"description": "Unknown or unsupported media type"},
          "500": {"description": "Internal server error"}
        }
      }
    },
    "/api/file/{file_id}/presigned-url": {
      "get": {
        "operationId": "handle_get_presigned_url",
        "parameters": [{"name": "file_id", "in": "path", "required": true, "schema": {"type": "string"}, "description": "File ID"}],
        "responses": {
          "200": {"description": "Presigned URL for the file", "content": {"text/plain": {}}},
          "401": {"description": "Unauthorized"},
          "404": {"description": "File not found"},
          "500": {"description": "Internal server error"}
        }
      }
    },
    "/api/file/metadata/{file_id}": {
      "get": {
        "operationId": "handle_get_metadata",
        "parameters": [{"name": "file_id", "in": "path", "required": true, "schema": {"type": "string"}, "description": "File ID"}],
        "responses": {
          "200": {"description": "File metadata", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/FileMetadata"}}}},
          "401": {"description": "Unauthorized"},
          "404": {"description": "Not found"},
          "500": {"description": "Internal server error"}
        }
      }
    },
    "/api/file/bulk-delete": {
      "post": {
        "operationId": "handle_bulk_delete_file",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/BulkDeleteRequest"}}}},
        "responses": {
          "200": {"description": "Bulk delete results", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/BulkDeleteResponse"}}}},
          "400": {"description": "Validation error"},
          "401": {"description": "Unauthorized"},
          "403": {"description": "Forbidden"},
          "500": {"description": "Internal server error"}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "PutFileRequest": {
        "type": "object",
        "required": ["file_name"],
        "properties": {
          "file_name": {"type": "string"},
          "content_type": {"type": "string", "nullable": true},
          "extension_data": {"nullable": true}
        }
      },
      "PutFileResponse": {
        "type": "object",
        "required": ["upload_url", "file_location", "id"],
        "properties": {
          "upload_url": {"type": "string"},
          "file_location": {"type": "string"},
          "id": {"type": "string"}
        }
      },
      "FileMetadata": {
        "type": "object",
        "required": ["file_id", "content_type", "is_uploaded", "file_name", "owner_id", "s3_key"],
        "properties": {
          "file_id": {"type": "string"},
          "content_type": {"type": "string"},
          "is_uploaded": {"type": "boolean"},
          "extension_data": {"nullable": true},
          "file_name": {"type": "string"},
          "owner_id": {"type": "string"},
          "s3_key": {"type": "string"}
        }
      },
      "BulkDeleteRequest": {
        "type": "object",
        "required": ["file_ids"],
        "properties": {"file_ids": {"type": "array", "items": {"type": "string"}}}
      },
      "BulkDeleteResponse": {
        "type": "object",
        "required": ["total", "succeeded", "failed", "results"],
        "properties": {
          "total": {"type": "integer"},
          "succeeded": {"type": "integer"},
          "failed": {"type": "integer"},
          "results": {"type": "array", "items": {"$ref": "#/components/schemas/DeleteResult"}}
        }
      },
      "DeleteResult": {
        "type": "object",
        "required": ["file_id", "success"],
        "properties": {
          "file_id": {"type": "string"},
          "success": {"type": "boolean"},
          "error": {"type": "string", "nullable": true}
        }
      },
      "ErrorResponse": {
        "type": "object",
        "required": ["message"],
        "properties": {"message": {"type": "string"}}
      }
    }
  }
}`

func openAPIHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(openAPIDoc))
}

// swaggerUI mirrors the utoipa-swagger-ui surface at /api/docs backed by
// /api/api-doc/openapi.json. Assets load from the unpkg CDN rather than being
// embedded the way utoipa bundles them.
const swaggerUIPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>static file service — Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: "/api/api-doc/openapi.json",
        dom_id: "#swagger-ui",
      });
    };
  </script>
</body>
</html>`

func swaggerUIHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerUIPage))
}

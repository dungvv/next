# static-file-service-go

Go port of the Rust `services/static_file_service`: file upload metadata in
DynamoDB, presigned S3 upload/download URLs, single and bulk deletes (including
transformed-variant cleanup), and an SQS long-poll loop that marks files
uploaded from S3 event notifications.

The port intentionally mirrors the Rust behavior contract:

- **Auth** (`internal/authz`): `UserOrInternal` policy. Internal callers send
  `x-internal-auth-key` (or the legacy `x-document-storage-service-auth-key`)
  plus optional `x-internal-macro-user-id` / org / fusion headers; acting user
  defaults to `macro|INTERNAL@macro.com`. Users authenticate with
  `?macro-api-token=` (RS256, `kid=macro`) or `Authorization: Bearer` / the
  env-prefixed `macro-access-token` cookie (HS256, aud+iss+exp). More than one
  explicit credential kind yields `400 ambiguous credentials`; missing/invalid
  credentials yield `401 unauthorized` as `{"message": ...}`.
- **Metadata** (`internal/dynamodb`): `file_id` partition key; `extension_data`
  round-trips arbitrary JSON through DynamoDB M/NULL exactly like serde_dynamo;
  `last_accessed` stored as RFC3339.
- **Storage** (`internal/s3client`): PUT presigns expire in 2 minutes, GET in
  1 hour; under `LOCAL_AWS_URL` the client uses path-style addressing and
  rewrites presigned URLs to `LOCAL_AWS_PUBLIC_URL` / `localhost` like
  `macro_aws_config::transform_aws_url`.
- **Events** (`internal/events`): long-polls (20s) the S3 event queue, deletes
  each message, and calls `mark_uploaded` on `ObjectCreated:*` records whose
  key is an original upload (`file/{id}`, skipping `file/{id}/{transform}`).
- **File types** (`internal/filetype`): the 423-entry extension→MIME table
  generated from `crates/model_file_type` (used when `content_type` is absent).

## Routes

| Route | Mirrors |
| --- | --- |
| `GET /api/health` | `health::health_handler` |
| `PUT /api/file` | `put_presigned_url::put_presigned_url` |
| `GET /api/file/metadata/{file_id}` | `metadata::handle_get_metadata` |
| `GET /api/file/{file_id}/presigned-url` | `get_file::handle_get_presigned_url` |
| `DELETE /api/file/{file_id}` | `delete_file::handle_delete_file` |
| `POST /api/file/bulk-delete` | `bulk_delete_file::handle_bulk_delete_file` |
| `/internal/...` | same file router for internal-key callers |
| `GET /api/docs`, `GET /api/api-doc/openapi.json` | swagger UI + OpenAPI |

## Configuration

Env vars match the Rust `config::Config` / `macro_env_var` names:

| Var | Default |
| --- | --- |
| `ENVIRONMENT` | `prod` (`prod`/`dev`/`local`) |
| `PORT` | `8080` |
| `STATIC_FILE_SERVICE_DYNAMODB_TABLE_NAME` | required |
| `STATIC_STORAGE_BUCKET` | required |
| `STATIC_FILE_SERVICE_URL` | per-env default |
| `INTERNAL_API_KEY` | required |
| `AUDIENCE`, `ISSUER`, `MACRO_API_TOKEN_ISSUER` | required |
| `JWT_SECRET_KEY`, `MACRO_API_TOKEN_PUBLIC_KEY` | literal locally; Secrets Manager secret name in dev/prod |
| `LOCAL_AWS_URL`, `LOCAL_AWS_PUBLIC_URL` | LocalStack endpoint / browser origin |
| `OVERRIDE_STATIC_FILE_SERVICE_S3_EVENT_QUEUE_URL` | per-env queue name |
| `LOCAL_AUTH` or `STATIC_FILE_SERVICE_DISABLE_EVENT_POLL` | disables the SQS poller (Rust `local_auth` feature) |
| `ALLOWED_ORIGINS` | comma-separated CORS origin override |

Not ported: tracing/OTel spans (replaced by `slog`), the Doppler config
binary, and the utoipa-bundled swagger assets (the Go swagger page loads
swagger-ui from the unpkg CDN; the OpenAPI JSON is equivalent).

## Run

```sh
go build ./cmd/server && ./server
```

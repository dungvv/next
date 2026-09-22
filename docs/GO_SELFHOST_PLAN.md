# Plan: Port backend Rust → Go, self-host, không phụ thuộc AWS

## Mục tiêu

Port toàn bộ backend Rust (~300k LOC: `services/` ~118k, `crates/` ~182k) sang Go với
ràng buộc:

- Không phụ thuộc AWS/Cloudflare/Doppler — chạy self-host hoàn chỉn.
- Deploy **all-in-one binary** + docker-compose.
- Queue/event **Postgres-only** (River + LISTEN/NOTIFY + outbox pattern).
- Identity: **Casdoor** (thay FusionAuth).

## Kiến trúc đích

```
macro (single Go binary)
├── macro api        # tất cả HTTP routers (17 axum services → sub-routers)
├── macro worker     # River workers (toàn bộ ex-Lambda + ex-Kafka consumers)
├── macro gateway    # websocket: connection_gateway + websocket-service
├── macro sync       # sync-service (CRDT)
├── macro scheduler  # River periodic jobs (ex-EventBridge cron)
└── macro migrate    # DB migrations (golang-migrate, reuse 367 file .sql)
```

Docker-compose self-host tối thiểu:

- `postgres` (+`pgvector`) — chứa 4 database: macrodb, email, comms, notification
- `minio` — object storage (S3 API-compatible)
- `valkey` — cache/rate-limit/conn registry (Redis-compatible)
- `casdoor` — identity provider
- `livekit` — calls/transcription (đã self-host được)
- `macro` — binary trên
- SMTP do user cung cấp (`mailhog` cho dev)
- Optional: OTel collector + Prometheus/Grafana/Loki

## Bảng thay thế hạ tầng

| AWS/Cloud hiện tại | Self-host | Cách thực hiện |
|---|---|---|
| SQS (`macro_queues`) | River job kinds | Mỗi typed queue → 1 job kind + worker |
| SNS (event fanout) | PG outbox + LISTEN/NOTIFY | Bảng `outbox` + relay process |
| Kafka (`macro_event_broker`, agent_trigger, search indexing) | PG outbox partitioned + River consumers | Giữ ordering per aggregate key; `EventBus` port cho phép gắn NATS sau |
| EventBridge cron (deleted_item_poller, org_retention_trigger, email_scheduled…) | River periodic jobs | |
| Lambda (~20 handlers) | River job kinds trong `macro worker` | Map 1:1 |
| DynamoDB (conn tracking, bulk upload) | Postgres tables + Valkey TTL | Conn registry: Valkey expiry, fallback PG |
| DynamoDB Streams (upload_extractor_trigger) | PG trigger → outbox | |
| S3 | MinIO | Giữ `aws-sdk-go-v2`, chỉ đổi endpoint |
| SecretsManager / Doppler | env vars / Infisical / sops | `SecretPort` interface |
| SES (gửi mail) | SMTP (`go-mail`/`gomail`) | `MailPort` interface |
| SNS push (APNs/FCM) | FCM HTTP v1 + `apns2`, hoặc ntfy | `PushPort` interface |
| SES bounce suppression | Webhook từ mail provider hoặc feature-flag | |
| ECS task (worker_trigger, agent_harness containers) | Docker socket API / in-process | `RuntimePort`; k8s Job adapter optional |
| Lambda invoke (convert_service, docx_unzip) | river enqueue trực tiếp | |
| OpenSearch | Postgres FTS (tsvector) + pgvector | `SearchPort`; Meilisearch adapter optional |
| Redis | Valkey | drop-in |
| Cloudflare Workers (sync-service, lexical, ai-editing, analytics-proxy, cla-worker, websocket-service) | Go modules trong binary | D1 → Postgres; Durable Objects → in-process + PG advisory lock |
| FusionAuth | Casdoor (`casdoor-go-sdk`) | `IdentityPort` interface |
| Datadog | OTel → Prometheus/Grafana/Loki | Giữ OTel exporter → vẫn gắn DD được |
| Gmail/Microsoft OAuth, Anthropic, Pipedream | external SaaS (bản chất integration) | Giữ, đặt sau feature flag |

## Stack mapping Rust → Go

| Rust | Go | Ghi chú |
|---|---|---|
| axum + tower | `chi` + `net/http` middleware | Hexagonal ports → interfaces |
| sqlx (compile-checked) | `sqlc` + `pgx/v5` | Giữ nguyên schema + migrations |
| lambda_runtime | River workers | Không còn lambda |
| rdkafka / kafka_util | PG outbox consumer (franz-go nếu cần adapter) | |
| aws-sdk (s3/sqs/sns/ses/dynamo/secrets) | `aws-sdk-go-v2` (S3→MinIO) + port interfaces | |
| async-graphql (`graphql_*`) | `gqlgen` | Schema parity cho `soup` |
| rmcp (mcp_service) | `modelcontextprotocol/go-sdk` | Kiểm tra feature parity |
| reqwest | `net/http` / `resty` | |
| macro_env_var/macro_config/Doppler | `caarlos0/env` + env files | |
| macro_auth/JWT | `golang-jwt/v5` + `go-oidc` | |
| Redis | `go-redis/v9` (Valkey) | |
| OpenSearch | PG FTS (`SearchPort`) | |
| Gmail/Google | `google.golang.org/api` | |
| tracing + Datadog | OTel + `slog` | `macro_entrypoint` → `pkg/entrypoint` |
| Websocket | `coder/websocket` | |
| utoipa/OpenAPI | `oapi-codegen` hoặc `huma` | `packages/sdk` giữ contract |

## Go module layout

```
cmd/macro/                # entrypoint, subcommands
internal/
  api/                    # sub-routers: auth, dss, dcs, email, calendar,
                          # contacts, convert, static, imageproxy, unfurl,
                          # notification, scheduled, mcp, search...
  gateway/                # ws: connection registry + fanout
  sync/                   # CRDT sync
  jobs/                   # river job kinds (ex-lambdas, ex-kafka consumers)
pkg/
  config/   auth/   httpmw/   store/    # sqlc: macrodb, emaildb, commsdb, notifdb
  queue/    events/ (outbox+notify)     cache/   objectstore/ (minio)
  search/   mail/   push/     secrets/  identity/ (casdoor)
  models/   lexical/          runtime/  (docker adapter)
  # domain ports giữ nguyên hexagonal: soup, entity_access, properties,
  # email, channels, documents, agent_*, notification, ai_tools...
```

## Plan theo từng module

### A. `macro api` — HTTP routers

| Module (từ service) | Ports cần | Size (Rust LOC) | Ghi chú self-host |
|---|---|---|---|
| `static_file_service` | ObjectStore | ~600 | Serve từ MinIO/local FS — làm đầu tiên |
| `image_proxy_service` | — | ~1.1k | `imaging`/libvips |
| `unfurl_service` | — | ~3k | Port SSRF guard `http_safety` |
| `contacts_service` | store | ~800 | Nhỏ nhất |
| `convert_service` | queue, objectstore, store | ~1.6k | `lambda invoke` → river enqueue |
| `notification_service` | push, store(notifdb), events | ~2.2k + notification crate 21k | SNS→FCM/APNs; poller→periodic job |
| `scheduled_action` | store, gateway | ~2.3k | pg-polling sẵn → river worker; live updates → NOTIFY |
| `calendar_service` | store(emaildb), gateway | ~3k + calendar_events 29k | google_token giữ (external OAuth) |
| `mcp_auth_proxy` | cache, casdoor | ~1.5k | FusionAuth→Casdoor OAuth flow |
| `mcp_service` | identity, mcp go-sdk, events | ~3.6k | Kiểm tra parity rmcp→go-sdk |
| `search_processing_service` | SearchPort (PG FTS/pgvector) | ~5.5k | Kafka→outbox consumer; tsvector thay OpenSearch |
| `authentication_service` | casdoor, store(3 DB), cache, mail, queue | ~6k + domain crates | login/oauth/oauth2/jwt/session/permissions/teams/referral/gtm/cursor-key/codex/github-pr/webhooks; port `microsoft_token_cipher` (AES) |
| `email_service` | mail, store(emaildb), queue, objectstore | ~5.7k + email 31k + email_db 17k | gmail_client→google API; backfill→river |
| `document_storage_service` | store, search, gateway, objectstore, lexical | ~9k + soup 29k + documents 26k + entity_access 26k | Nặng nhất: soup GraphQL→gqlgen, DynamoDB→PG |
| `document_cognition_service` | anthropic, store, search, mcp, gateway | ~8.5k + ai_tools/mcp_client/agent crates | SSE streaming |
| `agent_harness_service` | runtime(docker), queue, secrets, objectstore | ~6.8k + agent_* ~60k | `containers.rs` → Docker adapter; chú ý sandbox isolation |

### B. `macro gateway` — realtime

| Từ | Thay thế |
|---|---|
| `connection_gateway` | `coder/websocket`; DynamoDB conn registry → Valkey + PG fallback; fanout → LISTEN/NOTIFY; port script stale_connections |
| `websocket-service` (TS) | Port sang Go, merge vào gateway |

### C. `macro sync` — sync-service (quyết định riêng)

Rust→WASM / Durable Objects / D1 / loro CRDT → Go: chạy **yrs/loro WASM trong
`wazero`**, doc state lưu Postgres (thay D1), mutex per-doc bằng PG advisory lock.
Module rủi ro nhất — prototype sớm.

### D. `macro worker` — River job kinds (ex-Lambda + ex-Kafka consumers)

| Pipeline | Job kinds | Trigger mới |
|---|---|---|
| Upload | `extract_trigger` (PG outbox thay DynamoDB stream), `extract`, `docx_unzip`, `upload_finalize`, `text_extract` | MinIO bucket notification → river; hoặc outbox event |
| Email | `email_refresh`, `email_scheduled` (periodic), `email_sfs_delete`, `email_suppression` | Suppression: webhook inbound route thay SES-SNS |
| Search/AI | `search_upload`, `ai_projections_refresh`, `search_index` | Kafka → outbox consumer |
| Lifecycle | `delete_chat`, `deleted_item_poll` (periodic), `user_link_cleanup`, `sha_cleanup`, `org_retention_trigger` (periodic) + `org_retention_run` | |
| Misc | `dlp` (webhook route), `call_recording_preview` (bundle ffmpeg trong image), `image_optimize`, `worker_spawn` (ECS→RuntimePort) | |
| Agents | `agent_trigger` (Kafka→outbox consumer, ordering per channel), `coding_agent_worker` | |

### E. TS/Python services → fold vào binary hoặc sidecar

| Service | Xử lý |
|---|---|
| `lexical-service` (TS, CF worker) | Port sang `pkg/lexical` (Go) + giữ HTTP route tương thích (Rust services gọi qua `lexical_client`) |
| `ai-editing-worker`, `cla-worker`, `analytics-proxy`, `coding-agent-worker`, `bots/*` (TS) | Port sang Go module/job; D1→PG |
| `transcription` (Python/LiveKit) | Giữ sidecar Python, hoặc port `livekit-go` SDK — optional |
| `crates/client` (cache-wasm/turso) | Phục vụ frontend WASM — không convert |
| `apps/web`, `packages/*` | Frontend giữ nguyên TypeScript |

## Phases

0. **Foundation**: go module layout, port interfaces, `pkg/store` + sqlc wiring,
   compose dev env (postgres/minio/valkey/casdoor/livekit/mailhog), OTel,
   `macro migrate` reuse migrations.
1. **DB layer** (chunk lớn nhất): port sqlx→sqlc theo domain —
   macro_db_client (~26k), email_db_client (~17k), comms_db_client,
   notification_db_client.
2. **Leaf routers**: static, image proxy, unfurl, contacts, convert — validate stack.
3. **Worker job kinds**: toàn bộ ex-lambda; ít rủi ro, testable độc lập.
4. **Mid routers**: notification → scheduled → calendar → mcp_auth_proxy (casdoor)
   → mcp → search_processing (PG FTS).
5. **Gateway**: connection_gateway + websocket-service.
6. **Heavies**: authentication (casdoor) → email → dss (gqlgen) → dcs →
   agent_harness (+trigger, docker runtime).
7. **Sync + edge ports**: sync-service (wazero prototype), lexical/ai-editing/cla.
8. **Packaging**: Dockerfile (kèm ffmpeg), `docker-compose.yml` production,
   `macro migrate`, docs self-host (env, SMTP, MinIO buckets, Casdoor bootstrap).

## Rủi ro

1. **Kafka → PG outbox**: mất replay/ordering tổng quát. Giải: outbox partition
   theo aggregate key + retention; `EventBus` port sẵn sàng cho NATS adapter.
2. **Search**: PG FTS yếu hơn OpenSearch (relevance, aggregations) — `SearchPort`
   cho phép swap Meilisearch; pgvector cho semantic.
3. **CRDT sync**: không có loro native Go — wazero+yrs WASM là đường khả thi nhất;
   prototype ở phase sớm để de-risk.
4. **Agent runtime isolation**: `agent_harness` chạy code agent — Docker socket
   trên self-host cần cân nhắc bảo mật (gVisor/k8s Job adapter).
5. **SES suppression + DLP**: không có equivalent tự nhiên với SMTP — webhook
   generic hoặc feature-flag.
6. **Push notification**: FCM/APNs trực tiếp thay SNS — quản lý device token +
   credentials trong config.
7. **GraphQL soup parity**: gqlgen schema phải khớp frontend — contract test bằng
   `packages/sdk` + `crates/integration_tests` chạy qua HTTP.
8. **sqlx → sqlc**: hàng nghìn query compile-checked — phần việc lớn nhất; giữ
   nguyên DB giúp chạy song song Rust/Go trên cùng data để đối chiếu.

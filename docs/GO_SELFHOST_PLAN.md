# Plan: Port backend Rust → Go, self-host, không phụ thuộc AWS

## Mục tiêu

Port toàn bộ backend Rust (~300k LOC: `services/` ~118k, `crates/` ~182k) sang Go với
ràng buộc:

- Không phụ thuộc AWS/Cloudflare/Doppler — chạy self-host hoàn chỉn.
- Deploy **all-in-one binary** + docker-compose (mỗi service là 1 subcommand,
  chạy độc lập được để scale riêng).
- Queue/event: **NATS JetStream** (work-queue streams cho ex-SQS, streams cho
  ex-Kafka topics, core subjects cho fanout/realtime).
- Identity: **Casdoor** (thay FusionAuth).

## Kiến trúc đích

```
macro (single Go binary)
├── macro api        # HTTP routers (axum services → sub-routers, trừ chat)
├── macro chat       # chat service riêng: channels + messages + comms DB
├── macro worker     # JetStream consumers (toàn bộ ex-Lambda + ex-Kafka)
├── macro gateway    # websocket: connection_gateway (websocket-service là stub)
├── macro sync       # sync-service (CRDT)
├── macro scheduler  # embedded cron → publish JetStream (ex-EventBridge)
└── macro migrate    # DB migrations (golang-migrate, reuse 367 file .sql)
```

`macro chat` là subcommand tách riêng trong cùng binary: có thể deploy chung
1 container (`macro all`) hoặc tách container riêng để scale độc lập. Chat
service sở hữu comms DB và giao tiếp với phần còn lại qua NATS subjects —
không qua HTTP nội bộ.

Docker-compose self-host tối thiểu:

- `postgres` (+`pgvector`) — **một database chung `macrodb`**: các query join chéo
  domain (share_permission ⋈ comms_channels ⋈ email_threads) nên chưa thể tách DB
  vật lý; 4 biến `*_DB_URL` tồn tại để sizing/monitor pool nhưng phải trỏ cùng DB
  cho tới khi redesign ownership boundaries
- `nats` — JetStream enabled, queue + event bus + fanout duy nhất
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
| SQS (`macro_queues`, notification ingress/delivery, push events) | JetStream work-queue streams | Mỗi typed queue → 1 stream + durable pull consumer; `max_deliver` + `nak` thay visibility timeout |
| SNS (event fanout) | NATS core subjects | Pub/sub trực tiếp, không cần relay |
| Kafka (`macro_event_broker`: `macro.chats`/`macro.documents`/`macro.teams`, agent_trigger, search indexing) | JetStream streams | Giữ ordering per key bằng subject-shard `topic.<agg_id>` + filtered consumer; retention để replay |
| EventBridge cron (deleted_item_poller, org_retention_trigger, email_scheduled…) | Embedded cron (`robfig/cron`) trong `macro scheduler` → publish JetStream | Job vẫn chạy qua consumer → 1 đường delivery duy nhất |
| Lambda (~20 handlers) | JetStream consumers trong `macro worker` | Map 1:1, at-least-once giữ nguyên semantics |
| DynamoDB (conn tracking, bulk upload) | Postgres tables + Valkey TTL | Conn registry: Valkey expiry, fallback PG |
| DynamoDB Streams (upload_extractor_trigger) | Outbox nhẹ → publish JetStream | Bảng outbox trong cùng TX write, relay process đẩy sang NATS |
| S3 | MinIO | Giữ `aws-sdk-go-v2`, chỉ đổi endpoint |
| SecretsManager / Doppler | env vars / Infisical / sops | `SecretPort` interface |
| SES (gửi mail) | SMTP (`go-mail`/`gomail`) | `MailPort` interface |
| SNS push (APNs/FCM) | FCM HTTP v1 + `apns2`, hoặc ntfy | `PushPort` interface |
| SES bounce suppression | Webhook từ mail provider hoặc feature-flag | |
| ECS task (worker_trigger, agent_harness containers) | Docker socket API / in-process | `RuntimePort`; k8s Job adapter optional |
| Lambda invoke (convert_service, docx_unzip) | publish JetStream trực tiếp | |
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
| lambda_runtime | JetStream pull consumers | Không còn lambda |
| rdkafka / kafka_util / macro_event_broker | `nats.go` JetStream | `EventBus`/`MessageBroker` port → adapter NATS |
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
  chat/                   # SERVICE RIÊNG: channels + messages routers,
                          # commsdb store, delivery → NATS
  gateway/                # ws: connection registry + fanout (subscribe NATS)
  sync/                   # CRDT sync
  jobs/                   # JetStream consumers (ex-lambdas, ex-kafka consumers)
pkg/
  config/   auth/   httpmw/   store/    # sqlc: macrodb, emaildb, commsdb, notifdb
  nats/     # jetstream helpers: streams, durable consumers, subjects
  events/   # topic/subject catalog + envelope (thay macro_event_broker)
  cache/    objectstore/ (minio)
  search/   mail/   push/     secrets/  identity/ (casdoor)
  models/   lexical/          runtime/  (docker adapter)
  # domain ports giữ nguyên hexagonal: soup, entity_access, properties,
  # email, channels, messages, documents, agent_*, notification, ai_tools...
```

## Plan theo từng module

### A. `macro api` — HTTP routers

| Module (từ service) | Ports cần | Size (Rust LOC) | Ghi chú self-host |
|---|---|---|---|
| `static_file_service` | ObjectStore | ~600 | Serve từ MinIO/local FS — làm đầu tiên |
| `image_proxy_service` | — | ~1.1k | `imaging`/libvips |
| `unfurl_service` | — | ~3k | Port SSRF guard `http_safety` |
| `contacts_service` | store | ~800 | Nhỏ nhất |
| `convert_service` | queue, objectstore, store | ~1.6k | `lambda invoke` → publish JetStream |
| `notification_service` | push, store(notifdb), events | ~2.2k + notification crate 21k | SNS→FCM/APNs; 2 SQS → 2 JetStream work-queue streams; poller→scheduler job |
| `scheduled_action` | store, gateway | ~2.3k | pg-polling sẵn → JetStream consumer; live updates → NATS subject |
| `calendar_service` | store(emaildb), gateway | ~3k + calendar_events 29k | google_token giữ (external OAuth) |
| `mcp_auth_proxy` | cache, casdoor | ~1.5k | FusionAuth→Casdoor OAuth flow |
| `mcp_service` | identity, mcp go-sdk, events | ~3.6k | Kiểm tra parity rmcp→go-sdk |
| `search_processing_service` | SearchPort (PG FTS/pgvector) | ~5.5k | Kafka→JetStream consumer; tsvector thay OpenSearch |
| `authentication_service` | casdoor, store(3 DB), cache, mail, queue | ~6k + domain crates | login/oauth/oauth2/jwt/session/permissions/teams/referral/gtm/cursor-key/codex/github-pr/webhooks; port `microsoft_token_cipher` (AES) |
| `email_service` | mail, store(emaildb), queue, objectstore | ~5.7k + email 31k + email_db 17k | gmail_client→google API; backfill→JetStream consumer |
| `document_storage_service` | store, search, gateway, objectstore, lexical | ~9k + soup 29k + documents 26k + entity_access 26k | Nặng nhất: soup GraphQL→gqlgen, DynamoDB→PG |
| `document_cognition_service` | anthropic, store, search, mcp, gateway | ~8.5k + ai_tools/mcp_client/agent crates | SSE streaming |
| `agent_harness_service` | runtime(docker), queue, secrets, objectstore | ~6.8k + agent_* ~60k | `containers.rs` → Docker adapter; chú ý sandbox isolation |

### B. `macro chat` — service chat riêng (channels + messages)

| Module | Size (Rust LOC) | Ghi chú |
|---|---|---|
| `channels` domain + routers | ~35k | DM/group/channel, mentions, typing, reactions, side effects |
| `messages` shared service | ~7.8k | `MessageService` + `MessageEventPublisher` → delivery |
| `comms_db_client` → sqlc | ~2.8k | comms DB do chat sở hữu độc quyền |
| `channel_bots`/`channel_labels`/`channel_sender` | ~4.8k | bot hooks, labels, sender resolution |

Delivery model sau tách:

- Realtime `message_update`/`typing`/`reaction`: `MessageRealtime` port →
  publish NATS subject `realtime.user.<id>` → `macro gateway` subscribe và
  fanout xuống WS (thay HTTP `batch_send_message` của connection_gateway).
- Notification requests (mentions/messages): publish JetStream
  `notifications.ingress` → notification module consume — thay SQS ingress.
- Kafka `macro.chats` (chat crate, DCS) → JetStream stream `macro.chats`,
  subject-shard `macro.chats.<chat_id>` giữ per-chat ordering.
- `entity_access`/permissions: chat đọc qua shared `pkg/store` read-only hoặc
  gọi `macro api` — chọn shared read model để giữ chat self-contained.

Lưu ý: `crates/chat` (~10k, AI chat) **không** nằm trong `macro chat` — nó
gắn chặt với DCS streaming (Anthropic SSE), chỉ share topic `macro.chats`.

### C. `macro gateway` — realtime

| Từ | Thay thế |
|---|---|
| `connection_gateway` | `coder/websocket`; DynamoDB conn registry → Valkey + PG fallback; services publish `realtime.user.<id>` lên NATS, gateway subscribe fanout; port script stale_connections |
| `websocket-service` (TS) | Không port — chỉ là stub 23 dòng, drop |

### D. `macro sync` — sync-service (quyết định riêng)

Rust→WASM / Durable Objects / D1 / loro CRDT → Go: chạy **yrs/loro WASM trong
`wazero`**, doc state lưu Postgres (thay D1), mutex per-doc bằng PG advisory lock.
Module rủi ro nhất — prototype sớm.

### E. `macro worker` — JetStream consumers (ex-Lambda + ex-Kafka consumers)

| Pipeline | Consumers | Trigger mới |
|---|---|---|
| Upload | `extract_trigger` (outbox→JetStream thay DynamoDB stream), `extract`, `docx_unzip`, `upload_finalize`, `text_extract` | MinIO bucket notification → NATS; hoặc outbox event |
| Email | `email_refresh`, `email_scheduled` (scheduler publish), `email_sfs_delete`, `email_suppression` | Suppression: webhook inbound route thay SES-SNS |
| Search/AI | `search_upload`, `ai_projections_refresh`, `search_index` | JetStream stream thay Kafka topic |
| Lifecycle | `delete_chat`, `deleted_item_poll` (scheduler), `user_link_cleanup`, `sha_cleanup`, `org_retention_trigger` (scheduler) + `org_retention_run` | |
| Misc | `dlp` (webhook route), `call_recording_preview` (bundle ffmpeg trong image), `image_optimize`, `worker_spawn` (ECS→RuntimePort) | |
| Agents | `agent_trigger` (JetStream, ordering per channel bằng subject-shard), `coding_agent_worker` | |
| Notification delivery | `notif_ingress`, `notif_delivery`, `push_event` consumers | Thay 3 SQS queue của notification_service |

### F. TS/Python services → fold vào binary hoặc sidecar

| Service | Xử lý |
|---|---|
| `lexical-service` (TS, CF worker) | Port sang `pkg/lexical` (Go) + giữ HTTP route tương thích (Rust services gọi qua `lexical_client`) |
| `ai-editing-worker`, `cla-worker`, `analytics-proxy`, `coding-agent-worker`, `bots/*` (TS) | Port sang Go module/job; D1→PG |
| `transcription` (Python/LiveKit) | Giữ sidecar Python, hoặc port `livekit-go` SDK — optional |
| `crates/client` (cache-wasm/turso) | Phục vụ frontend WASM — không convert |
| `apps/web`, `packages/*` | Frontend giữ nguyên TypeScript |

## Phases

0. **Foundation**: go module layout, port interfaces, `pkg/store` + sqlc wiring,
   `pkg/nats` (stream/consumer bootstrap code), `pkg/events` (subject catalog +
   envelope schema versioned), compose dev env (postgres/nats/minio/valkey/
   casdoor/livekit/mailhog), OTel, `macro migrate` reuse migrations.
1. **DB layer** (chunk lớn nhất): port sqlx→sqlc theo domain —
   macro_db_client (~26k), email_db_client (~17k), comms_db_client,
   notification_db_client.
2. **Leaf routers**: static, image proxy, unfurl, contacts, convert — validate stack.
3. **Worker consumers**: toàn bộ ex-lambda → JetStream; ít rủi ro, testable độc lập.
4. **Chat service** (`macro chat`): channels + messages + commsdb — tách sớm
   để validate delivery path (NATS realtime + notification ingress) trước khi
   gateway/heavies. Song song được với phase 5 vì ít phụ thuộc.
5. **Mid routers**: notification (2 SQS → JetStream, SNS→FCM/APNs) → scheduled
   → calendar → mcp_auth_proxy (casdoor) → mcp → search_processing (PG FTS).
6. **Gateway**: connection_gateway + subscribe `realtime.*` từ NATS.
7. **Heavies**: authentication (casdoor) → email → dss (gqlgen) → dcs →
   agent_harness (+trigger, docker runtime).
8. **Sync + edge ports**: sync-service (wazero prototype — spike sớm ở phase 0),
   lexical/ai-editing/cla.
9. **Packaging**: Dockerfile (kèm ffmpeg), `docker-compose.yml` production,
   `macro migrate`, docs self-host (env, SMTP, MinIO buckets, Casdoor bootstrap,
   NATS stream provisioning qua `nats` CLI hoặc Terraform).

## Rủi ro

1. **Kafka → JetStream**: JetStream có retention/replay sẵn (tốt hơn outbox),
   nhưng ordering per aggregate key không tự nhiên như Kafka partition — cần
   subject-shard `topic.<agg_id>` + filtered durable consumer; consumer count
   tăng theo số key, cân nhắc giới hạn shard hoặc chấp nhận ordering toàn
   stream. HA: JetStream cluster cần ≥3 node nếu muốn chịu node failure —
   compose single-node là single point of failure cho cả queue lẫn events.
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

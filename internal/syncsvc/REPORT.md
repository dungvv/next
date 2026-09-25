# syncsvc feasibility spike — REPORT

Port of `services/sync-service` (Rust/WASM Cloudflare Worker + Durable Object
+ D1 + loro CRDT + Bebop) to Go, per `docs/GO_SELFHOST_PLAN.md` §"D. macro
sync" / CRDT-sync risk #3. **This is a spike, not a production port.**

## Verdict

**Wazero is feasible.** The `loro-crdt` wasm-bindgen artifact runs inside
`wazero` and every document operation the sync path needs was exercised
successfully end-to-end. The ~118 `__wbindgen_*` imports are real but tractable:
they are a JS-object heap ABI (i32 indexes into a host-side heap), not a JS
runtime. `internal/syncsvc/lorowasm` implements a minimal version of that heap
plus all 118 host functions; **0 imports fall back to stubs**.

### What was proven (TestDocRoundTrip, all PASS)

| Operation | wasm export | Result |
|---|---|---|
| new document | `lorodoc_new` | ptr `0x1312a8` |
| snapshot export | `lorodoc_export({mode:"snapshot"})` | 81 bytes |
| snapshot → doc | `lorodoc_fromSnapshot` | OK |
| re-export | `lorodoc_export` | 81 bytes |
| import | `lorodoc_import` | OK |
| oplog vv | `lorodoc_oplogVersion` + `versionvector_encode`/`decode` | OK |
| updates-since-vv | `lorodoc_export({mode:"update",from:vv})` | 22 bytes |
| shallow snapshot | `lorodoc_export({mode:"shallow-snapshot",frontiers})` | 137 bytes |
| op log replay | `lorodoc_importBatch` | implemented (used on session load) |
| awareness store | `ephemeralstorewasm_new`/`apply`/`encodeAll` | wired in engine |
| multi-doc | 2 `Instance`s sharing one `Runtime` | heap isolation verified |

Artifact: `npm pack loro-crdt` → `package/nodejs/loro_wasm_bg.wasm`
(3.27 MB, loro-crdt 1.16.3; Rust service pins `loro = "1.16.2"` — same line).
353 exported functions, 80 `lorodoc_*`.

### Import surface (all 118, module `__wbindgen_placeholder__`)

All params/returns are `externref` (i127) — the old wasm-bindgen JS-heap ABI.

- Core: `__wbindgen_string_{new,get}`, `__wbindgen_{malloc,free,realloc,
  add_to_stack_pointer}` (these 4 are *exports* the host calls),
  `__wbindgen_{number,boolean,bigint}_*`, `__wbindgen_object_{clone,drop}_ref`,
  `__wbindgen_{typeof,is_*,in,jsval_eq,jsval_loose_eq,as_number}`,
  `__wbindgen_{error_new,throw,rethrow,cb_drop,memory,debug_string}`,
  `__wbindgen_closure_wrapper{283,542}`.
- Object/reflect: `__wbg_get*`/`__wbg_set*`/`__wbg_getindex_*`/
  `__wbg_setindex_*`, `__wbg_new*` (Object/Map/Array/Uint8Array/Error),
  `__wbg_{length,buffer,subarray,push,from,isArray,isSafeInteger,entries,
  ownKeys,getOwnPropertySymbols,instanceof_*,String}`.
- Iterators: `__wbg_{iterator,next_*,done,value}` (needed by shallow-snapshot
  frontiers iteration).
- Calls: `__wbg_{call_*,apply_*}`, `__wbg_{resolve,then}` (promise stub).
- Globals/env: `__wbg_static_accessor_{GLOBAL,GLOBAL_THIS,SELF,WINDOW}`,
  `__wbg_{crypto,msCrypto,process,versions,node,require}`,
  `__wbg_{getRandomValues,randomFillSync}`, `__wbg_{now,mark,measure}`.
- Console: `__wbg_{log_*,warn,error}` → `slog`.
- Loro state-tree builders: `__wbg_state{Options,Slice,Container,Cid,
  RootCid,Value,Binary,Index,Set}` (used by get_deep_value/diff paths).
- Class wrappers: `__wbg_{loromap,lorolist,loromovablelist,lorotext,lorotree,
  lorotreenode,lorocounter,cursor,versionvector,changemodifier}_new`.

Unimplemented-but-present (stubs counted in `Runtime.StubReport()`): promise
`then`, `Function` constructor evaluation, closure bodies, FinalizationRegistry
GC (wazero has no GC hooks; Rust objects are leaked until the instance is
closed — acceptable because a session owns its instance).

### Errors hit and fixed during the spike

1. **Freelist corruption on double-drop** — wasm double-drops heap slots; the
   upstream JS freelist self-loops (`heap[idx]=idx`) and poisons later adds.
   Fixed: `jsHeap.freed` set makes `drop` idempotent + defensive grow when the
   freelist link is clobbered. Symptom: `"Invalid export mode. JsValue(Error:
   Invalid mode)"` on the second export.
2. **`__wbg_ptr must be a number`** — `export({mode:"update",from:vv})` needs
   the VersionVector *object* (`{__wbg_ptr: n}`), not the heap index. Fixed via
   `vvAsHeapObj`.
3. **`frontiers is not iterable`** — shallow-snapshot iterates
   `frontiers[Symbol.iterator]`. Fixed: `jsGet(*jsArray, jsSymbol)` returns a
   `jsIter` factory.
4. **Hang in `__wbg_next_…`** — the import calls `iter.next()`, not `iter()`.
   Fixed: dispatch through `i.callJS(i.jsGet(it,"next"), it, nil)`.
5. **Peer id `0`** — `lorodoc_new` never calls the crypto imports in this
   build (peer id is lazily/externally seeded); server docs only *import*
   updates, so this is benign for sync. Flagged if we ever commit ops on the
   server side.

## What the skeleton does

```
internal/syncsvc/
  syncsvc.go   Run(ctx, cfg) — pgx pool, schema, engine factory, http.Server,
               graceful shutdown; signature unchanged.
  config.go    env config: SYNC_INSECURE_AUTH, SYNC_INTERNAL_API_KEY,
               SYNC_WS_ORIGINS, SYNC_LORO_WASM_PATH, SYNC_FLUSH_INTERVAL,
               SYNC_IDLE_TTL, SYNC_PING_INTERVAL.
  store.go     Postgres persistence: syncsvc_documents (snapshot+vv),
               syncsvc_pending_ops (op log), syncsvc_peer_user,
               syncsvc_blame; advisory lock = dedicated pgxpool.Conn +
               pg_advisory_lock(hashtext(docID)) held for the session.
  engine.go    docEngine interface + lorowasm adapter + passthrough
               fallback (opaque bytes, no merge — plumbing-only).
  session.go   docSession = the Durable Object analogue: lock-conn, engine,
               sockets map, broadcast, flush; sessionManager = acquire/
               release/maybeClose + race-safe creation.
  server.go    Routes: /health, /schema, /document/{id}/{connect,exists,
               initialize,snapshot,state,update,raw,active_peers,peer/{p},
               metadata,blame/{n},copy,wakeup,debug_dump_operations}.
               WS: binary Bebop dispatch + text ping→pong.
  bebop.go     FromPeer decoder + FromRemote encoders + init-request decoder.
  lorowasm/    the wazero spike (engine.go, host.go, jsheap.go, tests).
```

Route parity vs `durable_object.rs`: all listed routes exist; `state`/`update`
implement the base64-JSON `document_api.rs` contract; `copy` orchestrates
snapshot+initialize internally like `copy_handler`.

## Deliberate gaps (flagged, not bugs)

- **Auth**: JWT (`macro_sync_service_jwt`) verification is NOT implemented —
  `SYNC_INSECURE_AUTH=1` or `x-internal-auth-key` only. Rust checks
  `document_id`/`access_level` claims; the spike trusts `?user_id`.
- **`raw` (deep JSON value)**: `lorodoc_toJSON`/`getDeepValueWithID` exports
  exist but the returned JS object needs recursive heap→Go marshalling; 501.
- **snapshot `version_id` / `Frontiers::ID` state-only export**: needs the
  `{mode:"state-only", frontiers}` object plumbing; ignored for now.
- **`diff`, `get_path_to_container`, blame-from-CRDT**: blame endpoints work
  via `syncsvc_blame`; the wasm `lorodoc_diff` path is unwired.
- **Initial-sync-before-initialize**: Rust defers initial sync until the
  snapshot lands; the spike sends it immediately (empty doc).
- **`subscribe`/event callbacks into wasm** (e.g. `lorodoc_subscribe`,
  `ephemeralstorewasm_subscribe`) require calling *into* wasm from a host
  closure — not attempted; `__wbindgen_closure_wrapper*` return inert fns.
- **Flush is close-only**: `flush()` runs on last-socket-close/shutdown; the
  `SYNC_FLUSH_INTERVAL` periodic sweep is not yet scheduled.
- **Op-log byte parity**: `appendOpsTx` stores each update blob (Rust stores
  the same bytes in DO KV); `Operation{update,timestamp}` Bebop encoding of
  the op log is not replicated (we store raw `bytea`).
- **Websocket close codes**: Rust uses 1000/1008-style closes on auth/socket
  errors; spike closes on read error silently.
- Schema is created with `CREATE TABLE IF NOT EXISTS` in `ensureSchema` —
  move to `crates/macro_db_client/migrations` for production.

## Recommendation

**Primary: wazero + `loro_wasm_bg.wasm`** for the Go syncsvc, with the
`docEngine` interface (already in `engine.go`) as the seam.

Rationale: the spike proves the full document lifecycle; host-shim risk is
bounded (all 118 imports already implemented; new loro versions just add
detectable stubs); and the artifact is versioned via `loro-crdt` npm release,
which the repo already consumes (`packages/loro-mirror`).

Before production:

1. Pin the artifact (`loro-crdt` version ↔ `loro` Rust crate version must stay
   in lockstep — 1.16.x); check it into a pinned location or fetch in CI.
2. Close the gaps above — priority: real JWT auth, `toJSON`, state-only
   snapshot, periodic flush, wasm-side `subscribe` for change events.
3. Load-test: each doc session = one wasm instance (~MBs heap + JS heap Go
   objects); measure per-doc instantiation (~ms) and memory under N docs.
4. Add a chaos test: corrupt/malformed client updates (wasm traps → errors,
   verified via `Instance.call` recover).

**Fallbacks ranked**:

- **Rust sidecar** (keep `services/sync-service` in compose): zero porting
  risk, keeps Loro-native + identical bytes; but keeps Rust+workerd ops cost —
  the thing the plan is trying to remove. Best *short-term* answer if wazero
  hits a wall on an untested API (e.g. `subscribe`).
- **cgo + Rust FFI**: compile the loro crate to `libloro.a` + cbindgen.
  Removes the JS shim entirely, but reintroduces cgo, cross-compile pain, and
  a Rust build dep — the opposite of "pure Go" self-hosting. Only consider if
  wazero perf is unacceptable.
- **Pure-Go CRDT**: no mature Loro-compatible Go implementation exists
  (automerge-go is partial, yjs ports incompatible with Loro's encoding).
  Not viable for wire compat.

## Verification

```sh
export PATH=/tmp/go/bin:$PATH
go build ./internal/syncsvc/...          # PASS
go vet  ./internal/syncsvc/...          # PASS
go test ./internal/syncsvc/...          # PASS (bebop codec tests)

LORO_WASM_PATH=/path/to/loro_wasm_bg.wasm \
  go test ./internal/syncsvc/lorowasm -v # PASS — full doc round-trip
```

Reproduce the artifact: `npm pack loro-crdt && tar xzf loro-crdt-*.tgz
package/nodejs/loro_wasm_bg.wasm`.

Blocked in this environment: no local Postgres on :5432 → DB-backed HTTP/WS
paths compile but were not smoke-tested end-to-end; `just check` not run
(repo-wide recipe touches out-of-scope dirs).

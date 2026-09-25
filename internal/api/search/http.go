package search

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/events"
	pkgsearch "github.com/macro-inc/macro/pkg/search"
)

// Config carries the search-processing settings; it embeds the root config
// so package-level env additions live here, not in pkg/config.
type Config struct {
	config.Config
	// BackfillJobTTLSeconds bounds how long finished job snapshots stay
	// queryable (was backfill_job_ttl_seconds / DynamoDB TTL).
	BackfillJobTTLSeconds int `env:"SEARCH_BACKFILL_JOB_TTL_SECONDS" envDefault:"86400"`
	// VectorDimensions enables the pgvector embedding column when > 0.
	VectorDimensions int `env:"SEARCH_VECTOR_DIMENSIONS" envDefault:"0"`
	// Backfill page sizes (backfill_page_sizes in the Rust config).
	BackfillPageSize int `env:"SEARCH_BACKFILL_PAGE_SIZE" envDefault:"500"`
	// EmailThreadsPerMessage bounds thread ids per queue message
	// (DEFAULT_EMAIL_BATCH_SIZE in the Rust email source).
	EmailThreadsPerMessage int `env:"SEARCH_EMAIL_BATCH_SIZE" envDefault:"20"`
}

// Load parses environment variables into the search Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Deps wires the router + consumer.
type Deps struct {
	Config       Config
	Jobs         *BackfillJobs
	Orchestrator *Orchestrator
	Indexer      *Indexer
	Store        pkgsearch.SearchPort
	Publisher    NATSPublisher
}

// RegisterInternal mounts the internal-only routes; callers must wrap with
// auth.InternalMiddleware (the Rust service gated these behind InternalOnly).
// Each handler also re-checks the internal caller defensively, so a
// mis-mounted router fails closed rather than open.
func (d Deps) RegisterInternal(r chi.Router) {
	r.Delete("/delete/{document_id}", d.deleteDocument)
	r.Post("/extract_sync", d.extractSync)
	r.Route("/backfill", func(r chi.Router) {
		r.Post("/documents", d.backfillDocuments)
		r.Post("/chats", d.backfillChats)
		r.Post("/channels", d.backfillChannels)
		r.Post("/emails", d.backfillEmails)
		r.Post("/projects", d.backfillProjects)
		r.Post("/calls", d.backfillCalls)
		r.Post("/calendar-events", d.backfillCalendarEvents)
		r.Post("/properties", d.backfillProperties)
		r.Get("/{job_id}", d.jobStatus)
	})
}

// RegisterQuery mounts the authenticated search endpoint (new in the Go
// port: OpenSearch was queried out-of-band; the self-host build exposes the
// Postgres FTS index directly).
func (d Deps) RegisterQuery(r chi.Router) {
	r.Post("/search", d.query)
}

// ---------------------------------------------------------------------------
// /internal/delete/{document_id}
// ---------------------------------------------------------------------------

// requireInternal enforces the InternalOnly boundary on every internal
// handler (x-internal-auth-key, constant-time compared upstream).
func (d Deps) requireInternal(w http.ResponseWriter, r *http.Request) bool {
	caller, ok := auth.FromContext(r.Context())
	return ok && auth.RequireInternal(w, caller)
}

func (d Deps) deleteDocument(w http.ResponseWriter, r *http.Request) {
	if !d.requireInternal(w, r) {
		return
	}
	docID := chi.URLParam(r, "document_id")
	if docID == "" {
		httpx.Error(w, http.StatusBadRequest, "document_id required")
		return
	}
	if err := d.Store.Delete(r.Context(), pkgsearch.EntityDocument, docID); err != nil {
		slog.Error("search: delete document failed", "document_id", docID, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to delete document")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// /internal/extract_sync — republishes document sync events onto the bus
// (Kafka DocumentMacroEvent → events.Envelope on search.index).
// ---------------------------------------------------------------------------

type syncDocument struct {
	DocumentID        string  `json:"document_id"`
	DocumentVersionID *string `json:"document_version_id"`
	FileType          string  `json:"file_type"`
	Actor             *string `json:"actor"`
	OnBehalfOf        *string `json:"on_behalf_of"`
}

type extractSyncRequest struct {
	Documents []syncDocument `json:"documents"`
}

func (d Deps) extractSync(w http.ResponseWriter, r *http.Request) {
	if !d.requireInternal(w, r) {
		return
	}
	var req extractSyncRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	for _, doc := range req.Documents {
		if doc.DocumentID == "" {
			continue
		}
		env, err := events.New("document.sync_content_updated", "search",
			doc.DocumentID, 1, map[string]any{
				"document_id":         doc.DocumentID,
				"document_version_id": doc.DocumentVersionID,
				"file_type":           doc.FileType,
				"actor":               doc.Actor,
				"on_behalf_of":        doc.OnBehalfOf,
			})
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "failed to build event")
			return
		}
		if err := publishEnvelope(r.Context(), d.Publisher, doc.DocumentID, env); err != nil {
			slog.Error("search: failed to publish sync event", "document_id", doc.DocumentID, "err", err)
			httpx.Error(w, http.StatusInternalServerError, "failed to enqueue documents")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// /internal/backfill/{entity} → 202 + job id; GET /internal/backfill/{job_id}
// ---------------------------------------------------------------------------

type acceptedReceipt struct {
	JobID string `json:"job_id"`
}

func (d Deps) spawnBackfill(w http.ResponseWriter, r *http.Request, entity string,
	run func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error)) {
	if !d.requireInternal(w, r) {
		return
	}
	handle, err := d.Jobs.Start(context.Background(), entity)
	if err != nil {
		slog.Error("search: failed to allocate backfill job", "entity", entity, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "registry unavailable")
		return
	}
	slog.Info("search: spawning backfill", "job_id", handle.ID, "entity", entity)
	go func() {
		_, runErr := run(handle.Ctx, d.Orchestrator, handle.Progress)
		d.Jobs.Finish(context.Background(), handle.ID, handle.Ctx.Err() != nil, runErr)
		if runErr != nil {
			slog.Error("search: backfill failed", "job_id", handle.ID, "entity", entity, "err", runErr)
		} else {
			slog.Info("search: backfill completed", "job_id", handle.ID, "entity", entity)
		}
	}()
	httpx.WriteJSON(w, http.StatusAccepted, acceptedReceipt{JobID: handle.ID})
}

func (d Deps) jobStatus(w http.ResponseWriter, r *http.Request) {
	if !d.requireInternal(w, r) {
		return
	}
	snap, err := d.Jobs.Snapshot(r.Context(), chi.URLParam(r, "job_id"))
	if err != nil {
		slog.Error("search: failed to read backfill job snapshot", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "registry unavailable")
		return
	}
	if snap == nil {
		httpx.Error(w, http.StatusNotFound, "unknown job id")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, snap)
}

func (d Deps) backfillDocuments(w http.ResponseWriter, r *http.Request) {
	var req DocumentBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "documents", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillDocuments(ctx, req, p)
	})
}

func (d Deps) backfillChats(w http.ResponseWriter, r *http.Request) {
	var req ChatBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "chats", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillChats(ctx, req, p)
	})
}

func (d Deps) backfillChannels(w http.ResponseWriter, r *http.Request) {
	var req ChannelBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "channels", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillChannels(ctx, req, p)
	})
}

func (d Deps) backfillEmails(w http.ResponseWriter, r *http.Request) {
	var req EmailBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "emails", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillEmails(ctx, req, p)
	})
}

func (d Deps) backfillProjects(w http.ResponseWriter, r *http.Request) {
	var req ProjectBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "projects", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillProjects(ctx, req, p)
	})
}

func (d Deps) backfillCalls(w http.ResponseWriter, r *http.Request) {
	var req CallBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "calls", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillCalls(ctx, req, p)
	})
}

func (d Deps) backfillCalendarEvents(w http.ResponseWriter, r *http.Request) {
	var req CalendarEventBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "calendar-events", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillCalendarEvents(ctx, req, p)
	})
}

func (d Deps) backfillProperties(w http.ResponseWriter, r *http.Request) {
	var req PropertiesBackfillRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	d.spawnBackfill(w, r, "properties", func(ctx context.Context, o *Orchestrator, p *JobProgress) (BackfillReceipt, error) {
		return o.BackfillEntityProperties(ctx, req, p)
	})
}

// ---------------------------------------------------------------------------
// POST /search — keyword/semantic/hybrid query against the FTS index.
// ---------------------------------------------------------------------------

type queryRequest struct {
	Text        string    `json:"text"`
	EntityTypes []string  `json:"entity_types"`
	Mode        string    `json:"mode"` // "keyword" | "semantic" | "hybrid"
	Limit       int       `json:"limit"`
	Embedding   []float32 `json:"embedding"`
}

func (d Deps) query(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok || caller.UserID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req queryRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	q := pkgsearch.Query{
		Text:        req.Text,
		Embedding:   req.Embedding,
		OwnerID:     caller.UserID,
		EntityTypes: req.EntityTypes,
		Limit:       req.Limit,
	}
	switch req.Mode {
	case "semantic":
		q.Mode = pkgsearch.ModeSemantic
	case "hybrid":
		q.Mode = pkgsearch.ModeHybrid
	default:
		q.Mode = pkgsearch.ModeKeyword
	}
	hits, err := d.Store.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "search failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": hits})
}

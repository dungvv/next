package dss

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/objectstore"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// Caller re-exports the shared auth caller identity.
type Caller = auth.Caller

// internalUserID mirrors MACRO_INTERNAL_USER_ID.
const internalUserID = auth.InternalUserID

// Deps carries everything the dss router needs.
type Deps struct {
	Pool      *pgxpool.Pool
	Store     objectstore.Store
	DocBucket string              // DOCUMENT_STORAGE_BUCKET (was cloud-storage S3 bucket)
	NC        *nats.Conn          // nil → event publishing is a no-op
	JS        jetstream.JetStream // nil → event publishing is a no-op
	// DocumentPermissionJWTSecret is the HS256 secret for document
	// permission tokens (document_permission_jwt in Rust config). Empty
	// disables token minting.
	DocumentPermissionJWTSecret string
}

// Service implements the dss HTTP surface and the gql.Backend port.
type Service struct {
	q         *macrodb.Queries
	pool      *pgxpool.Pool
	store     objectstore.Store
	bucket    string
	nc        *nats.Conn
	js        jetstream.JetStream
	jwtSecret string
	access    *accessChecker
}

// NewService builds the service and (best effort) ensures the
// macro.documents JetStream event stream exists.
func NewService(ctx context.Context, d Deps) *Service {
	s := &Service{
		q:         macrodb.New(d.Pool),
		pool:      d.Pool,
		store:     d.Store,
		bucket:    d.DocBucket,
		nc:        d.NC,
		js:        d.JS,
		jwtSecret: d.DocumentPermissionJWTSecret,
		access:    &accessChecker{pool: d.Pool},
	}
	if d.JS != nil {
		if _, err := natsx.EnsureEventStream(ctx, d.JS, "macro.documents",
			[]string{"macro.documents.>"}); err != nil {
			slog.Warn("dss: ensure macro.documents stream failed", "err", err)
		}
	}
	return s
}

// callerFrom mirrors MacroAuthorizationExtractor<UserOrInternal>: any
// authenticated caller (user or internal) is accepted.
func callerFrom(r *http.Request) (Caller, bool) {
	return auth.FromContext(r.Context())
}

// Register mounts the document_storage_service routes on r. The Rust service
// mounted the same internal router at root, under /{version}, and under /dss;
// the combined binary mounts only under /dss (and /dss/{version}).
func (s *Service) Register(r chi.Router) {
	s.registerRoutes(r)
	// The Rust service also serves the whole surface under /{version}
	// (e.g. /dss/v1/documents). chi resolves literal segments before
	// wildcards, so real routes still win over the {version} branch.
	r.Route("/{version}", func(r chi.Router) {
		s.registerRoutes(r)
	})
}

func (s *Service) registerRoutes(r chi.Router) {
	// --- documents -------------------------------------------------------
	r.Route("/documents", func(r chi.Router) {
		r.Get("/", s.getUserDocuments)
		r.Post("/", s.createDocument)
		r.Get("/list", s.getDocumentList)
		r.Get("/starter_docs", s.getStarterDocs)
		r.Post("/initialize_user_documents", s.initializeUserDocuments)
		r.Post("/preview", s.getBatchPreview)

		// backend-owned creation flows
		r.Post("/create_markdown", s.createMarkdown)
		r.Post("/create_snippet", s.createSnippet)
		r.Post("/create_skill", s.createSkill)
		r.Post("/create_task", s.createTask)
		r.Get("/system_skills", s.getSystemSkills)

		// permissions_token
		r.Route("/permissions_token", func(r chi.Router) {
			r.Post("/{document_id}", s.createPermissionToken)
			r.Post("/validate", s.validatePermissionsToken)
		})

		r.Route("/{document_id}", func(r chi.Router) {
			r.Use(s.ensureDocumentExists)
			r.Get("/", s.getDocument)
			r.Patch("/", s.editDocument)
			r.Delete("/", s.deleteDocument)
			r.Put("/", s.saveDocument)
			r.Get("/permissions", s.getDocumentPermissions)
			r.Get("/access_level", s.getDocumentAccessLevel)
			r.Get("/location", s.getDocumentLocation)
			r.Get("/location_v3", s.getDocumentLocationV3)
			r.Get("/text", s.getDocumentText)
			r.Get("/views", s.getDocumentViews)
			r.Get("/export", s.exportDocument)
			r.Get("/processing", s.getDocumentProcessingResult)
			r.Get("/processing/{job_id}", s.getJobProcessingResult)
			r.Get("/full_pdf_modification_data", s.getFullPDFModificationData)
			r.Get("/key", s.getDocumentKey)
			r.Get("/short_id", s.getShortID)
			r.Get("/branch_name", s.getBranchName)
			r.Get("/github_prs", s.getGithubPRs)
			r.Get("/team_share", s.getTeamShare)
			r.Put("/team_share", s.putTeamShare)
			r.Get("/{document_version_id}", s.getDocumentVersion)
			r.Get("/{document_version_id}/key", s.getDocumentVersionKey)
			r.Put("/simple_save", s.simpleSave)
			r.Put("/presave", s.preSave)
			r.Put("/revert_delete", s.revertDeleteDocument)
			r.Delete("/permanent", s.permanentlyDeleteDocument)
		})
		// Rust also exposes PUT /presave/{document_id} (deprecated alias).
		r.Put("/presave/{document_id}", s.preSave)
	})

	// --- projects (basic ops) ---------------------------------------------
	r.Route("/projects", func(r chi.Router) {
		r.Get("/", s.getProjects)
		r.Post("/", s.createProject)
		r.Get("/pending", s.getPendingProjects)
		r.Post("/preview", s.getBatchProjectPreview)
		r.Route("/{id}", func(r chi.Router) {
			r.Use(s.ensureProjectExists)
			r.Get("/", s.getProject)
			r.Patch("/", s.editProject)
			r.Delete("/", s.deleteProject)
			r.Get("/content", s.getProjectContent)
			r.Get("/permissions", s.getProjectPermissions)
			r.Get("/access_level", s.getProjectAccessLevel)
			r.Put("/revert_delete", s.revertDeleteProject)
		})
	})

	// --- pins --------------------------------------------------------------
	r.Route("/pins", func(r chi.Router) {
		r.Get("/", s.getPins)
		r.Post("/{pinned_item_id}", s.addPin)
		r.Delete("/{pinned_item_id}", s.removePin)
		r.Patch("/", s.reorderPins)
	})

	// --- history -----------------------------------------------------------
	r.Route("/history", func(r chi.Router) {
		r.Get("/", s.getHistory)
		r.Post("/{item_type}/{item_id}", s.upsertHistory)
		r.Delete("/{item_type}/{item_id}", s.deleteHistory)
	})

	// --- recents -----------------------------------------------------------
	r.Route("/recents", func(r chi.Router) {
		r.Get("/deleted", s.recentlyDeleted)
	})

	// --- user_document_view_location ---------------------------------------
	r.Route("/user_document_view_location", func(r chi.Router) {
		r.Get("/{document_id}", s.getUserDocumentViewLocation)
		r.Post("/{document_id}", s.upsertUserDocumentViewLocation)
		r.Delete("/{document_id}", s.deleteUserDocumentViewLocation)
	})

	// --- entity ------------------------------------------------------------
	r.Route("/entity", func(r chi.Router) {
		r.Get("/{entity_type}/{entity_id}/permissions", s.getEntityPermission)
	})

	// --- items (soup REST + item id helpers) --------------------------------
	r.Route("/items", func(r chi.Router) {
		r.Post("/validate_item_ids", s.validateItemIDs)
		r.Post("/item_ids", s.getItemIDs)
		r.Get("/soup", s.getSoup) // minimal unfiltered soup page
		r.Post("/soup", s.postSoup)
		r.Post("/soup/ast", s.postSoupAST)
		s.mountGraphQL(r)
	})

	// --- instructions ------------------------------------------------------
	r.Route("/instructions", func(r chi.Router) {
		r.Post("/", s.createInstructions)
		r.Get("/", s.getInstructions)
	})

	// --- saved_views --------------------------------------------------------
	r.Route("/saved_views", func(r chi.Router) {
		r.Get("/", s.getSavedViews)
		r.Post("/", s.createSavedView)
		r.Post("/exclude_default", s.excludeDefaultSavedView)
		r.Delete("/{saved_view_id}", s.deleteSavedView)
		r.Patch("/{saved_view_id}", s.patchSavedView)
	})

	// --- internal (service-to-service subset) -------------------------------
	// Rust guards this router with MacroAuthorizationExtractor<InternalOnly>:
	// only internal-key callers reach it (a forwarded x-user-id is allowed —
	// handlers still run their own per-document access checks).
	r.Route("/internal", func(r chi.Router) {
		r.Use(requireInternalCaller)
		r.Get("/documents/{document_id}", s.getDocument)
		r.Get("/documents/{document_id}/basic", s.getDocumentBasic)
		r.Get("/documents/{document_id}/text", s.getDocumentText)
		r.Get("/documents/{document_id}/access_level", s.getDocumentAccessLevel)
		r.Get("/documents/{document_id}/permissions", s.getDocumentPermissions)
		r.Get("/documents/list_with_access", s.listDocumentsWithAccess)
		r.Post("/validate_item_ids", s.validateItemIDs)
		r.Post("/item_ids", s.getItemIDs)
		r.Get("/documents/{document_id}/location", s.getDocumentLocation)
		r.Get("/documents/{document_id}/location_v3", s.getDocumentLocationV3)
	})

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("healthy"))
	})
}

// requireInternalCaller mirrors the InternalOnly extractor on the Rust
// internal router: the request must have authenticated with the internal
// API key. User-JWT callers are rejected outright — internal metadata
// endpoints are not a second path around document authorization.
func requireInternalCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := auth.FromContext(r.Context())
		if !ok || !c.Internal {
			writeErr(w, http.StatusUnauthorized, "unauthorized: internal only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mountGraphQL installs the gqlgen handler at /soup/graphql (mirroring
// graphql_soup.rs GRAPHQL_PATH under the /items nest).
func (s *Service) mountGraphQL(r chi.Router) {
	h := newGraphQLHandler(s)
	r.Handle("/soup/graphql", h)
	r.Handle("/soup/graphql/ws", h) // subscriptions over websocket
}

// helper: write GenericResponse data envelope.
func writeData(w http.ResponseWriter, status int, data any) {
	httpx.WriteJSON(w, status, GenericResponse{Error: false, Data: mustJSON(data)})
}

func writeDataErr(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, GenericResponse{Error: true, Message: msg})
}

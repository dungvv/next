package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/dungvv/next/services/static-file-service-go/internal/authz"
	"github.com/dungvv/next/services/static-file-service-go/internal/config"
	"github.com/dungvv/next/services/static-file-service-go/internal/dynamodb"
	"github.com/dungvv/next/services/static-file-service-go/internal/s3client"
)

// Mirrors MAX_REQUEST_SIZE (bytes).
const maxRequestSize = 4096

// Server mirrors AppState.
type Server struct {
	Config    *config.Config
	Metadata  *dynamodb.Client
	Storage   *s3client.Client
	Authorize *authz.Authorizer
}

// Handler mirrors api::setup_and_serve's router: `/api` (CDN-routed) and
// `/internal` mounts of the file router plus health, swagger UI and the
// OpenAPI document, behind CORS and the request-size limit.
func (s *Server) Handler() http.Handler {
	fileRouter := chi.NewRouter()
	fileRouter.Use(s.Authorize.Middleware)
	fileRouter.Get("/file/metadata/{file_id}", s.GetMetadata)
	fileRouter.Get("/file/{file_id}/presigned-url", s.GetPresignedURL)
	fileRouter.Put("/file", s.PutPresignedURL)
	fileRouter.Delete("/file/{file_id}", s.DeleteFile)
	fileRouter.Post("/file/bulk-delete", s.BulkDeleteFile)

	root := chi.NewRouter()
	root.Use(corsMiddleware)
	root.Use(limitBody)
	root.Mount("/api", func() http.Handler {
		r := chi.NewRouter()
		r.Mount("/", fileRouter)
		r.Get("/health", s.Health)
		return r
	}())
	root.Mount("/internal", fileRouter)
	root.Get("/api/docs", swaggerUIHandler)
	root.Get("/api/docs/", swaggerUIHandler)
	root.Get("/api/api-doc/openapi.json", openAPIHandler)
	return root
}

// limitBody mirrors RequestBodyLimitLayer(MAX_REQUEST_SIZE).
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestSize {
			http.Error(w, "length limit exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
		next.ServeHTTP(w, r)
	})
}

var allowedOrigins = []string{
	"http://localhost:5173",
	"https://dashboarddev.macro.com",
	"https://dashboard.macro.com",
	"http://host.local:3000",
	"https://dev.macro.com",
	"https://staging.macro.com",
	"https://www.macro.com",
	"https://macro.com",
	"http://tauri.localhost",
	"tauri://localhost",
	"https://apollo-testing.macro.com",
}

var corsAllowedHeaders = []string{
	"Authorization",
	"Content-Type",
	"x-permissions-token",
	"traceparent",
	"tracestate",
	"x-email-link-id",
}

// isAllowedOrigin mirrors macro_cors::is_allowed_origin.
func isAllowedOrigin(origin string) bool {
	list := allowedOrigins
	if env := strings.TrimSpace(envString("ALLOWED_ORIGINS")); env != "" {
		list = nil
		for _, o := range strings.Split(env, ",") {
			list = append(list, strings.TrimSpace(o))
		}
	}
	for _, o := range list {
		if o == origin {
			return true
		}
	}
	if strings.HasPrefix(origin, "https://") && strings.HasSuffix(origin, "preview.macro.com") {
		return true
	}
	if rest, ok := strings.CutPrefix(origin, "http://"); ok {
		if host, portStr, ok := strings.Cut(rest, ":"); ok {
			if host == "localhost" || strings.HasSuffix(host, ".localhost") {
				if port, err := strconv.Atoi(portStr); err == nil {
					return (port >= 3000 && port <= 3999) || (port >= 20000 && port <= 60000)
				}
			}
		}
	}
	return false
}

func envString(name string) string {
	return os.Getenv(name)
}

// corsMiddleware mirrors macro_cors::cors_layer: credentials allowed, fixed
// header/method allow-lists, predicate-based origin matching.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", strings.Join(corsAllowedHeaders, ", "))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

package mcp

import (
	"net/http"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/macro-inc/macro/internal/api/mcpauth"
	"github.com/macro-inc/macro/pkg/config"
)

// Config carries the mcp_service settings; it embeds the root config so
// package-level env additions live here, not in pkg/config.
type Config struct {
	config.Config
	// ItemBaseURL is APP_BASE_URL — the public web app URL used to build item
	// links and the favicon URL in MCP responses.
	ItemBaseURL string `env:"APP_BASE_URL" envDefault:""`
	// MetadataPath is the protected-resource metadata path advertised in
	// WWW-Authenticate challenges (default
	// "/mcp/.well-known/oauth-protected-resource").
	MetadataPath string `env:"MCP_METADATA_PATH" envDefault:""`
}

// Load parses environment variables into the mcp Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Deps wires the router.
type Deps struct {
	Catalog   ToolCatalog
	Caller    ToolCaller
	Validator mcpauth.TokenValidator
	Config    Config
}

// Register mounts the streamable MCP endpoint at the mount root (the caller
// mounts this router at `/mcp`, matching the Rust gateway prefix). Every
// method is Bearer-guarded; CORS mirrors mcp_cors_layer so browser clients
// can complete the OAuth dance.
func (d Deps) Register(r chi.Router) {
	if d.Catalog == nil {
		d.Catalog = StaticCatalog{}
	}
	if d.Caller == nil {
		d.Caller = NotImplementedCaller{}
	}
	metadataPath := d.Config.MetadataPath
	if metadataPath == "" {
		metadataPath = "/mcp/.well-known/oauth-protected-resource"
	}

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		userID, _ := mcpauth.CallerFromContext(req.Context())
		return newServer(req.Context(), d.Catalog, d.Caller, userID, d.Config.ItemBaseURL)
	}, &mcp.StreamableHTTPOptions{
		// Mirrors StreamableHttpServerConfig: stateful_mode = false,
		// json_response = true.
		Stateless:    true,
		JSONResponse: true,
	})

	guarded := corsMiddleware()(
		mcpauth.BearerMiddleware(d.Validator, metadataPath)(handler),
	)
	r.Handle("/", guarded)
	r.Handle("/*", guarded)
}

// corsMiddleware mirrors the Rust mcp_cors_layer: mirror request origin,
// credentials, MCP headers; OPTIONS preflights short-circuit to 204 before
// auth.
func corsMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", strings.Join([]string{
					"content-type", "authorization", "mcp-protocol-version", "mcp-session-id",
				}, ", "))
				w.Header().Set("Access-Control-Max-Age", "3600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if origin != "" {
				w.Header().Set("Access-Control-Expose-Headers", "mcp-session-id, www-authenticate")
			}
			next.ServeHTTP(w, r)
		})
	}
}

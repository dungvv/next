package dss

// graphql.go wires the gqlgen executable schema into the chi router. The
// schema is the verbatim async-graphql SDL exported by the Rust service
// (static_assets/schema.graphql). Implemented fields:
//
//	Query.user                        → GraphqlUser{ID: <authenticated user>}
//	GraphqlUser.soup                  → document/project/chat page
//	GraphqlUser.emailLabels/emailLinks → empty lists (email service not ported)
//
// All mutations and subscriptions return a GraphQL "not implemented" error
// rather than panicking. See internal/api/dss/gql/resolver_gen.go for the
// complete stub list.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/macro-inc/macro/internal/api/dss/gql"
)

// newGraphQLHandler builds the /items/soup/graphql handler. GET serves
// GraphiQL (mirroring the Rust graphiql route); POST serves the API. The
// /ws sibling route handles subscriptions (they currently return
// not-implemented errors at the resolver level).
func newGraphQLHandler(s *Service) http.Handler {
	srv := handler.New(gql.NewExecutableSchema(gql.Config{
		Resolvers: &gql.Resolver{Backend: s},
	}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	// Websocket transport for the subscription route; resolvers return
	// explicit "not implemented" errors.
	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
	})
	srv.Use(extension.Introspection{})
	srv.SetErrorPresenter(sanitizeGraphQLError)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("query") == "" {
			playground.Handler("soup", r.URL.Path).ServeHTTP(w, r)
			return
		}
		srv.ServeHTTP(w, r)
	})
}

// sanitizeGraphQLError is the gqlgen error presenter: it decides what a
// resolver or runtime error may reveal to the client. The default presenter
// forwards the raw message — which leaks backend details (query errors,
// internal wiring). Errors that already carry a `code` extension (gqlgen
// validation/parse errors and resolver-marked client errors) pass through,
// as do the explicit "not implemented" stub messages so clients can
// feature-detect. Everything else is logged server-side and replaced with a
// generic internal-error message.
func sanitizeGraphQLError(ctx context.Context, err error) *gqlerror.Error {
	gqlErr := graphql.DefaultErrorPresenter(ctx, err)
	if gqlErr.Extensions != nil {
		if _, ok := gqlErr.Extensions["code"]; ok {
			return gqlErr
		}
	}
	msg := gqlErr.Message
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not implemented"),
		strings.Contains(lower, "unauthenticated"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "not found"):
		return gqlErr
	}
	slog.Error("dss: graphql resolver error (sanitized)",
		"path", graphql.GetPath(ctx), "err", err)
	return &gqlerror.Error{
		Message:    "internal server error",
		Path:       gqlErr.Path,
		Extensions: map[string]any{"code": "INTERNAL"},
	}
}

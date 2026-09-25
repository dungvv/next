package gql

import "context"

// Backend is the narrow port the implemented resolvers call into. The dss
// service satisfies it; keeping it here avoids an import cycle (dss imports
// gql for the generated code).
type Backend interface {
	// ViewerUserID returns the authenticated user's macro id
	// ("macro|<email>") for the request context, or "" when absent.
	ViewerUserID(ctx context.Context) string

	// SoupPage returns a page of soup entities for the viewer.
	SoupPage(ctx context.Context, input SoupInput) (*SoupPageResult, error)

	// EntityByID loads one soup-visible entity for the viewer.
	EntityByID(ctx context.Context, entityType GraphqlSoupEntityType, id string) (GraphqlSoupEntity, error)
}

// SoupPageResult is the resolved soup page plus the opaque next cursor.
type SoupPageResult struct {
	Items      []GraphqlSoupEntity
	NextCursor *string
}

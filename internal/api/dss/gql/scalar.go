package gql

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/99designs/gqlgen/graphql"
)

// JSON is the Go binding for the `JSON` and `SoupCacheProjection` custom
// scalars: a raw, already-encoded JSON value.
type JSON []byte

// MarshalGQL writes the raw JSON value verbatim.
func (j JSON) MarshalGQL(w io.Writer) {
	if len(j) == 0 {
		_, _ = io.WriteString(w, "null")
		return
	}
	_, _ = w.Write(j)
}

// UnmarshalGQL re-encodes the decoded input value back to raw JSON.
func (j *JSON) UnmarshalGQL(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON scalar: %w", err)
	}
	*j = raw
	return nil
}

var _ = graphql.MarshalAny // keep the import used even if helpers change

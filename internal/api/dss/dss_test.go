package dss

import (
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRegisterRoutesPanicsOnConflict exercises Service.Register on a fresh
// chi router: conflicting wildcard segments panic at registration time, so a
// clean run proves the mounted tree is well-formed.
func TestRegisterRoutesPanicsOnConflict(t *testing.T) {
	s := &Service{}
	r := chi.NewRouter()
	s.Register(r)
}

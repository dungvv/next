// Package httpx holds small helpers for JSON request/response handling used
// across the api sub-routers.
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorResponse mirrors model::response::ErrorResponse.
type ErrorResponse struct {
	Message string `json:"message"`
}

// EmptyResponse mirrors model::response::EmptyResponse.
type EmptyResponse struct{}

// WriteJSON serializes v with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes a plain-text error body (axum's `(status, &str)` responses).
func Error(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

// ErrorJSON writes an ErrorResponse JSON body.
func ErrorJSON(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Message: msg})
}

// DecodeJSON decodes a JSON request body; on failure it writes a 400 and
// returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		ErrorJSON(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// DefaultBodyLimitBytes mirrors axum's DefaultBodyLimit (2 MiB) — the cap
// the Rust services applied to request bodies unless overridden.
const DefaultBodyLimitBytes int64 = 2 << 20

// MaxBody returns middleware capping the request body at limit bytes via
// http.MaxBytesReader. Handlers decoding past the cap get a read error; the
// response is written as 413 by handlers that check for it, or surfaces as a
// decode failure.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

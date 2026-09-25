package dcs

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// Response conventions in Rust DCS are mixed:
//   - chat-crate handlers (crates/chat router) return plain Json(...) bodies;
//     errors are (status, "plain text") — see ChatErr::into_response.
//   - DCS-native handlers (history, stream, misc) return plain Json bodies;
//     errors are Json({"error": msg}) or (status, "plain text") per handler.
//
// writeJSON emits the plain body; writeTextErr / writeObjErr cover the two
// error shapes. Handlers pick the shape their Rust counterpart used.

// writeJSON writes a plain JSON body (axum Json(v)).
func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}

// writeTextErr writes a plain-text error body, mirroring axum
// `(StatusCode, String)` / ChatErr::into_response.
func writeTextErr(w http.ResponseWriter, status int, msg string) {
	httpx.Error(w, status, msg)
}

// writeObjErr writes {"error": msg}, mirroring the DCS-native handlers that
// return Json(serde_json::json!({"error": ...})).
func writeObjErr(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, map[string]string{"error": msg})
}

// ChatErr mirrors chat::domain::models::ChatErr for HTTP mapping.
type ChatErr struct {
	Kind chatErrKind
	Msg  string
	Err  error
}

type chatErrKind int

const (
	errInternal chatErrKind = iota
	errNotFound
	errNoAccess
	errBadRequest
	errConflict
	errNotImplemented
)

func (e *ChatErr) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *ChatErr) Unwrap() error { return e.Err }

func errf(kind chatErrKind, format string, args ...any) *ChatErr {
	return &ChatErr{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

func wrapErr(kind chatErrKind, msg string, err error) *ChatErr {
	return &ChatErr{Kind: kind, Msg: msg, Err: err}
}

// writeChatErr maps a ChatErr to (status, text) mirroring
// crates/chat/src/inbound/error_response.rs — the chat-crate routes respond
// with a bare status + text body, not a JSON envelope.
func writeChatErr(w http.ResponseWriter, err error) {
	var ce *ChatErr
	if !errors.As(err, &ce) {
		slog.Error("dcs: internal error", "err", err)
		writeTextErr(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	switch ce.Kind {
	case errNotFound:
		writeTextErr(w, http.StatusNotFound, "Not found")
	case errNoAccess:
		writeTextErr(w, http.StatusForbidden, "Forbidden")
	case errBadRequest:
		writeTextErr(w, http.StatusBadRequest, "Bad request")
	case errConflict:
		writeTextErr(w, http.StatusConflict, "Conflict")
	case errNotImplemented:
		writeTextErr(w, http.StatusNotImplemented, firstNonEmpty(ce.Msg, "Not implemented"))
	default:
		slog.Error("dcs: internal error", "err", ce)
		writeTextErr(w, http.StatusInternalServerError, "Internal server error")
	}
}

// writeDcsChatErr maps a ChatErr for the DCS-native handlers that return
// {"error": msg} bodies (history, batch messages).
func writeDcsChatErr(w http.ResponseWriter, err error) {
	var ce *ChatErr
	if !errors.As(err, &ce) {
		slog.Error("dcs: internal error", "err", err)
		writeObjErr(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	switch ce.Kind {
	case errNotFound:
		writeObjErr(w, http.StatusNotFound, firstNonEmpty(ce.Msg, "Not found"))
	case errNoAccess:
		writeObjErr(w, http.StatusForbidden, firstNonEmpty(ce.Msg, "Access denied to chat"))
	case errBadRequest:
		writeObjErr(w, http.StatusBadRequest, ce.Msg)
	case errConflict:
		writeObjErr(w, http.StatusConflict, ce.Msg)
	default:
		slog.Error("dcs: internal error", "err", ce)
		writeObjErr(w, http.StatusInternalServerError, "Internal server error")
	}
}

func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

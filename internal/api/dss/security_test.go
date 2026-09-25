package dss

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

// validBomSHA guards the content-addressed key that gets presigned for docx
// BOM parts — traversal, separators, and absolute-style keys must be
// rejected so a stored part can never mint a presigned URL for an object
// outside its slot.
func TestValidBomSHA(t *testing.T) {
	good := []string{
		"a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", // sha1 hex
		"abc-DEF_123.txt",
		"0",
	}
	for _, s := range good {
		if !validBomSHA(s) {
			t.Errorf("valid sha %q rejected", s)
		}
	}
	bad := []string{
		"",
		"../secret",
		"foo/../bar",
		"/etc/passwd",
		"owner/other-doc/3", // cross-document prefix
		".hidden",
		"a b",
		"a/b",
		"a\\b",
		"a\x00b",
		fmt.Sprintf("%257s", "a"), // overlong
	}
	for _, s := range bad {
		if validBomSHA(s) {
			t.Errorf("invalid sha %q accepted", s)
		}
	}
}

// validBomPath guards the caller-supplied zip-internal name stored in
// BomPart.path and echoed in location responses.
func TestValidBomPath(t *testing.T) {
	good := []string{"word/document.xml", "a/b/c.xml", "_rels/.rels", "[Content_Types].xml"}
	for _, p := range good {
		if !validBomPath(p) {
			t.Errorf("valid bom path %q rejected", p)
		}
	}
	bad := []string{
		"",
		"/etc/passwd",
		"../escape",
		"a/../../b",
		"a\\..\\b",
		"a\x00b",
	}
	for _, p := range bad {
		if validBomPath(p) {
			t.Errorf("invalid bom path %q accepted", p)
		}
	}
}

// sanitizeGraphQLError must not leak resolver/DB internals to clients while
// still passing through validation errors and deliberate resolver messages.
func TestSanitizeGraphQLError(t *testing.T) {
	ctx := context.Background()

	// A coded gqlgen error (validation, parse) passes through untouched.
	in := &gqlerror.Error{
		Message:    "field `x` not defined",
		Extensions: map[string]any{"code": "GRAPHQL_VALIDATION_FAILED"},
	}
	if got := sanitizeGraphQLError(ctx, in); got.Message != in.Message {
		t.Fatalf("coded error rewritten: %q", got.Message)
	}

	// Deliberate resolver messages pass through.
	for _, msg := range []string{
		"mutation not implemented",
		"unauthenticated",
		"document not found",
	} {
		got := sanitizeGraphQLError(ctx, errors.New(msg))
		if got.Message != msg {
			t.Fatalf("safe message %q rewritten to %q", msg, got.Message)
		}
	}

	// Raw internal errors are replaced with a generic message.
	secret := "pq: relation \"SecretTable\" does not exist at character 42"
	got := sanitizeGraphQLError(ctx, errors.New(secret))
	if got.Message == secret {
		t.Fatal("internal error leaked to graphql client")
	}
	if got.Message == "" || got.Extensions["code"] != "INTERNAL" {
		t.Fatalf("sanitized error malformed: %+v", got)
	}
}

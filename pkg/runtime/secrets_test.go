package runtime

import (
	"context"
	"testing"
)

func TestEnvAllowlist_Filter(t *testing.T) {
	al := NewEnvAllowlist("ANTHROPIC_API_KEY", "MACRO_*")
	allowed, dropped := al.Filter(map[string]string{
		"ANTHROPIC_API_KEY":   "k",
		"MACRO_SESSION_TOKEN": "t",
		"AWS_SECRET":          "nope",
		"PATH":                "nope",
	})
	if len(allowed) != 2 || allowed["ANTHROPIC_API_KEY"] != "k" || allowed["MACRO_SESSION_TOKEN"] != "t" {
		t.Fatalf("allowed = %v", allowed)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEnvSecrets_ResolvePrefix(t *testing.T) {
	t.Setenv("AGENT_SECRET_FOO", "prefixed")
	t.Setenv("BAR", "bare")
	s := EnvSecrets{Prefix: "AGENT_SECRET_"}
	if v, err := s.Resolve(context.Background(), "FOO"); err != nil || v != "prefixed" {
		t.Fatalf("prefixed resolve: %v %v", v, err)
	}
	if v, err := s.Resolve(context.Background(), "BAR"); err != nil || v != "bare" {
		t.Fatalf("bare fallback: %v %v", v, err)
	}
	if _, err := s.Resolve(context.Background(), "MISSING"); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

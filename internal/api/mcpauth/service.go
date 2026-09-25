package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service is the MCP OAuth broker (domain::service::McpAuthProxyService).
type Service struct {
	inflight  InflightAuthStore
	provider  OAuthProvider
	publicURL string
	// allowedRedirects is the MCP_ALLOWED_REDIRECT_URIS allowlist. The Rust
	// service accepted any https redirect_uri; the Go port exact-matches
	// non-loopback redirect_uris against this set when it is configured.
	allowedRedirects map[string]struct{}
}

// NewService builds the broker. `publicURL` is MCP_PUBLIC_URL — the external
// base URL these endpoints are reachable at. `allowedRedirectURIs` is the
// MCP_ALLOWED_REDIRECT_URIS allowlist (loopback http redirects are always
// allowed for local MCP clients).
func NewService(publicURL string, inflight InflightAuthStore, provider OAuthProvider, allowedRedirectURIs []string) *Service {
	s := &Service{
		inflight:         inflight,
		provider:         provider,
		publicURL:        strings.TrimRight(publicURL, "/"),
		allowedRedirects: map[string]struct{}{},
	}
	for _, u := range allowedRedirectURIs {
		if u = strings.TrimSpace(u); u != "" {
			s.allowedRedirects[u] = struct{}{}
		}
	}
	if len(s.allowedRedirects) == 0 {
		slog.Warn("mcpauth: MCP_ALLOWED_REDIRECT_URIS unset — any https redirect_uri is accepted (Rust parity); configure the allowlist to restrict the OAuth broker")
	}
	return s
}

// ---------------------------------------------------------------------------
// typed errors (domain::service error enums)
// ---------------------------------------------------------------------------

// StartAuthorizationError mirrors StartAuthorizationError.
type StartAuthorizationError struct{ Kind string }

func (e *StartAuthorizationError) Error() string {
	switch e.Kind {
	case "unsupported_response_type":
		return "unsupported response_type"
	case "unsupported_code_challenge_method":
		return "unsupported code_challenge_method"
	case "invalid_redirect_uri":
		return "redirect_uri must be https or a loopback address"
	case "inflight_store":
		return "failed to persist inflight auth state"
	default:
		return "failed to construct authorize URL"
	}
}

var (
	errStartResponseType    = &StartAuthorizationError{"unsupported_response_type"}
	errStartChallengeMethod = &StartAuthorizationError{"unsupported_code_challenge_method"}
	errStartRedirectURI     = &StartAuthorizationError{"invalid_redirect_uri"}
	errStartInflight        = &StartAuthorizationError{"inflight_store"}
	errStartAuthorizeURL    = &StartAuthorizationError{"construct_authorize_url"}
)

// CompleteCallbackError mirrors CompleteCallbackError.
type CompleteCallbackError struct{ Kind string }

func (e *CompleteCallbackError) Error() string { return e.Kind }

var (
	errCallbackMissingState   = &CompleteCallbackError{"missing_state"}
	errCallbackMissingCode    = &CompleteCallbackError{"missing_code"}
	errCallbackUnknownSession = &CompleteCallbackError{"unknown_or_expired_session"}
	errCallbackInflight       = &CompleteCallbackError{"inflight_store"}
	errCallbackExchange       = &CompleteCallbackError{"authorization_code_exchange_failed"}
)

// TokenExchangeError mirrors TokenExchangeError.
type TokenExchangeError struct{ Kind string }

func (e *TokenExchangeError) Error() string { return e.Kind }

var (
	errTokenUnsupportedGrant = &TokenExchangeError{"unsupported_grant_type"}
	errTokenCodeRequired     = &TokenExchangeError{"code_required"}
	errTokenInvalidCode      = &TokenExchangeError{"invalid_or_expired_code"}
	errTokenRedirectMismatch = &TokenExchangeError{"redirect_uri_mismatch"}
	errTokenRedirectRequired = &TokenExchangeError{"redirect_uri_required"}
	errTokenClientRequired   = &TokenExchangeError{"client_id_required"}
	errTokenClientMismatch   = &TokenExchangeError{"client_id_mismatch"}
	errTokenVerifierRequired = &TokenExchangeError{"code_verifier_required"}
	errTokenPKCE             = &TokenExchangeError{"pkce_verification_failed"}
	errTokenRefreshRequired  = &TokenExchangeError{"refresh_token_required"}
	errTokenInflight         = &TokenExchangeError{"inflight_store"}
	errTokenRefreshFailed    = &TokenExchangeError{"refresh_failed"}
)

// ---------------------------------------------------------------------------
// discovery metadata
// ---------------------------------------------------------------------------

// MCPResourceURL mirrors mcp_resource_url: append /mcp unless already there.
func MCPResourceURL(publicURL string) string {
	base := strings.TrimRight(publicURL, "/")
	if strings.HasSuffix(base, "/mcp") {
		return base
	}
	return base + "/mcp"
}

// AuthorizationServerMetadata mirrors authorization_server_metadata.
func (s *Service) AuthorizationServerMetadata() map[string]any {
	base := s.publicURL
	return map[string]any{
		"issuer":                           base,
		"authorization_endpoint":           base + "/authorize",
		"token_endpoint":                   base + "/token",
		"registration_endpoint":            base + "/register",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	}
}

// ProtectedResourceMetadata mirrors protected_resource_metadata.
func (s *Service) ProtectedResourceMetadata() map[string]any {
	base := s.publicURL
	return map[string]any{
		"resource":              MCPResourceURL(base),
		"authorization_server":  base,
		"authorization_servers": []string{base},
	}
}

// RegisterClient mirrors register_client: dynamic registration for public
// MCP clients — a fresh client id, no secret.
func (s *Service) RegisterClient(body map[string]any) map[string]any {
	clientID := uuid.NewString()
	clientName, _ := body["client_name"].(string)
	if clientName == "" {
		clientName = "mcp-client"
	}
	slog.Info("mcpauth: dynamic client registration", "client_id", clientID, "client_name", clientName)
	redirectURIs := body["redirect_uris"]
	if redirectURIs == nil {
		redirectURIs = []any{}
	}
	return map[string]any{
		"client_id":                  clientID,
		"client_name":                clientName,
		"redirect_uris":              redirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
}

// ---------------------------------------------------------------------------
// flows
// ---------------------------------------------------------------------------

// StartAuthorization mirrors start_authorization: validates the request,
// stores the pending session, returns the upstream authorize URL.
func (s *Service) StartAuthorization(ctx context.Context, params AuthorizeRequest) (string, error) {
	if params.ResponseType != "code" {
		return "", errStartResponseType
	}
	if params.CodeChallengeMethod != "S256" {
		return "", errStartChallengeMethod
	}
	if !s.isAllowedRedirectURI(params.RedirectURI) {
		return "", errStartRedirectURI
	}
	sessionID := uuid.NewString()
	if err := s.inflight.InsertPending(ctx, sessionID, PendingAuthorization{
		CodeChallenge:     params.CodeChallenge,
		ClientState:       params.State,
		ClientRedirectURI: params.RedirectURI,
		ClientID:          params.ClientID,
	}); err != nil {
		return "", errStartInflight
	}
	url, err := s.provider.ConstructAuthorizeURL(ctx, sessionID)
	if err != nil {
		return "", errStartAuthorizeURL
	}
	return url, nil
}

// CompleteCallback mirrors complete_callback: redeems the upstream code,
// mints a broker code, returns the client loopback redirect URL.
func (s *Service) CompleteCallback(ctx context.Context, params CallbackRequest) (string, error) {
	if params.State == nil {
		return "", errCallbackMissingState
	}
	sessionID := strings.Trim(*params.State, `"`)

	pending, err := s.inflight.TakePending(ctx, sessionID)
	if err != nil {
		return "", errCallbackInflight
	}
	if pending == nil {
		return "", errCallbackUnknownSession
	}

	if params.Error != nil {
		slog.Warn("mcpauth: upstream oauth returned error",
			"session", sessionID, "error", *params.Error)
		redirect := fmt.Sprintf("%s?error=%s&state=%s",
			pending.ClientRedirectURI,
			encodeURIComponent(*params.Error),
			encodeURIComponent(pending.ClientState))
		if params.ErrorDescription != nil {
			redirect += "&error_description=" + encodeURIComponent(*params.ErrorDescription)
		}
		return redirect, nil
	}

	if params.Code == nil {
		return "", errCallbackMissingCode
	}
	tokens, err := s.provider.ExchangeAuthorizationCode(ctx, *params.Code)
	if err != nil {
		return "", errCallbackExchange
	}

	var expiresAt time.Time
	if tokens.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	issuedCode := uuid.NewString()
	if err := s.inflight.InsertIssued(ctx, issuedCode, IssuedAuthorizationCode{
		AccessToken:          tokens.AccessToken,
		RefreshToken:         tokens.RefreshToken,
		CodeChallenge:        pending.CodeChallenge,
		RedirectURI:          pending.ClientRedirectURI,
		ClientID:             pending.ClientID,
		AccessTokenExpiresAt: expiresAt,
	}); err != nil {
		return "", errCallbackInflight
	}
	return fmt.Sprintf("%s?code=%s&state=%s",
		pending.ClientRedirectURI,
		encodeURIComponent(issuedCode),
		encodeURIComponent(pending.ClientState)), nil
}

// ExchangeToken mirrors exchange_token.
func (s *Service) ExchangeToken(ctx context.Context, params TokenRequest) (*TokenResponse, error) {
	switch params.GrantType {
	case "authorization_code":
		return s.exchangeAuthorizationCode(ctx, params)
	case "refresh_token":
		return s.refreshTokenExchange(ctx, params)
	default:
		return nil, errTokenUnsupportedGrant
	}
}

func (s *Service) refreshTokenExchange(ctx context.Context, params TokenRequest) (*TokenResponse, error) {
	if params.RefreshToken == nil {
		return nil, errTokenRefreshRequired
	}
	tokens, err := s.provider.RefreshAccessToken(ctx, *params.RefreshToken)
	if err != nil {
		return nil, errTokenRefreshFailed
	}
	expiresIn := tokens.ExpiresIn
	return &TokenResponse{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    &expiresIn,
	}, nil
}

func (s *Service) exchangeAuthorizationCode(ctx context.Context, params TokenRequest) (*TokenResponse, error) {
	if params.Code == nil {
		return nil, errTokenCodeRequired
	}
	issued, err := s.inflight.TakeIssued(ctx, *params.Code)
	if err != nil {
		return nil, errTokenInflight
	}
	if issued == nil {
		return nil, errTokenInvalidCode
	}
	if params.RedirectURI == nil {
		return nil, errTokenRedirectRequired
	}
	if *params.RedirectURI != issued.RedirectURI {
		return nil, errTokenRedirectMismatch
	}
	// The code is bound to the client_id that started the authorization
	// flow (RFC 6749 §4.1.3): when one was recorded, the token request must
	// present the same client_id.
	if issued.ClientID != "" {
		if params.ClientID == nil {
			return nil, errTokenClientRequired
		}
		if *params.ClientID != issued.ClientID {
			return nil, errTokenClientMismatch
		}
	}
	if params.CodeVerifier == nil {
		return nil, errTokenVerifierRequired
	}
	digest := sha256.Sum256([]byte(*params.CodeVerifier))
	if base64.RawURLEncoding.EncodeToString(digest[:]) != issued.CodeChallenge {
		return nil, errTokenPKCE
	}
	resp := &TokenResponse{
		AccessToken:  issued.AccessToken,
		RefreshToken: issued.RefreshToken,
		TokenType:    "Bearer",
	}
	if !issued.AccessTokenExpiresAt.IsZero() {
		remaining := uint64(max(time.Until(issued.AccessTokenExpiresAt).Seconds(), 0))
		resp.ExpiresIn = &remaining
	}
	return resp, nil
}

// CleanupExpired delegates to the store (only the in-memory backend needs it).
func (s *Service) CleanupExpired(ctx context.Context) error {
	return s.inflight.CleanupExpired(ctx)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isAllowedRedirectURI extends is_allowed_redirect_uri: loopback http is
// always allowed (local MCP clients redirect to a listener on an ephemeral
// port); https URIs must exact-match the MCP_ALLOWED_REDIRECT_URIS allowlist
// when it is configured, else any https URI is permitted (Rust parity).
func (s *Service) isAllowedRedirectURI(uri string) bool {
	parsed, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "https":
		if len(s.allowedRedirects) > 0 {
			_, ok := s.allowedRedirects[uri]
			return ok
		}
		return true
	case "http":
		switch parsed.Hostname() {
		case "localhost", "127.0.0.1", "::1", "[::1]":
			return true
		}
		return false
	default:
		return false
	}
}

// encodeURIComponent mirrors urlencoding::encode (percent-encode everything
// outside the unreserved set; QueryEscape + the '+' fix is equivalent).
func encodeURIComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

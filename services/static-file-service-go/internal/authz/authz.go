// Package authz mirrors macro_authorization's MacroAuthorizationExtractor with
// the UserOrInternal policy: internal service-key headers, user JWTs
// (macro-access-token HS256 and macro-api-token RS256), the ambiguous-
// credential guard, and the unauthorized rejection contract.
package authz

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Header names from macro_authorization::inbound::axum.
const (
	InternalAPIKeyHeader          = "x-internal-auth-key"
	InternalMacroUserIDHeader     = "x-internal-macro-user-id"
	InternalMacroOrgIDHeader      = "x-internal-macro-organization-id"
	InternalFusionUserIDHeader    = "x-internal-fusionauth-user-id"
	LegacyDSSInternalAPIKeyHeader = "x-document-storage-service-auth-key"
	LegacyDSSInternalUserIDHeader = "x-document-storage-service-user-id"
	BotTokenHeader                = "x-macro-bot-token"
	UserAPIKeyHeader              = "x-macro-user-api-key"
	HarnessTokenHeader            = "x-macro-harness-token"
	accessTokenCookieBase         = "macro-access-token"
	apiTokenQueryParam            = "macro-api-token"
)

// MacroInternalUserID mirrors MACRO_INTERNAL_USER_ID.
const MacroInternalUserID = "macro|INTERNAL@macro.com"

// Caller mirrors UserOrInternalCaller.
type Caller int

const (
	CallerUser Caller = iota
	CallerInternal
)

// Authorization is the narrowed UserOrInternal output: a verified acting
// user plus how the credential was established.
type Authorization struct {
	UserID       string
	FusionUserID string
	OrgID        *int
	Caller       Caller
}

// Config carries the resolved auth material.
type Config struct {
	InternalAPIKey         string
	DefaultInternalUserID  string
	JWTSecret              string // HMAC secret for macro-access-token
	Audience               string
	Issuer                 string
	MacroAPITokenIssuer    string
	MacroAPITokenPublicKey string // RSA public key (PEM) for macro-api-token
	Environment            string // prod|dev|local — selects the cookie prefix
}

// Authorizer validates requests.
type Authorizer struct {
	cfg        Config
	rsaKey     *rsa.PublicKey
	cookieName string
}

func New(cfg Config) (*Authorizer, error) {
	a := &Authorizer{cfg: cfg, cookieName: accessTokenCookieBase}
	switch cfg.Environment {
	case "dev":
		a.cookieName = "dev-" + accessTokenCookieBase
	case "local":
		a.cookieName = "local-" + accessTokenCookieBase
	}
	if cfg.DefaultInternalUserID == "" {
		a.cfg.DefaultInternalUserID = MacroInternalUserID
	}
	if cfg.MacroAPITokenPublicKey != "" {
		key, err := jwt.ParseRSAPublicKeyFromPEM([]byte(cfg.MacroAPITokenPublicKey))
		if err != nil {
			return nil, fmt.Errorf("unable to decode macro-api-token public key: %w", err)
		}
		a.rsaKey = key
	}
	return a, nil
}

type rejection struct {
	status  int
	message string
}

func (r *rejection) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": r.message})
}

func unauthorized() *rejection { return &rejection{http.StatusUnauthorized, "unauthorized"} }

// Middleware enforces the UserOrInternal policy. The verified Authorization is
// stored in the request context under ContextKey.
func (a *Authorizer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz, rej := a.authorize(r)
		if rej != nil {
			rej.write(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, authz)))
	})
}

type contextKey struct{}

// FromContext returns the verified Authorization, or nil.
func FromContext(ctx context.Context) *Authorization {
	a, _ := ctx.Value(contextKey{}).(*Authorization)
	return a
}

func (a *Authorizer) authorize(r *http.Request) (*Authorization, *rejection) {
	internal := internalConvention(r.Header)
	hasBot := r.Header.Get(BotTokenHeader) != ""
	hasAPIKey := r.Header.Get(UserAPIKeyHeader) != ""
	hasHarness := r.Header.Get(HarnessTokenHeader) != ""
	hasUserCred := explicitUserCredentialPresent(r)

	count := 0
	for _, b := range []bool{internal != nil, hasBot, hasAPIKey, hasHarness, hasUserCred} {
		if b {
			count++
		}
	}
	if count > 1 {
		return nil, &rejection{http.StatusBadRequest, "ambiguous credentials"}
	}

	if internal != nil {
		return a.authorizeInternal(r, internal)
	}
	if hasBot || hasAPIKey || hasHarness {
		// This service registers NoBotAuthorizer / NoUserApiKeyAuthorizer /
		// NoHarnessAuthorizer: those credential kinds can never authenticate.
		return nil, unauthorized()
	}

	user, rej := a.authorizeUser(r)
	if rej != nil {
		return nil, rej
	}
	if user == nil {
		return nil, unauthorized()
	}
	return user, nil
}

type internalConventionInfo struct {
	keyHeader      string
	userIDHeader   string
	orgIDHeader    string
	fusionIDHeader string
}

func internalConvention(h http.Header) *internalConventionInfo {
	if h.Get(InternalAPIKeyHeader) != "" {
		return &internalConventionInfo{
			keyHeader:      InternalAPIKeyHeader,
			userIDHeader:   InternalMacroUserIDHeader,
			orgIDHeader:    InternalMacroOrgIDHeader,
			fusionIDHeader: InternalFusionUserIDHeader,
		}
	}
	if h.Get(LegacyDSSInternalAPIKeyHeader) != "" {
		return &internalConventionInfo{
			keyHeader:    LegacyDSSInternalAPIKeyHeader,
			userIDHeader: LegacyDSSInternalUserIDHeader,
		}
	}
	return nil
}

func (a *Authorizer) authorizeInternal(r *http.Request, conv *internalConventionInfo) (*Authorization, *rejection) {
	provided := r.Header.Get(conv.keyHeader)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(a.cfg.InternalAPIKey)) != 1 {
		return nil, unauthorized()
	}
	userID := r.Header.Get(conv.userIDHeader)
	if userID == "" {
		userID = a.cfg.DefaultInternalUserID
	}
	if !validMacroUserID(userID) {
		return nil, &rejection{http.StatusUnauthorized, "invalid user id"}
	}
	var orgID *int
	if conv.orgIDHeader != "" {
		if v := r.Header.Get(conv.orgIDHeader); v != "" {
			var id int
			if _, err := fmt.Sscanf(v, "%d", &id); err == nil {
				orgID = &id
			}
		}
	}
	return &Authorization{
		UserID:       userID,
		FusionUserID: r.Header.Get(conv.fusionIDHeader),
		OrgID:        orgID,
		Caller:       CallerInternal,
	}, nil
}

func explicitUserCredentialPresent(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	if q := r.URL.Query(); q.Has(apiTokenQueryParam) {
		return true
	}
	return false
}

func (a *Authorizer) authorizeUser(r *http.Request) (*Authorization, *rejection) {
	var token string
	if explicitUserCredentialPresent(r) {
		// Explicit credentials: query param wins, then Bearer header.
		if v := r.URL.Query().Get(apiTokenQueryParam); v != "" {
			token = v
		} else {
			h := r.Header.Get("Authorization")
			const p = "Bearer "
			if !strings.HasPrefix(h, p) {
				return nil, unauthorized()
			}
			token = strings.TrimPrefix(h, p)
		}
	} else if c, err := r.Cookie(a.cookieName); err == nil {
		token = c.Value
	}
	if token == "" {
		return nil, nil
	}
	id, err := a.validateToken(token)
	if err != nil {
		return nil, unauthorized()
	}
	return id, nil
}

// macroAccessTokenClaims mirrors MacroAccessToken.
type macroAccessTokenClaims struct {
	FusionUserID        string `json:"fusion_user_id"`
	MacroUserID         string `json:"macro_user_id"`
	MacroOrganizationID *int   `json:"macro_organization_id"`
	RootMacroID         string `json:"root_macro_id"`
	jwt.RegisteredClaims
}

// macroAPITokenClaims mirrors MacroApiToken.
type macroAPITokenClaims struct {
	FusionUserID        string `json:"fusion_user_id"`
	MacroUserID         string `json:"macro_user_id"`
	MacroOrganizationID *int   `json:"macro_organization_id"`
	jwt.RegisteredClaims
}

// validateToken mirrors decode_jwt::handler: kid=="macro" → RS256 api token,
// otherwise HS256 access token with aud+iss+exp enforced.
func (a *Authorizer) validateToken(tokenStr string) (*Authorization, error) {
	unverified, _, err := jwt.NewParser().ParseUnverified(tokenStr, jwt.MapClaims{})
	if err != nil {
		return nil, errors.New("unable to decode token")
	}
	kid, _ := unverified.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("expected kid")
	}

	if kid == "macro" {
		if a.rsaKey == nil {
			return nil, errors.New("no api token public key configured")
		}
		claims := &macroAPITokenClaims{}
		_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
			}
			return a.rsaKey, nil
		}, jwt.WithIssuer(a.cfg.MacroAPITokenIssuer), jwt.WithExpirationRequired())
		if err != nil {
			return nil, err
		}
		if !validMacroUserID(claims.MacroUserID) {
			return nil, errors.New("invalid user id")
		}
		return &Authorization{
			UserID:       claims.MacroUserID,
			FusionUserID: claims.FusionUserID,
			OrgID:        claims.MacroOrganizationID,
			Caller:       CallerUser,
		}, nil
	}

	claims := &macroAccessTokenClaims{}
	_, err = jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		return []byte(a.cfg.JWTSecret), nil
	}, jwt.WithAudience(a.cfg.Audience), jwt.WithIssuer(a.cfg.Issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	if !validMacroUserID(claims.MacroUserID) {
		return nil, errors.New("invalid user id")
	}
	fusionID := claims.FusionUserID
	if claims.RootMacroID != "" {
		fusionID = claims.RootMacroID
	}
	return &Authorization{
		UserID:       claims.MacroUserID,
		FusionUserID: fusionID,
		OrgID:        claims.MacroOrganizationID,
		Caller:       CallerUser,
	}, nil
}

// validMacroUserID is a light stand-in for MacroUserId::parse_from_str:
// principals look like `kind|identifier`.
func validMacroUserID(s string) bool {
	kind, rest, ok := strings.Cut(s, "|")
	return ok && kind != "" && rest != ""
}

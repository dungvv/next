package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Casdoor implements Port against a self-hosted Casdoor instance using its
// standard OIDC endpoints plus the admin REST API for user management.
//
// Endpoint map (FusionAuth → Casdoor):
//
//	{base}/oauth2/authorize            → {public}/login/oauth/authorize
//	{base}/oauth2/token                → {base}/api/login/oauth/access_token
//	FusionAuth userinfo                → {base}/api/userinfo
//	FusionAuth admin API (API key hdr) → {base}/api/* (clientID:secret basic auth)
type Casdoor struct {
	cfg    Config
	oauth2 oauth2.Config
	http   *http.Client

	// oidc provider discovery is lazy: Casdoor may not be up when `macro api`
	// boots, and only token verification needs it.
	providerOnce sync.Once
	provider     *oidc.Provider
	providerErr  error
}

// NewCasdoor builds the Casdoor adapter from Config.
func NewCasdoor(cfg Config) (*Casdoor, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("identity: CASDOOR_CLIENT_ID is required")
	}
	c := &Casdoor{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
	}
	c.oauth2 = oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURI,
		Endpoint: oauth2.Endpoint{
			AuthURL:  strings.TrimRight(cfg.PublicEndpoint, "/") + "/login/oauth/authorize",
			TokenURL: strings.TrimRight(cfg.Endpoint, "/") + "/api/login/oauth/access_token",
			// Casdoor accepts client_secret in the POST body.
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: []string{"openid", "profile", "email", "offline_access"},
	}
	return c, nil
}

var defaultScopes = []string{"openid", "profile", "email", "offline_access"}

// AuthorizeURL builds the Casdoor authorize URL. `Provider` maps onto
// Casdoor's `provider` param which deep-links straight into the named IdP.
func (c *Casdoor) AuthorizeURL(req AuthorizeRequest) (string, error) {
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes
	}
	redirectURI := req.RedirectURI
	if redirectURI == "" {
		redirectURI = c.cfg.RedirectURI
	}
	u, err := url.Parse(c.oauth2.Endpoint.AuthURL)
	if err != nil {
		return "", fmt.Errorf("identity: bad authorize endpoint: %w", err)
	}
	q := u.Query()
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	if req.Provider != "" {
		q.Set("provider", req.Provider)
	}
	if req.LoginHint != "" {
		q.Set("login_hint", req.LoginHint)
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode completes the authorization-code grant at Casdoor's token
// endpoint. The id_token rides along in the token response extras.
func (c *Casdoor) ExchangeCode(ctx context.Context, code string) (*TokenGrant, error) {
	tok, err := c.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, oauthErr(err)
	}
	return grantFromToken(tok), nil
}

// PasswordLogin uses the OAuth resource-owner-password grant.
//
// TODO(verify): Casdoor supports grant_type=password when the application
// enables it. If the deployment disables ROPC this returns
// ErrInvalidCredentials/ErrUnsupported — treat as "password login off".
func (c *Casdoor) PasswordLogin(ctx context.Context, username, password string) (*TokenGrant, error) {
	tok, err := c.oauth2.PasswordCredentialsToken(ctx, username, password)
	if err != nil {
		return nil, oauthErr(err)
	}
	return grantFromToken(tok), nil
}

// RefreshTokens completes the refresh-token grant.
func (c *Casdoor) RefreshTokens(ctx context.Context, refreshToken string) (*TokenGrant, error) {
	src := c.oauth2.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	tok, err := src.Token()
	if err != nil {
		return nil, oauthErr(err)
	}
	return grantFromToken(tok), nil
}

func grantFromToken(tok *oauth2.Token) *TokenGrant {
	g := &TokenGrant{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
	}
	if idt, ok := tok.Extra("id_token").(string); ok {
		g.IDToken = idt
	}
	if !tok.Expiry.IsZero() {
		g.ExpiresIn = int64(time.Until(tok.Expiry).Seconds())
	}
	return g
}

func oauthErr(err error) error {
	var rerr *oauth2.RetrieveError
	if errors.As(err, &rerr) {
		switch rerr.Response.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized:
			return fmt.Errorf("%w: %s", ErrInvalidCredentials, rerr.ErrorDescription)
		}
	}
	return err
}

// oidcProvider lazily discovers Casdoor's OIDC configuration.
func (c *Casdoor) oidcProvider(ctx context.Context) (*oidc.Provider, error) {
	c.providerOnce.Do(func() {
		c.provider, c.providerErr = oidc.NewProvider(ctx, strings.TrimRight(c.cfg.Endpoint, "/"))
	})
	return c.provider, c.providerErr
}

// VerifyIDToken validates an id_token against Casdoor's JWKS and maps its
// claims onto a Profile.
func (c *Casdoor) VerifyIDToken(ctx context.Context, idToken string) (*Profile, error) {
	p, err := c.oidcProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: oidc discovery: %w", err)
	}
	tok, err := p.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, idToken)
	if err != nil {
		return nil, fmt.Errorf("identity: verify id token: %w", err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("identity: decode id token claims: %w", err)
	}
	return profileFromClaims(claims), nil
}

// UserInfo calls Casdoor's /api/userinfo endpoint.
func (c *Casdoor) UserInfo(ctx context.Context, accessToken string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.cfg.Endpoint, "/")+"/api/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("identity: userinfo status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, fmt.Errorf("identity: decode userinfo: %w", err)
	}
	return profileFromClaims(claims), nil
}

// LogoutURL builds the Casdoor logout URL. Casdoor's OIDC-flavored logout is
// `GET /api/logout` with id_token_hint + state, which terminates the SSO
// session.
//
// TODO(verify): confirm the post_logout_redirect_uri param name against the
// deployed Casdoor version.
func (c *Casdoor) LogoutURL(idTokenHint, postLogoutRedirectURI string) string {
	u, err := url.Parse(strings.TrimRight(c.cfg.PublicEndpoint, "/") + "/api/logout")
	if err != nil {
		return ""
	}
	q := u.Query()
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	if postLogoutRedirectURI != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirectURI)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- admin API -------------------------------------------------------------

// casdoorEnvelope is Casdoor's standard API response wrapper.
type casdoorEnvelope struct {
	Status string          `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

func (c *Casdoor) adminGet(ctx context.Context, path string, query url.Values, out any) error {
	u, err := url.Parse(strings.TrimRight(c.cfg.Endpoint, "/") + path)
	if err != nil {
		return err
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	return c.doAdmin(req, out)
}

func (c *Casdoor) adminPost(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.Endpoint, "/")+path, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/json")
	return c.doAdmin(req, out)
}

func (c *Casdoor) doAdmin(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("identity: admin api: %w", err)
	}
	defer resp.Body.Close()
	var env casdoorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("identity: admin api decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK || env.Status != "ok" {
		return fmt.Errorf("identity: admin api %s: status=%s http=%d", req.URL.Path, env.Msg, resp.StatusCode)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("identity: admin api data decode: %w", err)
		}
	}
	return nil
}

// casdoorUser is the subset of Casdoor's user record we read/write.
type casdoorUser struct {
	Owner         string   `json:"owner,omitempty"`
	Name          string   `json:"name,omitempty"`
	ID            string   `json:"id,omitempty"`
	Email         string   `json:"email,omitempty"`
	Password      string   `json:"password,omitempty"`
	DisplayName   string   `json:"displayName,omitempty"`
	FirstName     string   `json:"firstName,omitempty"`
	LastName      string   `json:"lastName,omitempty"`
	Avatar        string   `json:"avatar,omitempty"`
	IsAdmin       bool     `json:"isAdmin,omitempty"`
	Providers     []string `json:"providers,omitempty"`
	SignupApp     string   `json:"signupApplication,omitempty"`
	Type          string   `json:"type,omitempty"`
	CreatedTime   string   `json:"createdTime,omitempty"`
	EmailVerified bool     `json:"emailVerified,omitempty"`
}

// AdminGetUserByEmail looks up a Casdoor user by email inside the configured
// organization. Returns nil, nil when the user does not exist.
//
// TODO(verify): Casdoor's /api/get-user accepts `owner` + `email` params on
// recent versions; older versions may require listing via /api/get-users.
func (c *Casdoor) AdminGetUserByEmail(ctx context.Context, email string) (*Profile, error) {
	var u casdoorUser
	err := c.adminGet(ctx, "/api/get-user", url.Values{
		"owner": {c.cfg.Organization},
		"email": {email},
	}, &u)
	if err != nil {
		// Casdoor returns status:"error" for not found; surface as nil.
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	if u.ID == "" && u.Name == "" {
		return nil, nil
	}
	return casdoorUserProfile(&u), nil
}

// AdminCreateUser creates a Casdoor user in the configured organization
// (used by POST /user, replacing FusionAuth create_user).
func (c *Casdoor) AdminCreateUser(ctx context.Context, p *Profile, password string) error {
	user := casdoorUser{
		Owner:       c.cfg.Organization,
		Name:        casdoorUsername(p.Email),
		Email:       p.Email,
		Password:    password,
		DisplayName: firstNonEmpty(p.Name, p.Email),
		FirstName:   p.FirstName,
		LastName:    p.LastName,
		SignupApp:   c.cfg.Application,
		Type:        "normal-user",
	}
	return c.adminPost(ctx, "/api/add-user", user, nil)
}

// AdminDeleteUser deletes a Casdoor user. `name` is the Casdoor username
// (usually the email local part created by AdminCreateUser).
func (c *Casdoor) AdminDeleteUser(ctx context.Context, name string) error {
	return c.adminPost(ctx, "/api/delete-user", casdoorUser{
		Owner: c.cfg.Organization,
		Name:  name,
	}, nil)
}

// AdminSendVerificationCode asks Casdoor to email a signup verification code
// to the address — the FusionAuth skip_verification=false equivalent that
// gates password-created accounts on email ownership.
//
// TODO(verify): Casdoor's /api/send-verification-code field names
// (applicationId "owner/app", checkUser "true" because the account already
// exists when we send) against the deployed version.
func (c *Casdoor) AdminSendVerificationCode(ctx context.Context, email string) error {
	return c.adminPost(ctx, "/api/send-verification-code", map[string]any{
		"dest":          email,
		"type":          "signup",
		"method":        "email",
		"applicationId": c.cfg.Organization + "/" + c.cfg.Application,
		"checkUser":     "true",
	}, nil)
}

// casdoorUsername derives a Casdoor username from an email. Casdoor usernames
// must match `[a-zA-Z0-9_.-]+` roughly; fall back to a sanitized form.
func casdoorUsername(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, local)
	if local == "" {
		return "user"
	}
	return local
}

// --- claim mapping ---------------------------------------------------------

func profileFromClaims(claims map[string]any) *Profile {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := claims[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	p := &Profile{
		ProviderUserID: str("sub", "id", "user_id"),
		Email:          str("email"),
		Name:           str("displayName", "name", "preferred_username"),
		FirstName:      str("firstName", "given_name"),
		LastName:       str("lastName", "family_name"),
		Avatar:         str("picture", "avatar"),
	}
	if v, ok := claims["email_verified"].(bool); ok {
		p.EmailVerified = v
	}
	if v, ok := claims["providers"].([]any); ok {
		for _, pv := range v {
			if s, ok := pv.(string); ok {
				p.Providers = append(p.Providers, s)
			}
		}
	}
	return p
}

func casdoorUserProfile(u *casdoorUser) *Profile {
	return &Profile{
		ProviderUserID: u.ID,
		Email:          u.Email,
		Name:           u.DisplayName,
		FirstName:      u.FirstName,
		LastName:       u.LastName,
		Avatar:         u.Avatar,
		EmailVerified:  u.EmailVerified,
		Providers:      u.Providers,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

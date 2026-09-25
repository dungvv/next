package authentication

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// --- session codes & just-signed-up markers (macro_cache_client ports) ------

const (
	// mobileLoginSessionPrefix mirrors MACRO_MOBILE_LOGIN_SESSION_PREFIX.
	mobileLoginSessionPrefix = "mbl_login:"
	// mobileLoginSessionTTL mirrors MACRO_MOBILE_LOGIN_SESSION_EXPIRY_SECONDS.
	mobileLoginSessionTTL = 5 * time.Minute
	// justSignedUpPrefix mirrors macro_just_signed_up!.
	justSignedUpPrefix = "just_signed_up:"
	// justSignedUpTTL mirrors MACRO_JUST_SIGNED_UP_EXPIRY_SECONDS.
	justSignedUpTTL = 30 * time.Minute
)

func (d *Deps) setMobileLoginSession(ctx context.Context, code, refreshToken string) error {
	if d.Redis == nil {
		return errors.New("cache unavailable")
	}
	return d.Redis.Set(ctx, mobileLoginSessionPrefix+code, refreshToken, mobileLoginSessionTTL).Err()
}

// getMobileLoginSession consumes a session code atomically (GETDEL) — a code
// must not be redeemable twice, and a plain GET would let two racing readers
// both redeem it.
func (d *Deps) getMobileLoginSession(ctx context.Context, code string) (string, error) {
	if d.Redis == nil {
		return "", errors.New("cache unavailable")
	}
	v, err := d.Redis.GetDel(ctx, mobileLoginSessionPrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

func (d *Deps) markUserJustSignedUp(ctx context.Context, email string) {
	if d.Redis == nil {
		return
	}
	_ = d.Redis.Set(ctx, justSignedUpPrefix+strings.ToLower(email), "1", justSignedUpTTL).Err()
}

// takeUserJustSignedUp mirrors MacroCache::take_user_just_signed_up (GETDEL).
func (d *Deps) takeUserJustSignedUp(ctx context.Context, email string) bool {
	if d.Redis == nil {
		return false
	}
	v, err := d.Redis.GetDel(ctx, justSignedUpPrefix+strings.ToLower(email)).Result()
	return err == nil && v != ""
}

// generateSessionCode ports api::utils::generate_session_code — 25 chars from
// [a-zA-Z0-9] with at least one lower, one upper, one digit, then shuffled.
func generateSessionCode() (string, error) {
	const (
		lower  = "abcdefghijklmnopqrstuvwxyz"
		upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits = "0123456789"
		all    = lower + upper + digits
	)
	pick := func(charset string) (byte, error) {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		return charset[int(b[0])%len(charset)], nil
	}
	code := make([]byte, 0, 25)
	for _, set := range []string{lower, upper, digits} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		code = append(code, c)
	}
	for len(code) < 25 {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		code = append(code, c)
	}
	// Fisher–Yates shuffle.
	for i := len(code) - 1; i > 0; i-- {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		j := int(b[0]) % (i + 1)
		code[i], code[j] = code[j], code[i]
	}
	return string(code), nil
}

// --- original_url allowlist (login::sso port) --------------------------------

// isAllowedOriginalURL ports is_allowed_original_url verbatim: the app scheme
// and a fixed host allowlist for http(s)/tauri redirects.
func isAllowedOriginalURL(u *url.URL, extraHosts []string) bool {
	host := u.Hostname()
	switch u.Scheme {
	case "macro":
		return true
	case "tauri":
		return host == "localhost"
	case "http":
		if host == "localhost" || host == "tauri.localhost" {
			return true
		}
	case "https":
		if host == "localhost" || host == "tauri.localhost" ||
			host == "dev.macro.com" || host == "macro.com" {
			return true
		}
	default:
		return false
	}
	for _, h := range extraHosts {
		if h == host {
			return true
		}
	}
	return false
}

// allowedOriginalURLHosts parses the comma-separated env extra host list.
func (d *Deps) allowedOriginalURLHosts() []string {
	var hosts []string
	for _, h := range strings.Split(d.Cfg.AllowedOriginalURLHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// defaultRedirectURL ports api::utils::default_redirect_url.
func (d *Deps) defaultRedirectURL() string {
	if d.Cfg.FrontendURL != "" {
		return d.Cfg.FrontendURL
	}
	switch d.Cfg.Env {
	case "production", "prod":
		return "https://macro.com/app"
	case "develop", "dev":
		return "https://dev.macro.com/app"
	default:
		return fmt.Sprintf("http://localhost:%d", d.Cfg.FrontendPort)
	}
}

// --- macro user resolution / JIT provisioning ---------------------------------

// macroUser is what session issuance needs from macrodb.
type macroUser struct {
	// ID is User.id — "macro|<email>".
	ID             string
	OrganizationID *int64
	// RootMacroID is macro_user.id — goes into the root_macro_id claim.
	RootMacroID string
}

var errNoUser = errors.New("authentication: no macro user")

// lookupMacroUser resolves an email to the macro user record.
func (d *Deps) lookupMacroUser(ctx context.Context, email string) (*macroUser, error) {
	row, err := d.Q.GetUserInfoByEmail(ctx, strings.ToLower(email))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoUser
	}
	if err != nil {
		return nil, err
	}
	mu := &macroUser{ID: row.ID}
	if row.OrganizationID.Valid {
		org := int64(row.OrganizationID.Int32)
		mu.OrganizationID = &org
	}
	if row.MacroUserID.Valid {
		mu.RootMacroID = uuid.UUID(row.MacroUserID.Bytes).String()
	}
	return mu, nil
}

// ensureMacroUser is JIT provisioning: find-or-create the macro user for an
// authenticated provider profile. Replaces the FusionAuth create-user webhook
// for the self-host flow — Casdoor creates the identity; we create the DB rows
// on first completed login.
//
// Mirrors service::user::create_user: org match → roles → tx{ macro_user,
// email verification, User, RolesOnUsers }. Stripe customer creation is
// replaced by a deterministic local placeholder (self-host has no Stripe).
func (d *Deps) ensureMacroUser(ctx context.Context, p *identity.Profile) (*macroUser, bool, error) {
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if email == "" {
		return nil, false, fmt.Errorf("provider profile has no email")
	}
	if mu, err := d.lookupMacroUser(ctx, email); err == nil {
		return mu, false, nil
	} else if !errors.Is(err, errNoUser) {
		return nil, false, err
	}

	// macro_user.id was the FusionAuth UUID; Casdoor user ids are UUIDs too —
	// keep the same semantic, fall back to a fresh uuid when the provider
	// subject is not a UUID.
	macroUUID, err := uuid.Parse(p.ProviderUserID)
	if err != nil {
		macroUUID = uuid.New()
	}

	// Match the email to an organization (email or its domain).
	domain := ""
	if i := strings.LastIndex(email, "@"); i >= 0 {
		domain = email[i+1:]
	}
	var orgID *int64
	orgRow, err := d.Q.MatchUserToOrganization(ctx, []string{email, domain})
	if err == nil {
		org := int64(orgRow)
		orgID = &org
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("match organization: %w", err)
	}

	roles := []string{"self_serve"}
	if orgID != nil {
		roles, err = d.Q.GetOrganizationRolesForUser(ctx, int32(*orgID))
		if err != nil {
			return nil, false, fmt.Errorf("org roles: %w", err)
		}
		if _, err := d.Q.GetOrganizationRolesForUser2(ctx, email); err == nil {
			roles = append(roles, "organization_it")
		}
		if _, err := d.Q.GetOrganizationRolesForUser3(ctx, email); err == nil {
			roles = append(roles, "manage_organization_subscription")
		}
	}

	username := casdoorUsernameFallback(email)
	stripeID := "local-stripe-customer-" + email // self-host: no Stripe

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	qtx := d.Q.WithTx(tx)

	var muID pgtype.UUID
	muID.Bytes = macroUUID
	muID.Valid = true

	if _, err := qtx.CreateUser(ctx, macrodb.CreateUserParams{
		ID:               muID,
		Username:         username,
		StripeCustomerID: stripeID,
		Email:            email,
		HasTrialed:       false,
	}); err != nil {
		return nil, false, fmt.Errorf("create macro_user: %w", err)
	}
	if err := qtx.CreateUser2(ctx, macrodb.CreateUser2Params{
		MacroUserID: muID,
		Email:       email,
		IsVerified:  p.EmailVerified,
	}); err != nil {
		return nil, false, fmt.Errorf("create email verification: %w", err)
	}
	userID := "macro|" + email
	orgParam := pgtype.Int4{}
	if orgID != nil {
		orgParam.Int32 = int32(*orgID)
		orgParam.Valid = true
	}
	stripeParam := pgtype.Text{String: stripeID, Valid: true}
	if _, err := qtx.CreateUser3(ctx, macrodb.CreateUser3Params{
		ID:               userID,
		Email:            email,
		StripeCustomerId: stripeParam,
		OrganizationId:   orgParam,
		MacroUserID:      muID,
	}); err != nil {
		return nil, false, fmt.Errorf("create user: %w", err)
	}
	for _, role := range roles {
		if err := qtx.AddUserRole(ctx, macrodb.AddUserRoleParams{
			UserId: userID,
			RoleId: role,
		}); err != nil {
			return nil, false, fmt.Errorf("add role %q: %w", role, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}

	// Set the just-signed-up marker so the redirect can append signed_up=true.
	d.markUserJustSignedUp(ctx, email)
	slog.Info("authentication: provisioned new macro user", "email", email, "user_id", userID)

	return &macroUser{ID: userID, OrganizationID: orgID, RootMacroID: macroUUID.String()}, true, nil
}

func casdoorUsernameFallback(email string) string {
	local, _, _ := strings.Cut(email, "@")
	if local == "" {
		return "user"
	}
	return local
}

// --- short uuid ---------------------------------------------------------------

// flickrBase58 mirrors crates/macro_uuid::ShortUuidConverter (Flickr's base58
// alphabet, big-endian u128) used for referral codes.
const flickrBase58 = "123456789abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"

func shortUUIDFromUUID(u uuid.UUID) string {
	var num big.Int
	num.SetBytes(u[:])
	base := big.NewInt(int64(len(flickrBase58)))
	mod := new(big.Int)
	var digits []byte
	for num.Sign() > 0 {
		num.DivMod(&num, base, mod)
		digits = append([]byte{flickrBase58[mod.Int64()]}, digits...)
	}
	return string(digits)
}

func uuidFromShortUUID(s string) (uuid.UUID, bool) {
	if s == "" || len(s) >= 25 {
		return uuid.Nil, false
	}
	var num big.Int
	for _, c := range s {
		idx := strings.IndexRune(flickrBase58, c)
		if idx < 0 {
			return uuid.Nil, false
		}
		num.Mul(&num, big.NewInt(int64(len(flickrBase58))))
		num.Add(&num, big.NewInt(int64(idx)))
	}
	var u uuid.UUID
	num.FillBytes(u[:])
	return u, true
}

// --- refresh-token rotation store (identity.RefreshStore over Valkey) -------

// refreshTokenPrefix keys live refresh-token ids in Valkey.
const refreshTokenPrefix = "refresh_token:"

// redisRefreshStore is the RefreshStore adapter over Valkey: Register SETs
// jti→subject with the refresh TTL, Consume GETDELs (single-use, replay-safe),
// Revoke DELs.
type redisRefreshStore struct {
	rdb *redis.Client
}

func (s *redisRefreshStore) Register(ctx context.Context, id, subject string, ttl time.Duration) error {
	return s.rdb.Set(ctx, refreshTokenPrefix+id, subject, ttl).Err()
}

func (s *redisRefreshStore) Consume(ctx context.Context, id, subject string) (bool, error) {
	v, err := s.rdb.GetDel(ctx, refreshTokenPrefix+id).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == subject, nil
}

func (s *redisRefreshStore) Revoke(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, refreshTokenPrefix+id).Err()
}

// consumeRefresh validates the refresh token's signature/expiry and then
// atomically consumes its jti when a store is configured — the rotate-on-use
// half of the FusionAuth session semantics. Returns the claims for the
// caller to reissue against.
//
// Tokens minted before rotation existed carry no jti; with a store
// configured they are accepted once (and rotated into a tracked token) —
// they can never be revoked, so this is strictly a migration allowance.
func (d *Deps) consumeRefresh(ctx context.Context, raw string) (*identity.RefreshClaims, error) {
	claims, err := d.Validator.ValidateRefreshToken(raw)
	if err != nil {
		return nil, err
	}
	if d.RefreshTokens == nil || claims.ID == "" {
		return claims, nil
	}
	ok, err := d.RefreshTokens.Consume(ctx, claims.ID, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("consume refresh token: %w", err)
	}
	if !ok {
		return nil, identity.ErrRefreshTokenReused
	}
	return claims, nil
}

// revokeRefresh deletes the token's jti when a store is configured. Best
// effort: the caller logs failures; an already-expired or jti-less token is
// a no-op.
func (d *Deps) revokeRefresh(ctx context.Context, raw string) {
	if d.RefreshTokens == nil || raw == "" {
		return
	}
	claims, err := d.Validator.DecodeRefreshTokenAllowExpired(raw)
	if err != nil || claims.ID == "" {
		return
	}
	if err := d.RefreshTokens.Revoke(ctx, claims.ID); err != nil {
		slog.Warn("authentication: revoke refresh token failed", "err", err)
	}
}

// issueSession mints the access+refresh pair for a resolved macro user and,
// when a RefreshStore is configured, registers the refresh token's jti so it
// can be rotated/revoked.
func (d *Deps) issueSession(ctx context.Context, p *identity.Profile, mu *macroUser) (*identity.TokenPair, error) {
	rootID := mu.RootMacroID
	var rootPtr *string
	if rootID != "" {
		rootPtr = &rootID
	}
	access, err := d.Sessions.IssueAccessToken(identity.AccessClaims{
		Email:               strings.ToLower(p.Email),
		FusionUserID:        p.ProviderUserID,
		MacroUserID:         mu.ID,
		MacroOrganizationID: mu.OrganizationID,
		RootMacroID:         rootPtr,
	})
	if err != nil {
		return nil, err
	}
	refresh, jti, err := d.Sessions.IssueRefreshToken(mu.ID, strings.ToLower(p.Email))
	if err != nil {
		return nil, err
	}
	if d.RefreshTokens != nil {
		if err := d.RefreshTokens.Register(ctx, jti, mu.ID, d.Sessions.RefreshTTL()); err != nil {
			return nil, fmt.Errorf("register refresh token: %w", err)
		}
	}
	return &identity.TokenPair{AccessToken: access, RefreshToken: refresh}, nil
}

// errProviderEmailUnverified mirrors FusionAuthClientError::UserNotVerified —
// the 401 "user has not verified their primary email" gate.
var errProviderEmailUnverified = errors.New("provider account email is not verified")

// requireVerifiedProviderEmail ports the FusionAuth email-verification gate:
// a provider account created through password signup can't produce a session
// until its email is verified at the provider. Profiles the provider already
// asserts as verified (SSO/JIT) skip the admin lookup; a missing provider
// record is allowed (JIT provisioning hasn't run yet).
func (d *Deps) requireVerifiedProviderEmail(ctx context.Context, p *identity.Profile) error {
	if p.EmailVerified || d.Admin == nil {
		return nil
	}
	rec, err := d.Admin.AdminGetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(p.Email)))
	if err != nil {
		return fmt.Errorf("lookup provider user: %w", err)
	}
	if rec != nil && !rec.EmailVerified {
		return errProviderEmailUnverified
	}
	return nil
}

// signupAllowed ports the signup allowlist policy: when
// DEVELOPMENT_SIGNUP_ALLOWLIST_JSON is configured it gates signups in every
// environment (self-host deployments set it to restrict registration);
// otherwise the Rust policy applies — develop allows only macro.com,
// production/local allow everyone.
func (d *Deps) signupAllowed(email string) bool {
	if d.Cfg.SignupAllowlist != "" {
		if strings.HasSuffix(email, "@macro.com") {
			return true
		}
		var list []string
		if err := json.Unmarshal([]byte(d.Cfg.SignupAllowlist), &list); err != nil {
			return false
		}
		for _, e := range list {
			if strings.EqualFold(e, email) {
				return true
			}
		}
		return false
	}
	if d.Cfg.Env != "develop" && d.Cfg.Env != "dev" {
		return true
	}
	return strings.HasSuffix(email, "@macro.com")
}

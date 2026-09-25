package authentication

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// In Rust these endpoints were FusionAuth webhooks (InternalOnly-gated). For
// the self-host stack any trusted provisioner — Casdoor sync job, admin CLI,
// or an operator — can call them with x-internal-auth-key. JIT provisioning in
// helpers.go::ensureMacroUser already covers first-login user creation; these
// webhooks exist for out-of-band provider lifecycle events.

// userWebhook mirrors model::authentication::webhooks::FusionAuthUserWebhook
// (only the fields the handlers read).
type userWebhook struct {
	Event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		User struct {
			ID        string  `json:"id"`
			Email     string  `json:"email"`
			Username  *string `json:"username"`
			Verified  bool    `json:"verified"`
			FirstName *string `json:"firstName"`
			LastName  *string `json:"lastName"`
			FullName  *string `json:"fullName"`
		} `json:"user"`
	} `json:"event"`
}

// webhookCreateUser ports POST /webhooks/user — the create-user webhook.
// Rust ran signup policy, create_user (org match + roles + Stripe), the
// support channel, favorites, and Meta/analytics events. Here we run the
// tractable core: signup policy + macro user provisioning.
//
// TODO(port): support-channel welcome (channels service), favorites seeding,
// Meta CAPI/analytics events, Stripe customer creation.
func (rt *Router) webhookCreateUser(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	var req userWebhook
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Event.User.Email))
	if !isValidEmail(email) {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid email")
		return
	}
	if !d.signupAllowed(email) {
		httpx.ErrorJSON(w, http.StatusForbidden, "signup is not allowed for this email")
		return
	}

	first, last := providerName(req.Event.User.FirstName, req.Event.User.LastName, req.Event.User.FullName)
	_, isNew, err := d.ensureMacroUser(r.Context(), &identity.Profile{
		ProviderUserID: req.Event.User.ID,
		Email:          email,
		FirstName:      first,
		LastName:       last,
		EmailVerified:  req.Event.User.Verified,
	})
	if err != nil {
		slog.Error("authentication: create user webhook", "err", err, "email", email)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create user")
		return
	}
	if !isNew {
		slog.Info("authentication: create user webhook for existing user", "email", email)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// providerName mirrors identity_provider_name: prefer explicit first/last,
// else split full_name at the first space.
func providerName(first, last, full *string) (string, string) {
	trim := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	f, l := trim(first), trim(last)
	if f != "" || l != "" {
		return f, l
	}
	if name := trim(full); name != "" {
		if i := strings.Index(name, " "); i >= 0 {
			return name[:i], strings.TrimSpace(name[i+1:])
		}
		return name, ""
	}
	return "", ""
}

// webhookDeleteUser ports POST /webhooks/user/delete. Rust cleaned comms
// channels, notifications, DSS items and SQS link state here; the tractable
// DB core (User rows + macro_user) is ported, the rest is TODO.
//
// TODO(port): channel participant removal, notification cleanup, SQS
// LinkManagerMessage, DSS item deletion (DeleteUserChats*/DeleteUserDocuments*
// queries exist in macrodb but their SQS/DSS side-effects are unported).
func (rt *Router) webhookDeleteUser(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	var req userWebhook
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	providerID := req.Event.User.ID
	if providerID == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "missing user id")
		return
	}
	macroUUID, err := uuid.Parse(providerID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid user id")
		return
	}
	uid := pgtype.UUID{Bytes: macroUUID, Valid: true}

	// Skip deletion while an account merge is in flight (Rust parity).
	if _, err := d.Q.CheckMergeRequestForToMergeMacroUserId(r.Context(), uid); err == nil {
		slog.Info("authentication: delete user webhook skipped — merge request exists")
		httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("authentication: check merge request", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get merge request")
		return
	}

	profiles, err := d.Q.GetUserProfilesByFusionauthUserId(r.Context(), uid)
	if err != nil {
		slog.Error("authentication: get user profiles", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user profiles")
		return
	}
	for _, profileID := range profiles {
		if _, err := d.Q.DeleteUser(r.Context(), macrodb.DeleteUserParams{
			ID:          profileID,
			MacroUserID: uid,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("authentication: delete user profile", "err", err, "user_id", profileID)
		}
	}
	if err := d.Q.DeleteMacroUser(r.Context(), uid); err != nil {
		slog.Error("authentication: delete macro user", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// webhookPopulateJWT ports POST /webhooks/user/jwt — returns the macro user
// fields a JWT population lambda would inject. In the self-host flow the
// service issues tokens itself, so this exists for parity/testing.
func (rt *Router) webhookPopulateJWT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	mu, err := rt.deps.lookupMacroUser(r.Context(), email)
	if err != nil {
		slog.Error("authentication: populate jwt", "err", err, "email", email)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user info")
		return
	}
	var rootID *string
	if mu.RootMacroID != "" {
		rootID = &mu.RootMacroID
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		UserID         string  `json:"user_id"`
		OrganizationID *int64  `json:"organization_id"`
		RootMacroID    *string `json:"root_macro_id"`
	}{UserID: mu.ID, OrganizationID: mu.OrganizationID, RootMacroID: rootID})
}

// webhookUpdateName ports POST /webhooks/user/name — update a user's name by
// email. Rust spawned this async off the webhook; we do it inline.
func (rt *Router) webhookUpdateName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email     string  `json:"email"`
		FirstName *string `json:"first_name"`
		LastName  *string `json:"last_name"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	row, err := rt.deps.Q.GetUserMacroUserIdAndIdByEmail(r.Context(), email)
	if err != nil {
		slog.Error("authentication: update name webhook lookup", "err", err, "email", email)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user")
		return
	}
	text := func(p *string) pgtype.Text {
		if p == nil {
			return pgtype.Text{}
		}
		return pgtype.Text{String: *p, Valid: true}
	}
	if err := rt.deps.Q.Update(r.Context(), macrodb.UpdateParams{
		MacroUserID: row.MacroUserID,
		FirstName:   text(req.FirstName),
		LastName:    text(req.LastName),
	}); err != nil {
		slog.Error("authentication: update name webhook", "err", err, "email", email)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to update user name")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

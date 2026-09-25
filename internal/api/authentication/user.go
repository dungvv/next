package authentication

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// --- shared shapes (model::user ports) --------------------------------------

// userName mirrors model::user::UserName.
type userName struct {
	ID        string  `json:"id"`
	FirstName *string `json:"first_name"`
	LastName  *string `json:"last_name"`
}

// userNames mirrors model::user::UserNames.
type userNames struct {
	Names []userName `json:"names"`
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// providerUUID parses the caller's ProviderUserID (macro_user UUID) into
// pgtype — mirrors macro_uuid::string_to_uuid.
func providerUUID(u middleware.User) (pgtype.UUID, error) {
	var id pgtype.UUID
	if u.ProviderUserID == "" {
		return id, errors.New("missing provider user id")
	}
	parsed, err := uuid.Parse(u.ProviderUserID)
	if err != nil {
		return id, err
	}
	id.Bytes = parsed
	id.Valid = true
	return id, nil
}

// validMacroUserID mirrors MacroUserId::parse_from_str loosely: accepts
// "macro|<email>" or a bare UUID.
func validMacroUserID(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "macro|") && isValidEmail(strings.TrimPrefix(s, "macro|")) {
		return s, true
	}
	if _, err := uuid.Parse(s); err == nil {
		return s, true
	}
	return "", false
}

// --- handlers -----------------------------------------------------------------

// createUser ports POST /user: signup-policy check → username/email
// uniqueness → create the provider account (Casdoor admin API). The macrodb
// rows are provisioned on first login (ensureMacroUser) or via
// /webhooks/user — matching the Rust split between FusionAuth user creation
// and the create-user webhook.
func (rt *Router) createUser(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !d.signupAllowed(email) {
		httpx.ErrorJSON(w, http.StatusForbidden, "signup is not allowed for this email")
		return
	}
	if _, err := d.Q.CheckUsernameExists(r.Context(), req.Username); err == nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "username already exists")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check username")
		return
	}
	if _, err := d.Q.CheckEmailExists(r.Context(), email); err == nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "email already exists")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check email")
		return
	}
	if d.Admin == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "identity provider admin not configured")
		return
	}
	// TODO(port): password policy validation (Rust had the same TODO).
	if err := d.Admin.AdminCreateUser(r.Context(), &identity.Profile{
		Email: email,
		Name:  req.Username,
	}, req.Password); err != nil {
		slog.Error("authentication: create provider user", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create user")
		return
	}
	// FusionAuth's create_user ran skip_verification=false (the account
	// can't log in until the email is verified). The Casdoor account is
	// created unverified; kick off the provider's verification email —
	// best effort, an operator can resend.
	if err := d.Admin.AdminSendVerificationCode(r.Context(), email); err != nil {
		slog.Warn("authentication: send verification code failed", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// getUserInfo ports GET /user/me.
func (rt *Router) getUserInfo(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	perms, err := userPermissions(r, rt.deps.Q, user.UserID)
	if err != nil {
		slog.Error("authentication: get user permissions", "err", err, "user", user.UserID)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user permissions")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		UserID         string   `json:"user_id"`
		OrganizationID *int64   `json:"organization_id"`
		Permissions    []string `json:"permissions"`
	}{UserID: user.UserID, OrganizationID: user.OrganizationID, Permissions: perms})
}

// deleteUser ports DELETE /user/me: expire cookies, delete the provider
// identity, and delete the macrodb rows. The Rust version relies on the
// provider delete webhook to clean DB state asynchronously.
//
// TODO(port): full account deletion also removes documents/chats/links via
// the delete-user webhook pipeline (user_delete_user_dss_items_* queries and
// SQS cleanup jobs) — that orchestration is not ported yet.
func (rt *Router) deleteUser(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	user, _ := middleware.FromContext(r.Context())

	// Preserve the Rust footgun guard.
	if user.UserID == "macro|hutch@macro.com" {
		httpx.Error(w, http.StatusForbidden, "you cannot delete hutch")
		return
	}
	d.cookies.ClearAccess(w)
	d.cookies.ClearRefresh(w)

	email := strings.TrimPrefix(user.UserID, "macro|")
	if d.Admin != nil {
		profile, err := d.Admin.AdminGetUserByEmail(r.Context(), email)
		if err != nil {
			slog.Error("authentication: lookup provider user", "err", err)
			httpx.Error(w, http.StatusInternalServerError, "unable to get user")
			return
		}
		if profile == nil {
			// Fall back to the username convention used at create time.
			profile = &identity.Profile{Name: casdoorUsernameFallback(email)}
		}
		name := profile.Name
		if name == "" {
			name = casdoorUsernameFallback(email)
		}
		if err := d.Admin.AdminDeleteUser(r.Context(), name); err != nil {
			slog.Error("authentication: delete provider user", "err", err)
			httpx.Error(w, http.StatusInternalServerError, "unable to delete user")
			return
		}
	}

	// Delete the DB rows the Rust delete webhook would remove.
	if err := d.deleteUserRows(r, user, email); err != nil {
		slog.Error("authentication: delete user rows", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Success bool `json:"success"`
	}{Success: true})
}

func (d *Deps) deleteUserRows(r *http.Request, user middleware.User, email string) error {
	macroUUID, err := providerUUID(user)
	if err != nil {
		// Resolve via email when the claim lacks a parseable uuid.
		if mu, lerr := d.lookupMacroUser(r.Context(), email); lerr == nil && mu.RootMacroID != "" {
			if parsed, perr := uuid.Parse(mu.RootMacroID); perr == nil {
				macroUUID = pgtype.UUID{Bytes: parsed, Valid: true}
			}
		}
	}
	tx, err := d.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	qtx := d.Q.WithTx(tx)
	if _, err := qtx.DeleteUser(r.Context(), macrodb.DeleteUserParams{
		ID:          user.UserID,
		MacroUserID: macroUUID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if macroUUID.Valid {
		if err := qtx.DeleteMacroUser(r.Context(), macroUUID); err != nil {
			return err
		}
	}
	return tx.Commit(r.Context())
}

// getUserName ports GET /user/name.
func (rt *Router) getUserName(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	id, err := providerUUID(user)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "invalid user id")
		return
	}
	row, err := rt.deps.Q.GetUserName(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No name row yet — Rust returns the id with null names.
		httpx.WriteJSON(w, http.StatusOK, userName{ID: user.ProviderUserID})
		return
	}
	if err != nil {
		slog.Error("authentication: get user name", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to get user name")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, userName{
		ID:        uuid.UUID(row.MacroUserID.Bytes).String(),
		FirstName: textPtr(row.FirstName),
		LastName:  textPtr(row.LastName),
	})
}

// putUserName ports PUT /user/name?first_name=&last_name=.
func (rt *Router) putUserName(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	id, err := providerUUID(user)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "invalid user id")
		return
	}
	q := r.URL.Query()
	text := func(v string) pgtype.Text {
		return pgtype.Text{String: v, Valid: v != ""}
	}
	if err := rt.deps.Q.Update(r.Context(), macrodb.UpdateParams{
		MacroUserID: id,
		FirstName:   text(q.Get("first_name")),
		LastName:    text(q.Get("last_name")),
	}); err != nil {
		slog.Error("authentication: update user name", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to update user name")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// getNames ports POST /user/get_names (external variant; the internal twin
// lives in internal.go).
func (rt *Router) getNames(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserIDs []string `json:"user_ids"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rows, err := rt.deps.Q.GetUserNames(r.Context(), req.UserIDs)
	if err != nil {
		slog.Error("authentication: get names", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to get user names")
		return
	}
	out := make([]userName, 0, len(rows))
	for _, row := range rows {
		out = append(out, userName{
			ID:        row.UserProfileID,
			FirstName: textPtr(row.FirstName),
			LastName:  textPtr(row.LastName),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, userNames{Names: out})
}

// getNamesWithEmail ports POST /user/get_names_with_email.
func (rt *Router) getNamesWithEmail(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	var req struct {
		UserIDs []string `json:"user_ids"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ids := make([]string, 0, len(req.UserIDs))
	for _, raw := range req.UserIDs {
		if id, ok := validMacroUserID(raw); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		httpx.Error(w, http.StatusBadRequest, "user_ids cannot be empty")
		return
	}
	rows, err := rt.deps.Q.GetUserNamesWithEmail(r.Context(),
		macrodb.GetUserNamesWithEmailParams{MacroID: user.UserID, Column2: ids})
	if err != nil {
		slog.Error("authentication: get names with email", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to get user names")
		return
	}
	out := make([]userName, 0, len(rows))
	for _, row := range rows {
		out = append(out, userName{
			ID:        row.UserProfileID,
			FirstName: &row.FirstName,
			LastName:  &row.LastName,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, userNames{Names: out})
}

// getUserLinkExists ports GET /user/link_exists — whether the caller's
// provider account is linked to the named IdP.
func (rt *Router) getUserLinkExists(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	user, _ := middleware.FromContext(r.Context())
	q := r.URL.Query()
	idpName := q.Get("idp_name")
	idpID := q.Get("idp_id")
	if idpName == "" && idpID == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "idp_name or idp_id need to be provided")
		return
	}
	if d.Admin == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "identity provider admin not configured")
		return
	}
	email := strings.TrimPrefix(user.UserID, "macro|")
	profile, err := d.Admin.AdminGetUserByEmail(r.Context(), email)
	if err != nil {
		slog.Error("authentication: get provider user", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user links")
		return
	}
	// Casdoor provider names are the link identity (FusionAuth used idp ids —
	// idp_id lookups compare against the name after migration).
	want := idpName
	if want == "" {
		want = idpID
	}
	exists := false
	if profile != nil {
		for _, p := range profile.Providers {
			if strings.EqualFold(p, want) {
				exists = true
				break
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		LinkExists bool `json:"link_exists"`
	}{LinkExists: exists})
}

// patchUser is the shared PATCH helper for the User-row flag endpoints.
func (rt *Router) patchUser(w http.ResponseWriter, r *http.Request, apply func(id string) error) {
	user, _ := middleware.FromContext(r.Context())
	userID, ok := validMacroUserID(user.UserID)
	if !ok {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if err := apply(userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.ErrorJSON(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("authentication: patch user", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (rt *Router) patchTutorial(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TutorialComplete bool `json:"tutorialComplete"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rt.patchUser(w, r, func(id string) error {
		return rt.deps.Q.PatchUserTutorial(r.Context(), macrodb.PatchUserTutorialParams{
			TutorialComplete: req.TutorialComplete,
			ID:               id,
		})
	})
}

func (rt *Router) patchAIConsent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AIDataConsent bool `json:"aiDataConsent"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rt.patchUser(w, r, func(id string) error {
		return rt.deps.Q.PatchAiConsent(r.Context(), macrodb.PatchAiConsentParams{
			AiDataConsent: req.AIDataConsent,
			ID:            id,
		})
	})
}

func (rt *Router) patchUserGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rt.patchUser(w, r, func(id string) error {
		return rt.deps.Q.PatchUserGroup(r.Context(), macrodb.PatchUserGroupParams{
			Group: pgtype.Text{String: req.Group, Valid: req.Group != ""},
			ID:    id,
		})
	})
}

// patchUserOnboarding ports PATCH /user/onboarding
// (firstName/lastName/title/industry).
func (rt *Router) patchUserOnboarding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FirstName *string `json:"firstName"`
		LastName  *string `json:"lastName"`
		Title     *string `json:"title"`
		Industry  *string `json:"industry"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	opt := func(p *string) pgtype.Text {
		if p == nil {
			return pgtype.Text{}
		}
		return pgtype.Text{String: *p, Valid: true}
	}
	rt.patchUser(w, r, func(id string) error {
		return rt.deps.Q.PatchUserOnboarding(r.Context(), macrodb.PatchUserOnboardingParams{
			FirstName: opt(req.FirstName),
			LastName:  opt(req.LastName),
			Title:     opt(req.Title),
			Industry:  opt(req.Industry),
			ID:        id,
		})
	})
}

// getUserQuota ports GET /user/quota.
func (rt *Router) getUserQuota(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	userID, ok := validMacroUserID(user.UserID)
	if !ok {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid user id")
		return
	}
	row, err := rt.deps.Q.GetUserQuota(r.Context(), userID)
	if err != nil {
		slog.Error("authentication: get user quota", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get user quota")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		AiChatMessages int64 `json:"ai_chat_messages"`
		Documents      int64 `json:"documents"`
	}{AiChatMessages: row.AiChatMessages, Documents: row.Documents})
}

// getUserOrganization ports GET /user/organization — 204 when the caller has
// no org, else {organizationId, organizationName}.
func (rt *Router) getUserOrganization(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	var orgID *int64 = user.OrganizationID
	if orgID == nil {
		// Internal callers / stale tokens: fall back to the DB.
		row, err := rt.deps.Q.GetUserOrganization(r.Context(), user.UserID)
		if err == nil && row.Valid {
			org := int64(row.Int32)
			orgID = &org
		}
	}
	if orgID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	name, err := rt.deps.Q.GetOrganizationName(r.Context(), int32(*orgID))
	if err != nil {
		slog.Error("authentication: get organization", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		OrganizationID   int64  `json:"organizationId"`
		OrganizationName string `json:"organizationName"`
	}{OrganizationID: *orgID, OrganizationName: name})
}

// getProfilePictures ports POST /user/profile_pictures — {user_id_list} →
// {pictures:[{id, url, checksum}]}.
func (rt *Router) getProfilePictures(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserIDList []string `json:"user_id_list"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	type picture struct {
		ID       string  `json:"id"`
		URL      string  `json:"url"`
		Checksum *string `json:"checksum"`
	}
	if len(req.UserIDList) == 0 {
		httpx.WriteJSON(w, http.StatusOK, struct {
			Pictures []picture `json:"pictures"`
		}{Pictures: []picture{}})
		return
	}
	links, err := rt.deps.Q.GetProfilePictures(r.Context(), req.UserIDList)
	if err != nil {
		slog.Error("authentication: get profile pictures", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get profile pictures")
		return
	}
	profileIDByUUID := map[uuid.UUID]string{}
	uuids := make([]pgtype.UUID, 0, len(links))
	for _, l := range links {
		if !l.MacroUserID.Valid {
			continue
		}
		u := uuid.UUID(l.MacroUserID.Bytes)
		profileIDByUUID[u] = l.UserProfileID
		uuids = append(uuids, l.MacroUserID)
	}
	rows, err := rt.deps.Q.GetProfilePictures2(r.Context(), uuids)
	if err != nil {
		slog.Error("authentication: get profile pictures", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get profile pictures")
		return
	}
	out := make([]picture, 0, len(rows))
	for _, row := range rows {
		id := profileIDByUUID[uuid.UUID(row.MacroUserID.Bytes)]
		out = append(out, picture{
			ID:       id,
			URL:      row.ProfilePicture.String,
			Checksum: textPtr(row.ProfilePictureHash),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Pictures []picture `json:"pictures"`
	}{Pictures: out})
}

// putProfilePicture ports PUT /user/profile_picture?url=&checksum=.
//
// The Rust handler fetched the URL and checksummed it itself
// (fetch_and_checksum.rs); here we accept an optional checksum param and
// otherwise store without one.
//
// TODO(port): fetch the URL server-side and compute the checksum
// (fetch_and_checksum) so clients can't claim a different image.
func (rt *Router) putProfilePicture(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	id, err := providerUUID(user)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "invalid user id")
		return
	}
	q := r.URL.Query()
	pic := q.Get("url")
	checksum := q.Get("checksum")
	if err := rt.deps.Q.UpdateProfilePicture(r.Context(), macrodb.UpdateProfilePictureParams{
		MacroUserID:        id,
		ProfilePicture:     pgtype.Text{String: pic, Valid: pic != ""},
		ProfilePictureHash: pgtype.Text{String: checksum, Valid: checksum != ""},
	}); err != nil {
		slog.Error("authentication: update profile picture", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to update profile picture")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// getLegacyUserPermissions ports GET /user/legacy_user_permissions — the
// legacy entitlement blob the FE still reads (camelCase response).
func (rt *Router) getLegacyUserPermissions(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	userID, ok := validMacroUserID(user.UserID)
	if !ok {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid user id")
		return
	}
	row, err := rt.deps.Q.GetLegacyUserInfo(r.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "user not found")
		return
	}
	if err != nil {
		slog.Error("authentication: get legacy user info", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	perms, err := userPermissions(r, rt.deps.Q, user.UserID)
	if err != nil {
		slog.Error("authentication: get user permissions", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	licenseStatus := "inactive"
	for _, p := range perms {
		if p == "ReadProfessionalFeatures" {
			licenseStatus = "active"
			break
		}
	}
	var group *string
	if row.Group.Valid {
		switch strings.ToLower(row.Group.String) {
		case "a":
			g := "A"
			group = &g
		case "b":
			g := "B"
			group = &g
		}
	}
	var createdAt *string
	if row.CreatedAt.Valid {
		s := row.CreatedAt.Time.UTC().Format(time.RFC3339Nano)
		createdAt = &s
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		UserID           string   `json:"userId"`
		Permissions      []string `json:"permissions"`
		Email            string   `json:"email"`
		Name             *string  `json:"name"`
		LicenseStatus    string   `json:"licenseStatus"`
		TutorialComplete bool     `json:"tutorialComplete"`
		Group            *string  `json:"group"`
		HasChromeExt     bool     `json:"hasChromeExt"`
		HasTrialed       bool     `json:"hasTrialed"`
		AIDataConsent    bool     `json:"aiDataConsent"`
		ReferralCode     string   `json:"referralCode"`
		CreatedAt        *string  `json:"createdAt,omitempty"`
	}{
		UserID:           userID,
		Permissions:      perms,
		Email:            row.Email,
		Name:             textPtr(row.Name),
		LicenseStatus:    licenseStatus,
		TutorialComplete: row.TutorialComplete,
		Group:            group,
		HasChromeExt:     row.HasChromeExt,
		HasTrialed:       row.HasTrialed,
		AIDataConsent:    row.AiDataConsent,
		ReferralCode:     shortUUIDFromUUID(uuid.UUID(row.MacroUserID.Bytes)),
		CreatedAt:        createdAt,
	})
}

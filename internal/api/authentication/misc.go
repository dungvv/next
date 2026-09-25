package authentication

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// gtmOffer is the GET /gtm-invite/offer stub — the GTM invite domain service
// (invite links, free-months promo) is not ported.
//
// TODO(port): gtm_invite service — active_offer_for_user + free_months
// config → GtmInviteOfferStatus{offer}.
func (rt *Router) gtmOffer(w http.ResponseWriter, r *http.Request) {
	notImplemented("gtm invite offer")(w, r)
}

// mobileWelcomeEmail ports the tractable half of POST
// /mobile-welcome-email: email validation + the blocked-email check. The
// dedupe insert (mobile_welcome_email table — no generated sqlc query yet)
// and the Loops nurture-sequence send are TODOs, so the handler 501s after
// the validation gate rather than pretend the email was queued.
//
// TODO(port): add a sqlc query for insert_mobile_welcome_email
// (ON CONFLICT DO NOTHING → rows affected) and send via pkg/mail (or a
// Loops-equivalent) — Rust also rate-limited this endpoint.
func (rt *Router) mobileWelcomeEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !isValidEmail(email) {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid email address")
		return
	}
	blocked, err := rt.deps.Q.GetBlockedEmails(r.Context(), []string{email})
	if err != nil {
		slog.Error("authentication: check blocked emails", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(blocked) > 0 {
		httpx.ErrorJSON(w, http.StatusBadRequest, "email is blocked")
		return
	}
	httpx.ErrorJSON(w, http.StatusNotImplemented,
		"not implemented: mobile welcome email send (needs mobile_welcome_email insert query + mail port)")
}

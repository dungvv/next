package email

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// InitRequest is POST /email/init's body. In the Rust service this kicked off
// FusionAuth grant resolution + shared-inbox + watch + backfill orchestration.
// The selfhost version takes the caller's mailbox credentials directly: the
// OAuth dance that produced the refresh token happens outside this service
// (Casdoor/browser flow writes tokens via the token store).
type InitRequest struct {
	EmailAddress string `json:"email_address"`
	// Provider is the mailbox provider; only "GMAIL" is supported today.
	Provider string `json:"provider,omitempty"`
	// RefreshToken, when present, is stored so provider-backed features
	// (watch, label ops, attachment fetch) can call the Gmail API.
	RefreshToken string   `json:"refresh_token,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// NoBackfill skips creating the initial history-import job.
	NoBackfill bool `json:"no_backfill,omitempty"`
}

// initInbox handles POST /email/init — upserts the caller's email link, stores
// the gmail refresh token when provided, registers the gmail watch (best
// effort), and kicks off the initial backfill.
func (d *Deps) initInbox(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req InitRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.EmailAddress == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "email_address is required")
		return
	}
	provider := emaildb.EmailUserProviderEnumGMAIL
	if req.Provider != "" && req.Provider != string(emaildb.EmailUserProviderEnumGMAIL) {
		httpx.ErrorJSON(w, http.StatusBadRequest, "unsupported provider")
		return
	}
	ctx := r.Context()

	// Mailbox ownership: in Rust the link email came from the FusionAuth
	// grant, not the request body. Here the refresh token is that proof —
	// exchange it and require the Gmail profile's address to match the
	// claimed mailbox. Without the token store there is no provider access
	// to verify against, and provider-backed features stay disabled anyway.
	if d.Tokens != nil {
		if req.RefreshToken == "" {
			httpx.ErrorJSON(w, http.StatusBadRequest, "refresh_token is required to verify mailbox ownership")
			return
		}
		profileEmail, err := d.Tokens.VerifyMailbox(ctx, req.RefreshToken)
		if err != nil {
			slog.Warn("email: mailbox ownership check failed", "err", err)
			httpx.ErrorJSON(w, http.StatusForbidden, "unable to verify mailbox ownership")
			return
		}
		if !strings.EqualFold(profileEmail, req.EmailAddress) {
			httpx.ErrorJSON(w, http.StatusForbidden, "token does not belong to the claimed mailbox")
			return
		}
	}

	row, err := d.q().UpsertLink(ctx, emaildb.UpsertLinkParams{
		ID:               pgUUID(uuid.New()),
		MacroID:          c.UserID,
		FusionauthUserID: c.UserID,
		EmailAddress:     req.EmailAddress,
		Provider:         provider,
		IsSyncActive:     true,
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to upsert link")
		return
	}
	linkID := uuidFromPg(row.ID)

	// Default settings row for new links.
	if err := d.q().UpsertLink2(ctx, pgUUID(linkID)); err != nil {
		slog.Warn("email: init settings upsert failed", "link_id", linkID, "err", err)
	}

	// Store the refresh token so gmail-backed endpoints work.
	if req.RefreshToken != "" && d.Tokens != nil {
		if err := d.Tokens.Put(ctx, linkID, req.RefreshToken, req.Scopes); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to store credentials")
			return
		}
	}

	// Register the gmail push watch (best effort — needs both a token and a
	// configured Pub/Sub topic).
	if gc := d.gmailClient(ctx, linkID); gc != nil && d.Cfg.GmailPubsubTopic != "" {
		if _, err := gc.Watch(ctx, d.Cfg.GmailPubsubTopic); err != nil {
			slog.Warn("email: gmail watch registration failed", "link_id", linkID, "err", err)
		}
	}

	resp := InitResponse{LinkID: linkID}
	if !req.NoBackfill {
		job, err := d.q().CreateBackfillJob(ctx, emaildb.CreateBackfillJobParams{
			ID: pgUUID(uuid.New()), LinkID: pgUUID(linkID),
			FusionauthUserID:      c.UserID,
			ThreadsRequestedLimit: pgtype.Int4{},
			IsRecovery:            false,
		})
		if err == nil {
			jid := uuidFromPg(job.ID)
			if err := d.publishJob(ctx, subjEmailBackfill, "email.backfill", BackfillJobMsg{
				JobID: jid, LinkID: linkID,
			}); err != nil {
				slog.Warn("email: backfill enqueue failed", "job_id", jid, "err", err)
			}
			resp.BackfillJobID = &jid
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

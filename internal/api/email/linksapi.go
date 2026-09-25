package email

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// deriveSyncStatus mirrors api::link::SyncStatus::derive.
func deriveSyncStatus(isSyncActive, needsReauth bool, latest *emaildb.EmailBackfillJobStatus) string {
	if !isSyncActive {
		return "INACTIVE"
	}
	if needsReauth {
		return "NEEDS_REAUTH"
	}
	if latest == nil {
		return "UP_TO_DATE"
	}
	switch *latest {
	case emaildb.EmailBackfillJobStatusInit, emaildb.EmailBackfillJobStatusInProgress:
		return "SYNCING"
	case emaildb.EmailBackfillJobStatusFailed, emaildb.EmailBackfillJobStatusCancelled:
		return "ERROR"
	default:
		return "UP_TO_DATE"
	}
}

// listLinks handles GET /email/links.
func (d *Deps) listLinks(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	rows, err := d.q().FetchInboxDetailsForMacroId(r.Context(), c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	links := make([]Link, 0, len(rows))
	for _, row := range rows {
		var latest *emaildb.EmailBackfillJobStatus
		if row.LatestBackfillStatus != "" {
			s := row.LatestBackfillStatus
			latest = &s
		}
		// calendar permission heuristic: missing "calendar" scope → needs permission
		needsCal := true
		for _, s := range row.GoogleGrantedScopes {
			if s == "https://www.googleapis.com/auth/calendar" ||
				s == "https://www.googleapis.com/auth/calendar.readonly" {
				needsCal = false
			}
		}
		sigOn := false
		if row.SignatureOnRepliesForwards.Valid {
			sigOn = row.SignatureOnRepliesForwards.Bool
		}
		links = append(links, Link{
			ID: uuidFromPg(row.ID), MacroID: row.MacroID,
			FusionauthUserID: row.FusionauthUserID, EmailAddress: row.EmailAddress,
			PhotoURL: strPtrFromPg(row.PhotoUrl), Provider: string(row.Provider),
			IsSyncActive:            row.IsSyncActive,
			SyncStatus:              deriveSyncStatus(row.IsSyncActive, row.NeedsReauth, latest),
			NeedsReauth:             row.NeedsReauth,
			NeedsCalendarPermission: needsCal,
			CalendarDisabled:        row.CalendarDisabled,
			HasCalendarData:         row.HasCalendarData,
			Settings: Settings{
				SignatureOnRepliesForwards: &sigOn,
				Signature:                  strPtrFromPg(row.Signature),
			},
			IsPrimary: row.IsPrimary,
			CreatedAt: tsFromPg(row.CreatedAt), UpdatedAt: tsFromPg(row.UpdatedAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, ListLinksResponse{Links: links})
}

// linkHealthCheck handles POST /email/links/health-check — verifies each of
// the caller's links can still reach the provider (needs a stored token).
func (d *Deps) linkHealthCheck(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	links, err := d.accessibleLinks(r, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	type status struct {
		LinkID  uuid.UUID `json:"link_id"`
		Healthy bool      `json:"healthy"`
	}
	out := make([]status, 0, len(links))
	for _, l := range links {
		ok := false
		if gc := d.gmailClient(r.Context(), l.ID); gc != nil {
			if _, err := gc.GetProfile(r.Context()); err == nil {
				ok = true
			}
		}
		out = append(out, status{LinkID: l.ID, Healthy: ok})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"links": out})
}

// deleteLink handles DELETE /email/links/{link_id}: removes the link row and
// enqueues provider teardown (gmail watch stop + token cleanup).
func (d *Deps) deleteLink(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	linkID, err := uuid.Parse(chi.URLParam(r, "link_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid link id")
		return
	}
	ctx := r.Context()
	row, err := d.q().FetchLinkById(ctx, pgUUID(linkID))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "link not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch link")
		return
	}
	link := linkFromFetchLinkById(row)
	// only the owner may delete a link (delegated users lose access differently)
	if link.MacroID != c.UserID && !c.Internal {
		httpx.ErrorJSON(w, http.StatusForbidden, "only the link owner can delete it")
		return
	}
	// best-effort: stop the gmail watch + drop stored tokens
	if gc := d.gmailClient(ctx, linkID); gc != nil {
		_ = gc.Stop(ctx)
	}
	if d.Tokens != nil {
		_ = d.Tokens.Delete(ctx, linkID)
	}
	if err := d.q().DeleteLinkById(ctx, pgUUID(linkID)); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete link")
		return
	}
	d.publishJobBestEffort(ctx, subjLinkManager, "link_manager.delete", LinkManagerDeleteJob{
		LinkID: linkID, Email: link.EmailAddress,
		AuthID: link.FusionauthUserID, DeletedByID: c.UserID,
	})
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// resyncLink handles POST /email/links/{link_id}/resync — creates a new
// backfill job for the link and enqueues it.
func (d *Deps) resyncLink(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	linkID, err := uuid.Parse(chi.URLParam(r, "link_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid link id")
		return
	}
	ctx := r.Context()
	acc, err := d.linkAccessible(ctx, linkID, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to verify link access")
		return
	}
	if !acc {
		httpx.ErrorJSON(w, http.StatusNotFound, "link not found")
		return
	}
	link, err := d.q().FetchLinkById(ctx, pgUUID(linkID))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch link")
		return
	}
	// refuse if a job is already running
	if _, err := d.q().GetActiveBackfillJob(ctx, pgUUID(linkID)); err == nil {
		msg := "a backfill job is already in progress"
		httpx.WriteJSON(w, http.StatusOK, ResyncResponse{Success: false, Message: &msg})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check backfill status")
		return
	}
	jobID := uuid.New()
	job, err := d.q().CreateBackfillJob(ctx, emaildb.CreateBackfillJobParams{
		ID: pgUUID(jobID), LinkID: pgUUID(linkID),
		FusionauthUserID:      link.FusionauthUserID,
		ThreadsRequestedLimit: pgtype.Int4{},
		IsRecovery:            true,
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create backfill job")
		return
	}
	jid := uuidFromPg(job.ID)
	if err := d.publishJob(ctx, subjEmailBackfill, "email.backfill", BackfillJobMsg{
		JobID: jid, LinkID: linkID,
	}); err != nil {
		_ = d.q().FailBackfillJob(ctx, pgUUID(jid))
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ResyncResponse{Success: true, BackfillJobID: &jid})
}

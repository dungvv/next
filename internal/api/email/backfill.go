package email

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

func backfillFromGet(r emaildb.GetBackfillJobRow) BackfillJob {
	return BackfillJob{
		ID: uuidFromPg(r.ID), LinkID: uuidPtrFromPg(r.LinkID),
		FusionauthUserID:      r.FusionauthUserID,
		ThreadsRequestedLimit: i32PtrFromPg(r.ThreadsRequestedLimit),
		TotalThreads:          r.TotalThreads, Status: string(r.Status),
		ThreadsRetrievedCount: r.ThreadsRetrievedCount,
		CreatedAt:             tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func backfillFromActive(r emaildb.GetActiveBackfillJobRow) BackfillJob {
	return BackfillJob{
		ID: uuidFromPg(r.ID), LinkID: uuidPtrFromPg(r.LinkID),
		FusionauthUserID:      r.FusionauthUserID,
		ThreadsRequestedLimit: i32PtrFromPg(r.ThreadsRequestedLimit),
		TotalThreads:          r.TotalThreads, Status: string(r.Status),
		ThreadsRetrievedCount: r.ThreadsRetrievedCount,
		CreatedAt:             tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

// listBackfillJobs handles GET /email/backfill/gmail — all jobs for the
// caller's auth id (UserID doubles as fusionauth_user_id in the selfhost).
func (d *Deps) listBackfillJobs(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	rows, err := d.q().GetAllJobsByFusionauthUserId(r.Context(), c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch backfill jobs")
		return
	}
	jobs := make([]BackfillJob, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, BackfillJob{
			ID: uuidFromPg(row.ID), LinkID: uuidPtrFromPg(row.LinkID),
			FusionauthUserID:      row.FusionauthUserID,
			ThreadsRequestedLimit: i32PtrFromPg(row.ThreadsRequestedLimit),
			TotalThreads:          row.TotalThreads, Status: string(row.Status),
			ThreadsRetrievedCount: row.ThreadsRetrievedCount,
			CreatedAt:             tsFromPg(row.CreatedAt), UpdatedAt: tsFromPg(row.UpdatedAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, ListBackfillJobsResponse{Jobs: jobs})
}

// getBackfillJob handles GET /email/backfill/gmail/{id}.
func (d *Deps) getBackfillJob(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid job id")
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	row, err := d.q().GetBackfillJobWithLinkId(r.Context(), emaildb.GetBackfillJobWithLinkIdParams{
		ID: pgUUID(id), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "backfill job not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch backfill job")
		return
	}
	job := BackfillJob{
		ID: uuidFromPg(row.ID), LinkID: uuidPtrFromPg(row.LinkID),
		FusionauthUserID:      row.FusionauthUserID,
		ThreadsRequestedLimit: i32PtrFromPg(row.ThreadsRequestedLimit),
		TotalThreads:          row.TotalThreads, Status: string(row.Status),
		ThreadsRetrievedCount: row.ThreadsRetrievedCount,
		CreatedAt:             tsFromPg(row.CreatedAt), UpdatedAt: tsFromPg(row.UpdatedAt),
	}
	httpx.WriteJSON(w, http.StatusOK, GetBackfillJobResponse{Job: &job})
}

// getActiveBackfillJob handles GET /email/backfill/gmail/active.
func (d *Deps) getActiveBackfillJob(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	row, err := d.q().GetActiveBackfillJob(r.Context(), pgUUID(link.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteJSON(w, http.StatusOK, GetActiveBackfillJobResponse{Job: nil})
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch backfill job")
		return
	}
	job := backfillFromActive(row)
	httpx.WriteJSON(w, http.StatusOK, GetActiveBackfillJobResponse{Job: &job})
}

// cancelBackfillJob handles DELETE /email/backfill/gmail {job_id}.
func (d *Deps) cancelBackfillJob(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req CancelBackfillParams
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	row, err := d.q().GetBackfillJobWithLinkId(r.Context(), emaildb.GetBackfillJobWithLinkIdParams{
		ID: pgUUID(req.JobID), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "backfill job not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch backfill job")
		return
	}
	switch row.Status {
	case emaildb.EmailBackfillJobStatusComplete, emaildb.EmailBackfillJobStatusCancelled,
		emaildb.EmailBackfillJobStatusFailed:
		httpx.ErrorJSON(w, http.StatusBadRequest, "job is not running")
		return
	}
	if err := d.q().UpdateBackfillJobStatus(r.Context(), emaildb.UpdateBackfillJobStatusParams{
		Column1: emaildb.EmailBackfillJobStatusCancelled, ID: pgUUID(req.JobID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to cancel job")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// --- internal backfill endpoints (other services / workers) ------------------

// internalCreateBackfill handles POST /internal/backfill/provider/gmail —
// creates a backfill job for a link (internal callers only).
func (d *Deps) internalCreateBackfill(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	var req struct {
		LinkID                uuid.UUID `json:"link_id"`
		ThreadsRequestedLimit *int32    `json:"threads_requested_limit"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	link, err := d.q().FetchLinkById(r.Context(), pgUUID(req.LinkID))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "link not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch link")
		return
	}
	limit := pgtype.Int4{}
	if req.ThreadsRequestedLimit != nil {
		limit = pgtype.Int4{Int32: *req.ThreadsRequestedLimit, Valid: true}
	}
	job, err := d.q().CreateBackfillJob(r.Context(), emaildb.CreateBackfillJobParams{
		ID: pgUUID(uuid.New()), LinkID: pgUUID(req.LinkID),
		FusionauthUserID:      link.FusionauthUserID,
		ThreadsRequestedLimit: limit, IsRecovery: false,
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create job")
		return
	}
	jid := uuidFromPg(job.ID)
	if err := d.publishJob(r.Context(), subjEmailBackfill, "email.backfill", BackfillJobMsg{
		JobID: jid, LinkID: req.LinkID,
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, GetBackfillJobResponse{Job: &BackfillJob{
		ID: jid, LinkID: &req.LinkID, FusionauthUserID: link.FusionauthUserID,
		ThreadsRequestedLimit: req.ThreadsRequestedLimit,
		Status:                string(emaildb.EmailBackfillJobStatusInit),
	}})
}

// internalCancelBackfill handles DELETE /internal/backfill/provider/gmail.
func (d *Deps) internalCancelBackfill(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	var req CancelBackfillParams
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := d.q().UpdateBackfillJobStatus(r.Context(), emaildb.UpdateBackfillJobStatusParams{
		Column1: emaildb.EmailBackfillJobStatusCancelled, ID: pgUUID(req.JobID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to cancel job")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// internalGetBackfill handles GET /internal/backfill/provider/gmail/{id}.
func (d *Deps) internalGetBackfill(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid job id")
		return
	}
	row, err := d.q().GetBackfillJob(r.Context(), pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch job")
		return
	}
	job := backfillFromGet(row)
	httpx.WriteJSON(w, http.StatusOK, GetBackfillJobResponse{Job: &job})
}

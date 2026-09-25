package email

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// HeaderLinkID selects the inbox a request operates on (mirrors
// x-email-link-id in the Rust service). Absent → the caller's primary inbox.
const HeaderLinkID = "x-email-link-id"

var (
	errNoCaller     = errors.New("unauthorized")
	errLinkNotFound = errors.New("email link not found")
	errForbidden    = errors.New("link does not belong to caller")
)

// caller extracts the authenticated caller; writes 401 and returns false.
func caller(w http.ResponseWriter, r *http.Request) (auth.Caller, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
		return auth.Caller{}, false
	}
	return c, true
}

// accessibleLinks returns every inbox the caller can read: their own links
// plus links delegated to them through macro_user_links.
func (d *Deps) accessibleLinks(r *http.Request, macroID string) ([]linkRow, error) {
	rows, err := d.q().FetchInboxesForMacroId(r.Context(), macroID)
	if err != nil {
		return nil, err
	}
	links := make([]linkRow, 0, len(rows))
	for _, row := range rows {
		links = append(links, linkFromInbox(row))
	}
	return links, nil
}

// resolveLink returns the link for the request: the x-email-link-id inbox when
// the header is present (validated against the caller's accessible set),
// otherwise the caller's own primary (most recent) inbox.
func (d *Deps) resolveLink(w http.ResponseWriter, r *http.Request, c auth.Caller) (linkRow, bool) {
	ctx := r.Context()
	if hdr := r.Header.Get(HeaderLinkID); hdr != "" {
		id, err := uuid.Parse(hdr)
		if err != nil {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid x-email-link-id header")
			return linkRow{}, false
		}
		row, err := d.q().FetchLinkById(ctx, pgUUID(id))
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.ErrorJSON(w, http.StatusNotFound, "email link not found")
			return linkRow{}, false
		}
		if err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch link")
			return linkRow{}, false
		}
		link := linkFromFetchLinkById(row)
		// Caller must own the link or have it delegated via macro_user_links.
		if link.MacroID != c.UserID {
			ok, derr := d.linkAccessible(ctx, link.ID, c.UserID)
			if derr != nil {
				httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to verify link access")
				return linkRow{}, false
			}
			if !ok {
				httpx.ErrorJSON(w, http.StatusForbidden, "link not accessible")
				return linkRow{}, false
			}
		}
		return link, true
	}

	row, err := d.q().FetchLinkByMacroId(ctx, c.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "user has no email link")
		return linkRow{}, false
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch link")
		return linkRow{}, false
	}
	return linkFromMacroRow(row), true
}

func linkFromMacroRow(r emaildb.FetchLinkByMacroIdRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func pgTextBool(b bool) pgtype.Bool { return pgtype.Bool{Bool: b, Valid: true} }

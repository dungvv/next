package email

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// getSettings handles GET /email/settings.
func (d *Deps) getSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	row, err := d.q().FetchSettings(r.Context(), pgUUID(link.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		// no settings row → defaults
		f := false
		httpx.WriteJSON(w, http.StatusOK, Settings{SignatureOnRepliesForwards: &f})
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch settings")
		return
	}
	on := row.SignatureOnRepliesForwards
	httpx.WriteJSON(w, http.StatusOK, Settings{
		SignatureOnRepliesForwards: &on,
		Signature:                  strPtrFromPg(row.Signature),
	})
}

// patchSettings handles PATCH /email/settings — sanitizes the signature HTML
// before storing.
func (d *Deps) patchSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req PatchSettingsRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	var sig *string
	if req.Settings.Signature != nil {
		s := htmlSanitizer.Sanitize(*req.Settings.Signature)
		sig = &s
	}
	// The generated PatchSettings takes a non-nullable bool, which can't
	// express "leave unchanged" — run the same upsert with a nullable flag.
	var row emaildb.PatchSettingsRow
	err := d.Pool.QueryRow(r.Context(), `
		INSERT INTO email_settings (link_id, signature_on_replies_forwards, signature)
		VALUES ($1, COALESCE($2::bool, FALSE), $3)
		ON CONFLICT (link_id)
		DO UPDATE SET
		    signature_on_replies_forwards = COALESCE($2::bool, email_settings.signature_on_replies_forwards),
		    signature = COALESCE($3::text, email_settings.signature),
		    updated_at = NOW()
		RETURNING link_id, signature_on_replies_forwards, signature`,
		pgUUID(link.ID), pgBool(req.Settings.SignatureOnRepliesForwards), pgText(sig),
	).Scan(&row.LinkID, &row.SignatureOnRepliesForwards, &row.Signature)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to patch settings")
		return
	}
	on := row.SignatureOnRepliesForwards
	httpx.WriteJSON(w, http.StatusOK, PatchSettingsResponse{Settings: Settings{
		SignatureOnRepliesForwards: &on,
		Signature:                  strPtrFromPg(row.Signature),
	}})
}

func pgBool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

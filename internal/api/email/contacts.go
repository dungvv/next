package email

import (
	"net/http"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// listContacts handles GET /email/contacts — contacts grouped by link id.
func (d *Deps) listContacts(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	links, err := d.accessibleLinks(r, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	out := map[string][]ContactInfoWithInteraction{}
	for _, l := range links {
		rows, err := d.q().FetchContactsByLinkId(r.Context(), pgUUID(l.ID))
		if err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch contacts")
			return
		}
		list := make([]ContactInfoWithInteraction, 0, len(rows))
		for _, row := range rows {
			list = append(list, ContactInfoWithInteraction{
				Email:           row.EmailAddress,
				EmailAddress:    row.EmailAddress,
				Name:            strPtrFromPg(row.Name),
				PhotoURL:        strPtrFromPg(row.PhotoUrl),
				LastInteraction: tsPtrFromPg(row.LastInteraction),
			})
		}
		out[l.ID.String()] = list
	}
	httpx.WriteJSON(w, http.StatusOK, ListContactsResponse{Contacts: out})
}

// blockSender handles POST /email/contacts/block {email_address} — enqueues
// a gmail filter creation (async in Rust; same contract here).
func (d *Deps) blockSender(w http.ResponseWriter, r *http.Request) {
	d.senderOp(w, r, "block_sender", true)
}

// unblockSender handles POST /email/contacts/unblock {email_address}.
func (d *Deps) unblockSender(w http.ResponseWriter, r *http.Request) {
	d.senderOp(w, r, "unblock_sender", false)
}

func (d *Deps) senderOp(w http.ResponseWriter, r *http.Request, kind string, block bool) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req BlockSenderRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.EmailAddress == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "email_address is required")
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	if err := d.publishJob(r.Context(), subjGmailOps, "gmail_ops."+kind,
		gmailOps(link.ID, kind, map[string]any{
			"email_address": req.EmailAddress,
		})); err != nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// listBlocked handles GET /email/contacts/blocked — in the Rust service this
// reads Gmail filters via the provider. Without a Gmail client it degrades.
func (d *Deps) listBlocked(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	gc := d.gmailClient(r.Context(), link.ID)
	if gc == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "email provider not configured")
		return
	}
	emails, err := gc.ListBlockedSenders(r.Context())
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadGateway, "unable to fetch blocked senders")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ListBlockedResponse{BlockedEmails: emails})
}

package email

import (
	"github.com/go-chi/chi/v5"
)

// Register mounts the user-facing email routes. The caller's router must
// already enforce auth middleware (the Rust service mounted these under
// /email behind MacroAuthorizationExtractor).
func (d *Deps) Register(r chi.Router) {
	// threads
	r.Get("/threads/previews/cursor/{view}", d.getThreadPreviews)
	r.Get("/threads/{id}", d.getThread)
	r.Get("/threads/{id}/messages", d.getThreadMessages)
	r.Post("/threads/{id}/seen", d.threadSeen)
	r.Patch("/threads/{id}/archived", d.threadArchived)
	r.Patch("/threads/{id}/labels", d.updateThreadLabels)

	// messages
	r.Post("/messages", d.sendMessage) // send_router mounted at /
	r.Get("/messages/{id}", d.getMessage)
	r.Post("/messages/batch", d.getMessagesBatch)
	r.Patch("/messages/labels", d.updateMessageLabels)

	// drafts
	r.Post("/drafts", d.createDraft) // draft_router mounted at /
	r.Delete("/drafts/{id}", d.deleteDraft)
	r.Post("/drafts/{id}/attachments", d.addDraftAttachment)
	r.Delete("/drafts/{id}/attachments/{attachment_id}", d.removeDraftAttachment)
	r.Post("/drafts/{id}/forwarded-attachments", d.addForwardedAttachment)
	r.Delete("/drafts/{id}/forwarded-attachments/{attachment_id}", d.removeForwardedAttachment)
	r.Get("/drafts/scheduled", d.listScheduledDrafts)
	r.Put("/drafts/scheduled/{message_id}", d.upsertScheduledDraft)
	r.Delete("/drafts/scheduled/{message_id}", d.deleteScheduledDraft)

	// labels
	r.Get("/labels", d.listLabels)
	r.Post("/labels", d.createLabel)
	r.Delete("/labels/{id}", d.deleteLabel)

	// attachments
	r.Get("/attachments/{id}", d.getAttachment)
	r.Get("/attachments/{id}/document_id", d.getAttachmentDocumentID)

	// links
	r.Get("/links", d.listLinks)
	r.Post("/links/health-check", d.linkHealthCheck)
	r.Delete("/links/{link_id}", d.deleteLink)
	r.Post("/links/{link_id}/resync", d.resyncLink)

	// contacts
	r.Get("/contacts", d.listContacts)
	r.Post("/contacts/block", d.blockSender)
	r.Post("/contacts/unblock", d.unblockSender)
	r.Get("/contacts/blocked", d.listBlocked)

	// filters — provider filter management isn't ported; explicit 501.
	r.Put("/filters", d.notImplemented("email filters"))
	r.Get("/filters", d.notImplemented("email filters"))
	r.Delete("/filters/{id}", d.notImplemented("email filters"))

	// backfill
	r.Get("/backfill/gmail", d.listBackfillJobs)
	r.Get("/backfill/gmail/active", d.getActiveBackfillJob)
	r.Get("/backfill/gmail/{id}", d.getBackfillJob)
	r.Delete("/backfill/gmail", d.cancelBackfillJob)

	// settings
	r.Get("/settings", d.getSettings)
	r.Patch("/settings", d.patchSettings)

	// sync
	r.Delete("/sync", d.disableSync)

	// init
	r.Post("/init", d.initInbox)
}

// RegisterGmailWebhook mounts POST /gmail/webhook (Google Pub/Sub push
// receiver). Keep it unauthenticated like the Rust service — Pub/Sub can't
// send our internal key; the payload is validated by content.
func (d *Deps) RegisterGmailWebhook(r chi.Router) {
	r.Post("/webhook", d.gmailWebhook)
}

// RegisterInternal mounts the internal-only endpoints other services called
// on the Rust email service. Caller must already be behind auth middleware.
func (d *Deps) RegisterInternal(r chi.Router) {
	r.Get("/messages/{id}", d.internalGetMessage)
	r.Post("/messages/batch", d.internalGetMessagesBatch)
	r.Post("/messages/senders", d.internalGetSenders)
	r.Post("/threads/histories", d.notImplemented("thread histories"))
	r.Post("/backfill/provider/gmail", d.internalCreateBackfill)
	r.Delete("/backfill/provider/gmail", d.internalCancelBackfill)
	r.Get("/backfill/provider/gmail/{id}", d.internalGetBackfill)
	r.Delete("/delete_user/{id}", d.internalDeleteUser)
	r.Get("/threads/{id}/owner", d.internalGetThreadOwner)
}

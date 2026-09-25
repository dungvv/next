package email

import (
	"errors"
	"net/http"
	"strings"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// sendMessage handles POST /email/messages — upserts the message then
// dispatches it through the configured mail.Port (SMTP). The Rust service
// queues a delayed send through the provider; the self-host port sends
// synchronously so failures surface to the caller.
func (d *Deps) sendMessage(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req SendMessageRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if len(req.Message.To) == 0 && len(req.Message.Cc) == 0 && len(req.Message.Bcc) == 0 {
		httpx.ErrorJSON(w, http.StatusBadRequest, "message has no recipients")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	// Sending is owner-only: delegated inboxes can read but not send.
	if link.MacroID != c.UserID && !c.Internal {
		httpx.ErrorJSON(w, http.StatusForbidden, "cannot send from a delegated inbox")
		return
	}
	if d.Mail == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "mail sender not configured")
		return
	}
	out, err := d.saveDraft(ctx, link, req.Message, nil)
	if err != nil {
		switch {
		case errors.Is(err, errDraftGuardRejected), errors.Is(err, errLinkNotFound):
			httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		default:
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create message")
		}
		return
	}
	msgID := *out.DBID

	to := joinEmails(req.Message.To)
	msg := mail.Message{
		From:    link.EmailAddress,
		To:      to,
		Cc:      joinEmails(req.Message.Cc),
		Bcc:     joinEmails(req.Message.Bcc),
		Subject: req.Message.Subject,
	}
	if req.Message.BodyText != nil {
		msg.TextBody = *req.Message.BodyText
	}
	if html := decodedBodyForOutput(req.Message.BodyHTML); html != nil {
		msg.HTMLBody = *html
	}
	if msg.TextBody == "" && msg.HTMLBody != "" {
		msg.TextBody = computeBodyParsed(msg.HTMLBody)
	}
	if err := d.Mail.Send(ctx, msg); err != nil {
		httpx.ErrorJSON(w, http.StatusBadGateway, "send failed: "+err.Error())
		return
	}

	// Mark sent in one transaction — mirrors the Rust scheduled-send tx so a
	// message and its scheduled row can never disagree, and a send failure
	// (above) never marks anything sent. Provider ids are filled in by sync
	// when available.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to mark message sent")
		return
	}
	qtx := d.q().WithTx(tx)
	txErr := func() error {
		if err := qtx.MarkMessageAsSent(ctx, emaildb.MarkMessageAsSentParams{
			ID: pgUUID(msgID), LinkID: pgUUID(link.ID),
		}); err != nil {
			return err
		}
		return qtx.MarkScheduledMessageAsSent(ctx, emaildb.MarkScheduledMessageAsSentParams{
			LinkID: pgUUID(link.ID), MessageID: pgUUID(msgID),
		})
	}()
	if txErr == nil {
		txErr = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if txErr != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to mark message sent")
		return
	}
	d.publishJobBestEffort(ctx, subjEmailEvents, "email.message_sent", map[string]any{
		"link_id":    link.ID,
		"message_id": msgID,
		"thread_id":  out.ThreadDBID,
		"owner":      link.MacroID,
		"actor":      c.UserID,
	})
	httpx.WriteJSON(w, http.StatusOK, SendMessageResponse{Message: *out})
}

func joinEmails(in []DraftContactInfo) string {
	var out []string
	for _, c := range in {
		if c.Email != "" {
			if c.Name != nil && *c.Name != "" {
				out = append(out, `"`+strings.ReplaceAll(*c.Name, `"`, "")+`" <`+c.Email+`>`)
			} else {
				out = append(out, c.Email)
			}
		}
	}
	return strings.Join(out, ", ")
}

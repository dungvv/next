package email

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// --- pgtype helpers ----------------------------------------------------------

func uuidFromPg(u pgtype.UUID) uuid.UUID {
	return uuid.UUID(u.Bytes)
}

func uuidPtrFromPg(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	id := uuid.UUID(u.Bytes)
	return &id
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgUUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}

func strPtrFromPg(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func pgTextStr(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func tsPtrFromPg(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func tsFromPg(t pgtype.Timestamptz) time.Time {
	return t.Time.UTC()
}

func pgTs(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func i64PtrFromPg(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	return &i.Int64
}

func i32PtrFromPg(i pgtype.Int4) *int32 {
	if !i.Valid {
		return nil
	}
	return &i.Int32
}

// --- link rows ---------------------------------------------------------------

// linkRow is the common shape shared by the various Fetch*Link* generated rows.
type linkRow struct {
	ID               uuid.UUID
	MacroID          string
	FusionauthUserID string
	EmailAddress     string
	Provider         emaildb.EmailUserProviderEnum
	IsSyncActive     bool
	IsPrimary        bool
	NeedsReauth      bool
	LastSyncErrorAt  *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func linkFromFetchLinkById(r emaildb.FetchLinkByIdRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func linkFromInbox(r emaildb.FetchInboxesForMacroIdRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func linkFromOwnedForMessage(r emaildb.FetchOwnedLinkForMessageRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func linkFromOwnedForThread(r emaildb.FetchOwnedLinkForThreadRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

func linkFromInboxDetail(r emaildb.FetchInboxDetailsForMacroIdRow) linkRow {
	return linkRow{
		ID: uuidFromPg(r.ID), MacroID: r.MacroID, FusionauthUserID: r.FusionauthUserID,
		EmailAddress: r.EmailAddress, Provider: r.Provider, IsSyncActive: r.IsSyncActive,
		IsPrimary: r.IsPrimary, NeedsReauth: r.NeedsReauth,
		LastSyncErrorAt: tsPtrFromPg(r.LastSyncErrorAt),
		CreatedAt:       tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
	}
}

// --- label rows ----------------------------------------------------------------

func labelFromDB(l emaildb.EmailLabel) Label {
	return Label{
		ID:                    uuidFromPg(l.ID),
		LinkID:                uuidFromPg(l.LinkID),
		ProviderLabelID:       l.ProviderLabelID,
		Name:                  l.Name,
		CreatedAt:             tsFromPg(l.CreatedAt),
		MessageListVisibility: string(l.MessageListVisibility),
		LabelListVisibility:   string(l.LabelListVisibility),
		Type:                  string(l.Type),
	}
}

func msgLabelFromBulk(r emaildb.FetchMessageLabelsInBulkRow) MessageLabel {
	mlv := string(r.MessageListVisibility)
	llv := string(r.LabelListVisibility)
	typ := string(r.Type)
	id := uuidFromPg(r.ID)
	name := r.Name
	return MessageLabel{
		ID:                    &id,
		LinkID:                uuidFromPg(r.LinkID),
		ProviderLabelID:       r.ProviderLabelID,
		Name:                  &name,
		CreatedAt:             tsFromPg(r.CreatedAt),
		MessageListVisibility: &mlv,
		LabelListVisibility:   &llv,
		Type:                  &typ,
	}
}

// --- contact rows --------------------------------------------------------------

func contactFromRecipient(r emaildb.FetchDbRecipientsInBulkRow) ContactInfo {
	photo := r.SfsPhotoUrl
	if !photo.Valid {
		photo = r.OriginalPhotoUrl
	}
	return ContactInfo{
		Email:    r.EmailAddress,
		Name:     strPtrFromPg(r.Name),
		PhotoURL: strPtrFromPg(photo),
	}
}

func contactFromSender(r emaildb.FetchSendersByMessageIdsRow, fromName pgtype.Text) ContactInfo {
	photo := r.SfsPhotoUrl
	if !photo.Valid {
		photo = r.OriginalPhotoUrl
	}
	name := r.Name
	if fromName.Valid && fromName.String != "" {
		name = fromName
	}
	return ContactInfo{
		Email:    r.EmailAddress,
		Name:     strPtrFromPg(name),
		PhotoURL: strPtrFromPg(photo),
	}
}

// --- attachment rows -------------------------------------------------------------

func attachmentFromBulk(r emaildb.FetchDbAttachmentsInBulkRow) MessageAttachment {
	return MessageAttachment{
		DBID:       uuidFromPg(r.ID),
		ProviderID: strPtrFromPg(r.ProviderAttachmentID),
		Filename:   strPtrFromPg(r.Filename),
		MimeType:   strPtrFromPg(r.MimeType),
		SizeBytes:  i64PtrFromPg(r.SizeBytes),
		SfsID:      uuidPtrFromPg(r.SfsID),
		ContentID:  strPtrFromPg(r.ContentID),
	}
}

func attachmentFromThreadRow(r emaildb.GetAttachmentsByThreadIdsRow) MessageAttachment {
	return MessageAttachment{
		DBID:       uuidFromPg(r.ID),
		ProviderID: strPtrFromPg(r.ProviderAttachmentID),
		Filename:   strPtrFromPg(r.Filename),
		MimeType:   strPtrFromPg(r.MimeType),
		SizeBytes:  i64PtrFromPg(r.SizeBytes),
		SfsID:      uuidPtrFromPg(r.SfsID),
		ContentID:  strPtrFromPg(r.ContentID),
	}
}

func draftAttachmentFromBulk(r emaildb.FetchDbDraftAttachmentsInBulkRow) AttachmentDraft {
	return AttachmentDraft{
		ID:          uuidFromPg(r.ID),
		DraftID:     uuidFromPg(r.DraftID),
		FileName:    r.FileName,
		ContentType: r.ContentType,
		Sha:         r.Sha,
		Size:        r.Size,
		S3Key:       r.S3Key,
	}
}

func fwdAttachmentFromRow(r emaildb.FetchForwardedAttachmentsByDraftIdRow) AttachmentForwarded {
	return AttachmentForwarded{
		AttachmentID:         uuidFromPg(r.AttachmentID),
		DraftID:              uuidFromPg(r.DraftID),
		ProviderAttachmentID: strPtrFromPg(r.ProviderAttachmentID),
		MessageProviderID:    r.MessageProviderID.String,
		Filename:             strPtrFromPg(r.Filename),
		MimeType:             strPtrFromPg(r.MimeType),
		SizeBytes:            i64PtrFromPg(r.SizeBytes),
	}
}

// --- message rows --------------------------------------------------------------

// parsedMsg is a normalized view over the parsed-message generated rows.
type parsedMsg struct {
	ID                uuid.UUID
	ProviderID        *string
	GlobalID          *string
	ThreadID          uuid.UUID
	ProviderThreadID  *string
	ReplyingToID      *uuid.UUID
	LinkID            uuid.UUID
	ProviderHistoryID *string
	InternalDateTs    *time.Time
	Snippet           *string
	SizeEstimate      *int64
	Subject           *string
	FromName          *string
	FromContactID     *uuid.UUID
	SentAt            *time.Time
	HasAttachments    bool
	IsRead            bool
	IsStarred         bool
	IsSent            bool
	IsDraft           bool
	HeadersJSON       []byte
	CreatedAt         time.Time
	UpdatedAt         time.Time
	BodyText          *string
	BodyHTMLSanitized *string
	BodyMacro         *string
}

func parsedFromPaginatedRow(r emaildb.GetPaginatedParsedMessagesByThreadIdRow) parsedMsg {
	return parsedMsg{
		ID: uuidFromPg(r.ID), ProviderID: strPtrFromPg(r.ProviderID),
		GlobalID: strPtrFromPg(r.GlobalID), ThreadID: uuidFromPg(r.ThreadID),
		ProviderThreadID:  strPtrFromPg(r.ProviderThreadID),
		ReplyingToID:      uuidPtrFromPg(r.ReplyingToID),
		LinkID:            uuidFromPg(r.LinkID),
		ProviderHistoryID: strPtrFromPg(r.ProviderHistoryID),
		InternalDateTs:    tsPtrFromPg(r.InternalDateTs),
		Snippet:           strPtrFromPg(r.Snippet),
		SizeEstimate:      i64PtrFromPg(r.SizeEstimate),
		Subject:           strPtrFromPg(r.Subject),
		FromName:          strPtrFromPg(r.FromName),
		FromContactID:     uuidPtrFromPg(r.FromContactID),
		SentAt:            tsPtrFromPg(r.SentAt),
		HasAttachments:    r.HasAttachments,
		IsRead:            r.IsRead, IsStarred: r.IsStarred,
		IsSent: r.IsSent, IsDraft: r.IsDraft,
		HeadersJSON: r.HeadersJsonb,
		CreatedAt:   tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
		BodyText:          strPtrFromPg(r.BodyText),
		BodyHTMLSanitized: strPtrFromPg(r.BodyHtmlSanitized),
		BodyMacro:         strPtrFromPg(r.BodyMacro),
	}
}

func parsedFromByIdRow(r emaildb.GetParsedMessageByIdRow) parsedMsg {
	return parsedMsg{
		ID: uuidFromPg(r.ID), ProviderID: strPtrFromPg(r.ProviderID),
		GlobalID: strPtrFromPg(r.GlobalID), ThreadID: uuidFromPg(r.ThreadID),
		ProviderThreadID:  strPtrFromPg(r.ProviderThreadID),
		ReplyingToID:      uuidPtrFromPg(r.ReplyingToID),
		LinkID:            uuidFromPg(r.LinkID),
		ProviderHistoryID: strPtrFromPg(r.ProviderHistoryID),
		InternalDateTs:    tsPtrFromPg(r.InternalDateTs),
		Snippet:           strPtrFromPg(r.Snippet),
		SizeEstimate:      i64PtrFromPg(r.SizeEstimate),
		Subject:           strPtrFromPg(r.Subject),
		FromName:          strPtrFromPg(r.FromName),
		FromContactID:     uuidPtrFromPg(r.FromContactID),
		SentAt:            tsPtrFromPg(r.SentAt),
		HasAttachments:    r.HasAttachments,
		IsRead:            r.IsRead, IsStarred: r.IsStarred,
		IsSent: r.IsSent, IsDraft: r.IsDraft,
		HeadersJSON: r.HeadersJsonb,
		CreatedAt:   tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
		BodyText:          strPtrFromPg(r.BodyText),
		BodyHTMLSanitized: strPtrFromPg(r.BodyHtmlSanitized),
		BodyMacro:         strPtrFromPg(r.BodyMacro),
	}
}

// apiMessage assembles the full ApiMessage from a parsed row plus batch-fetched
// recipients/labels/attachments.
func apiMessage(m parsedMsg, from *ContactInfo, to, cc, bcc []ContactInfo,
	labels []MessageLabel, atts []MessageAttachment, drafts []AttachmentDraft,
	fwd []AttachmentForwarded, sendTime *time.Time) Message {
	if to == nil {
		to = []ContactInfo{}
	}
	if cc == nil {
		cc = []ContactInfo{}
	}
	if bcc == nil {
		bcc = []ContactInfo{}
	}
	if labels == nil {
		labels = []MessageLabel{}
	}
	if atts == nil {
		atts = []MessageAttachment{}
	}
	if drafts == nil {
		drafts = []AttachmentDraft{}
	}
	if fwd == nil {
		fwd = []AttachmentForwarded{}
	}
	headers := json_or_null(m.HeadersJSON)
	bodyReplyless := computeBodyReplyless(m.BodyHTMLSanitized, m.BodyText)
	return Message{
		DBID: m.ID, ProviderID: m.ProviderID, ThreadDBID: m.ThreadID,
		ProviderThreadID: m.ProviderThreadID, ReplyingToID: m.ReplyingToID,
		GlobalID: m.GlobalID, LinkID: m.LinkID, Subject: m.Subject,
		Snippet: m.Snippet, ProviderHistoryID: m.ProviderHistoryID,
		SentAt: m.SentAt, InternalDateTs: m.InternalDateTs,
		SizeEstimate: m.SizeEstimate, IsRead: m.IsRead, IsStarred: m.IsStarred,
		IsSent: m.IsSent, IsDraft: m.IsDraft, HasAttachments: m.HasAttachments,
		ScheduledSendTime: sendTime, From: from, To: to, Cc: cc, Bcc: bcc,
		Labels: labels, BodyText: m.BodyText, BodyHTMLSanitized: m.BodyHTMLSanitized,
		BodyMacro: m.BodyMacro, BodyReplyless: bodyReplyless,
		Attachments: atts, AttachmentsDraft: drafts, AttachmentsForwarded: fwd,
		HeadersJSON: headers, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func json_or_null(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

func parsedFromBatchRow(r emaildb.GetParsedMessagesByIdBatchRow) parsedMsg {
	return parsedMsg{
		ID: uuidFromPg(r.ID), ProviderID: strPtrFromPg(r.ProviderID),
		GlobalID: strPtrFromPg(r.GlobalID), ThreadID: uuidFromPg(r.ThreadID),
		ProviderThreadID:  strPtrFromPg(r.ProviderThreadID),
		ReplyingToID:      uuidPtrFromPg(r.ReplyingToID),
		LinkID:            uuidFromPg(r.LinkID),
		ProviderHistoryID: strPtrFromPg(r.ProviderHistoryID),
		InternalDateTs:    tsPtrFromPg(r.InternalDateTs),
		Snippet:           strPtrFromPg(r.Snippet),
		SizeEstimate:      i64PtrFromPg(r.SizeEstimate),
		Subject:           strPtrFromPg(r.Subject),
		FromName:          strPtrFromPg(r.FromName),
		FromContactID:     uuidPtrFromPg(r.FromContactID),
		SentAt:            tsPtrFromPg(r.SentAt),
		HasAttachments:    r.HasAttachments,
		IsRead:            r.IsRead, IsStarred: r.IsStarred,
		IsSent: r.IsSent, IsDraft: r.IsDraft,
		HeadersJSON: r.HeadersJsonb,
		CreatedAt:   tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
		BodyText:          strPtrFromPg(r.BodyText),
		BodyHTMLSanitized: strPtrFromPg(r.BodyHtmlSanitized),
		BodyMacro:         strPtrFromPg(r.BodyMacro),
	}
}

func parsedFromScheduledRow(r emaildb.GetScheduledDbMessagesByLinkIdRow) parsedMsg {
	return parsedMsg{
		ID: uuidFromPg(r.ID), ProviderID: strPtrFromPg(r.ProviderID),
		GlobalID: strPtrFromPg(r.GlobalID), ThreadID: uuidFromPg(r.ThreadID),
		ProviderThreadID:  strPtrFromPg(r.ProviderThreadID),
		ReplyingToID:      uuidPtrFromPg(r.ReplyingToID),
		LinkID:            uuidFromPg(r.LinkID),
		ProviderHistoryID: strPtrFromPg(r.ProviderHistoryID),
		InternalDateTs:    tsPtrFromPg(r.InternalDateTs),
		Snippet:           strPtrFromPg(r.Snippet),
		SizeEstimate:      i64PtrFromPg(r.SizeEstimate),
		Subject:           strPtrFromPg(r.Subject),
		FromName:          strPtrFromPg(r.FromName),
		FromContactID:     uuidPtrFromPg(r.FromContactID),
		SentAt:            tsPtrFromPg(r.SentAt),
		HasAttachments:    r.HasAttachments,
		IsRead:            r.IsRead, IsStarred: r.IsStarred,
		IsSent: r.IsSent, IsDraft: r.IsDraft,
		HeadersJSON: r.HeadersJsonb,
		CreatedAt:   tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
		BodyText:          strPtrFromPg(r.BodyText),
		BodyHTMLSanitized: strPtrFromPg(r.BodyHtmlSanitized),
		BodyMacro:         strPtrFromPg(r.BodyMacro),
	}
}

func parsedFromWithLabelsRow(r emaildb.FetchMessagesWithLabelsRow) parsedMsg {
	return parsedMsg{
		ID: uuidFromPg(r.ID), ProviderID: strPtrFromPg(r.ProviderID),
		GlobalID: strPtrFromPg(r.GlobalID), ThreadID: uuidFromPg(r.ThreadID),
		ProviderThreadID:  strPtrFromPg(r.ProviderThreadID),
		ReplyingToID:      uuidPtrFromPg(r.ReplyingToID),
		LinkID:            uuidFromPg(r.LinkID),
		ProviderHistoryID: strPtrFromPg(r.ProviderHistoryID),
		InternalDateTs:    tsPtrFromPg(r.InternalDateTs),
		Snippet:           strPtrFromPg(r.Snippet),
		SizeEstimate:      i64PtrFromPg(r.SizeEstimate),
		Subject:           strPtrFromPg(r.Subject),
		FromName:          strPtrFromPg(r.FromName),
		FromContactID:     uuidPtrFromPg(r.FromContactID),
		SentAt:            tsPtrFromPg(r.SentAt),
		HasAttachments:    r.HasAttachments,
		IsRead:            r.IsRead, IsStarred: r.IsStarred,
		IsSent: r.IsSent, IsDraft: r.IsDraft,
		HeadersJSON: r.HeadersJsonb,
		CreatedAt:   tsFromPg(r.CreatedAt), UpdatedAt: tsFromPg(r.UpdatedAt),
		BodyText:          strPtrFromPg(r.BodyText),
		BodyHTMLSanitized: strPtrFromPg(r.BodyHtmlSanitized),
		BodyMacro:         strPtrFromPg(r.BodyMacro),
	}
}

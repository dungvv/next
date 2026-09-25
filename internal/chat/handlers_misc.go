// Misc /channels + /comms handlers: activity, entity mentions, attachment
// references, batch preview, and the channel list surface.
package chat

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/pkg/frecency"
)

func chiParam(r *http.Request, name string) string { return chi.URLParam(r, name) }

// postActivity — POST /channels/activity. Body {activity_type: view|interact,
// channel_id}. Rust records per-user channel activity rows.
func (s *Server) postActivity(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req PostActivityRequest
	if !decodeBody(w, r, &req) {
		return
	}
	kind := "view"
	if req.ActivityType == "interact" {
		kind = "interact"
	}
	if err := s.db.upsertActivity(r.Context(), c.UserID, req.ChannelID, kind); err != nil {
		writeError(w, http.StatusInternalServerError, "activity failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getActivity — GET /channels/activity — caller's activity rows per channel.
func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	rows, err := s.db.userActivity(r.Context(), c.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// createMention — POST /channels/mentions.
func (s *Server) createMention(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req CreateEntityMentionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	uid := c.UserID
	m, err := s.db.createEntityMention(r.Context(), req, &uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mention insert failed")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// deleteMention — DELETE /channels/mentions/{mention_id}.
func (s *Server) deleteMention(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	id, ok := parseUUIDParam(w, r, "mention_id")
	if !ok {
		return
	}
	deleted, err := s.db.deleteEntityMention(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, DeleteEntityMentionResponse{Deleted: deleted})
}

// getAttachmentReferences — GET /channels/attachments/{entity_type}/{entity_id}/references.
func (s *Server) getAttachmentReferences(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	atts, err := s.db.attachmentReferences(r.Context(),
		chiParam(r, "entity_type"), chiParam(r, "entity_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, atts)
}

// channelPreviews — POST /channels/preview.
func (s *Server) channelPreviews(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req GetBatchChannelPreviewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	previews := make([]ChannelPreview, 0, len(req.ChannelIDs))
	for _, raw := range req.ChannelIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			previews = append(previews, ChannelPreview{"type": "does_not_exist", "channel_id": raw})
			continue
		}
		ch, err := s.db.getChannel(r.Context(), id)
		if err != nil {
			previews = append(previews, ChannelPreview{"type": "does_not_exist", "channel_id": raw})
			continue
		}
		member, _, _ := s.db.isParticipant(r.Context(), id, c.UserID)
		access := member || ch.ChannelType == ChannelPublic || (c.Internal && c.UserID == auth.InternalUserID)
		if !access {
			previews = append(previews, ChannelPreview{"type": "no_access", "channel_id": raw})
			continue
		}
		preview := ChannelPreview{
			"type":         "access",
			"channel_id":   ch.ID.String(),
			"channel_name": chName(ch),
			"channel_type": string(ch.ChannelType),
		}
		if ch.ProfilePictureID != nil {
			preview["profile_picture_id"] = ch.ProfilePictureID.String()
		}
		previews = append(previews, preview)
	}
	writeJSON(w, http.StatusOK, GetBatchChannelPreviewResponse{Previews: previews})
}

// listChannels — GET /comms/channels?limit=&cursor=.
// Ports the Rust channel_list_router: paginated {items, next_cursor} page
// ordered by updated_at, with participants, latest messages, caller
// activity, resolved display names, and frecency scores.
func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var before *time.Time
	var beforeID *uuid.UUID
	if cur := q.Get("cursor"); cur != "" {
		if t, id, err := decodeChannelCursor(cur); err == nil {
			before, beforeID = &t, &id
		} else {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}

	// Fetch limit+1 to distinguish a full final page (Rust does the same).
	channels, err := s.db.userChannels(r.Context(), c.UserID, limit+1, before, beforeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	hasMore := len(channels) > limit
	if hasMore {
		channels = channels[:limit]
	}

	// Batch the per-channel enrichment queries.
	ids := make([]string, 0, len(channels))
	participantIDs := map[string]bool{}
	for _, ch := range channels {
		ids = append(ids, ch.ID.String())
	}
	frecScores := map[string]frecency.Aggregate{}
	if s.frec != nil {
		if f, err := s.frec.AggregatesFor(r.Context(), c.UserID, "channel", ids); err == nil {
			frecScores = f
		}
	}
	// Participants per channel (needed before name resolution).
	partsByCh := map[uuid.UUID][]ChannelParticipant{}
	for _, ch := range channels {
		parts, err := s.db.participants(r.Context(), ch.ID)
		if err != nil {
			continue
		}
		partsByCh[ch.ID] = parts
		for _, p := range parts {
			participantIDs[p.UserID] = true
		}
	}
	allIDs := make([]string, 0, len(participantIDs))
	for id := range participantIDs {
		allIDs = append(allIDs, id)
	}
	names, _ := s.db.userNames(r.Context(), allIDs)

	items := make([]ChannelListItem, 0, len(channels))
	for _, ch := range channels {
		parts := partsByCh[ch.ID]
		latest, latestTop, _ := s.db.latestMessages(r.Context(), ch.ID)
		viewed, interacted, _ := s.db.activityFor(r.Context(), c.UserID, ch.ID)
		name := resolveChannelName(ch, c.UserID, parts, names)
		var score *float64
		if f, ok := frecScores[ch.ID.String()]; ok {
			sc := f.Score
			score = &sc
		}
		items = append(items, ChannelListItem{
			ID:                     ch.ID,
			Name:                   &name,
			ChannelType:            ch.ChannelType,
			OrgID:                  ch.OrgID,
			TeamID:                 ch.TeamID,
			AutoJoinTeam:           ch.AutoJoinTeam,
			CreatedAt:              ch.CreatedAt,
			UpdatedAt:              ch.UpdatedAt,
			OwnerID:                ch.OwnerID,
			Participants:           parts,
			IsParticipant:          true,
			LatestMessage:          latest,
			LatestNonThreadMessage: latestTop,
			ViewedAt:               viewed,
			InteractedAt:           interacted,
			FrecencyScore:          score,
		})
	}

	var next *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		cur := encodeChannelCursor(last.UpdatedAt, last.ID)
		next = &cur
	}
	writeJSON(w, http.StatusOK, ChannelListPage{Items: items, NextCursor: next})
}

// channelCursor is the opaque pagination token: base64(JSON{updated_at,id}).
type channelCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        uuid.UUID `json:"id"`
}

func encodeChannelCursor(t time.Time, id uuid.UUID) string {
	raw, _ := json.Marshal(channelCursor{UpdatedAt: t, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeChannelCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	var c channelCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return c.UpdatedAt, c.ID, nil
}

// resolveChannelName ports Rust resolve_channel_name: stored name wins;
// DM/private fall back to other participants' display names (or a generic
// label), public/team to "#<id8>".
func resolveChannelName(ch channelRow, viewerID string, parts []ChannelParticipant, names map[string]string) string {
	if ch.Name != nil && *ch.Name != "" {
		return *ch.Name
	}
	otherNames := func() []string {
		out := []string{}
		for _, p := range parts {
			if p.UserID == viewerID {
				continue
			}
			if n, ok := names[p.UserID]; ok && n != "" {
				out = append(out, n)
			} else {
				out = append(out, displayName(p.UserID))
			}
		}
		return out
	}
	switch ch.ChannelType {
	case ChannelPublic, ChannelTeam:
		return "#" + ch.ID.String()[:8]
	case ChannelPrivate:
		ns := otherNames()
		sort.Strings(ns)
		if len(ns) == 0 {
			return "Private channel"
		}
		return strings.Join(ns, ", ")
	case ChannelDM:
		if ns := otherNames(); len(ns) > 0 {
			return ns[0]
		}
		return "Direct message"
	}
	return ""
}

// commsActivity — GET /comms/activity — caller's activity rows.
func (s *Server) commsActivity(w http.ResponseWriter, r *http.Request) {
	s.getActivity(w, r)
}

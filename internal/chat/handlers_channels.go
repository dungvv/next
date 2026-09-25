// HTTP handlers for the /channels surface, ported from
// crates/channels/src/inbound/axum_router.rs semantics.
package chat

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
)

// callerOrErr extracts the authenticated caller.
func caller(w http.ResponseWriter, r *http.Request) (auth.Caller, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return auth.Caller{}, false
	}
	return c, true
}

// requireParticipant enforces channel membership (or internal caller).
func (s *Server) requireParticipant(w http.ResponseWriter, r *http.Request, channelID uuid.UUID) (auth.Caller, bool) {
	c, ok := caller(w, r)
	if !ok {
		return c, false
	}
	if c.Internal && c.UserID == auth.InternalUserID {
		return c, true
	}
	member, _, err := s.db.isParticipant(r.Context(), channelID, c.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership check failed")
		return c, false
	}
	if !member {
		writeError(w, http.StatusForbidden, "not a channel participant")
		return c, false
	}
	return c, true
}

// requireRole enforces a minimum participant role for mutations (owner/admin
// for destructive ops).
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, channelID uuid.UUID, min ParticipantRole) (auth.Caller, bool) {
	c, ok := caller(w, r)
	if !ok {
		return c, false
	}
	if c.Internal && c.UserID == auth.InternalUserID {
		return c, true
	}
	member, role, err := s.db.isParticipant(r.Context(), channelID, c.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership check failed")
		return c, false
	}
	if !member || (min == RoleOwner && role != RoleOwner && role != RoleAdmin) {
		writeError(w, http.StatusForbidden, "insufficient channel role")
		return c, false
	}
	return c, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

// createChannel — POST /channels/.
func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req CreateChannelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	switch req.ChannelType {
	case ChannelPublic, ChannelPrivate, ChannelDM, ChannelTeam:
	default:
		writeError(w, http.StatusBadRequest, "invalid channel_type")
		return
	}
	if req.ChannelType != ChannelDM && req.ChannelType != ChannelPrivate && req.Name == nil {
		writeError(w, http.StatusBadRequest, "name is required for this channel type")
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO comms_channels (id, name, channel_type, org_id, team_id,
		                            auto_join_team, owner_id)
		VALUES ($1, $2, $3::comms_channel_type, $4, $5, $6, $7)`,
		id, req.Name, string(req.ChannelType), nil, req.TeamID, req.AutoJoinTeam, c.UserID); err != nil {
		writeError(w, http.StatusBadRequest, "channel insert failed: "+err.Error())
		return
	}
	if err := s.db.addParticipant(r.Context(), tx, id, c.UserID, RoleOwner); err != nil {
		writeError(w, http.StatusInternalServerError, "owner insert failed")
		return
	}
	for _, p := range req.Participants {
		if p == c.UserID {
			continue
		}
		if err := s.db.addParticipant(r.Context(), tx, id, p, RoleMember); err != nil {
			continue // non-blocking like Rust: participant add failures skip
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	// Invite notifications to added participants (channel_invite).
	if ch2, err := s.db.getChannel(r.Context(), id); err == nil && len(req.Participants) > 0 {
		recipients := []string{}
		for _, p := range req.Participants {
			if p != c.UserID {
				recipients = append(recipients, p)
			}
		}
		s.fx.sendIngress(r.Context(), ch2, &c.UserID, recipients, nil,
			"channel_invite", map[string]any{
				"invitedBy":   c.UserID,
				"channelName": chName(ch2),
			}, "Channel invite", "You've been invited to a channel")
	}
	writeJSON(w, http.StatusOK, CreateChannelResponse{ID: id.String()})
}

// getOrCreateDM — POST /channels/get_or_create_dm.
func (s *Server) getOrCreateDM(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req GetOrCreateDmRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RecipientID == "" {
		writeError(w, http.StatusBadRequest, "recipient_id required")
		return
	}
	if existing, err := s.db.findDM(r.Context(), c.UserID, req.RecipientID); err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	} else if existing != nil {
		writeJSON(w, http.StatusOK, GetOrCreateChannelResponse{ChannelID: existing.String(), Action: "get"})
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO comms_channels (id, name, channel_type, owner_id)
		VALUES ($1, NULL, 'direct_message', $2)`, id, c.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "channel insert failed")
		return
	}
	for _, uid := range []string{c.UserID, req.RecipientID} {
		if err := s.db.addParticipant(r.Context(), tx, id, uid, RoleMember); err != nil {
			writeError(w, http.StatusInternalServerError, "participant insert failed")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, GetOrCreateChannelResponse{ChannelID: id.String(), Action: "create"})
}

// getOrCreatePrivate — POST /channels/get_or_create_private.
func (s *Server) getOrCreatePrivate(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req GetOrCreatePrivateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	members := append([]string{c.UserID}, req.Recipients...)
	if existing, err := s.db.findPrivate(r.Context(), members); err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	} else if existing != nil {
		writeJSON(w, http.StatusOK, GetOrCreateChannelResponse{ChannelID: existing.String(), Action: "get"})
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO comms_channels (id, name, channel_type, owner_id)
		VALUES ($1, NULL, 'private', $2)`, id, c.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "channel insert failed")
		return
	}
	if err := s.db.addParticipant(r.Context(), tx, id, c.UserID, RoleOwner); err != nil {
		writeError(w, http.StatusInternalServerError, "participant insert failed")
		return
	}
	for _, uid := range req.Recipients {
		_ = s.db.addParticipant(r.Context(), tx, id, uid, RoleMember)
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, GetOrCreateChannelResponse{ChannelID: id.String(), Action: "create"})
}

// getChannel — GET /channels/{channel_id}.
func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := caller(w, r)
	if !ok {
		return
	}
	ch, err := s.db.getChannel(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	member, _, _ := s.db.isParticipant(r.Context(), id, c.UserID)
	if !member && !(c.Internal && c.UserID == auth.InternalUserID) && ch.ChannelType != ChannelPublic {
		writeError(w, http.StatusForbidden, "not a channel participant")
		return
	}
	parts, err := s.db.participants(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, GetChannelResponse{
		ID:            ch.ID,
		Name:          ch.Name,
		ChannelType:   ch.ChannelType,
		OrgID:         ch.OrgID,
		TeamID:        ch.TeamID,
		AutoJoinTeam:  ch.AutoJoinTeam,
		CreatedAt:     ch.CreatedAt,
		UpdatedAt:     ch.UpdatedAt,
		OwnerID:       ch.OwnerID,
		JoinCode:      ch.JoinCode,
		Participants:  parts,
		IsParticipant: member,
	})
}

// patchChannel — PATCH /channels/{channel_id}.
func (s *Server) patchChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireRole(w, r, id, RoleOwner); !ok {
		return
	}
	var req PatchChannelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ch, err := s.db.getChannel(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	var newType *ChannelType
	var teamID *uuid.UUID
	if req.ConvertToTeamChannel != nil {
		if *req.ConvertToTeamChannel {
			t := ChannelTeam
			newType = &t
			teamID = ch.TeamID
		} else {
			t := ChannelPrivate
			newType = &t
		}
	}
	if err := s.db.patchChannel(r.Context(), id, req.ChannelName, newType, teamID, req.AutoJoinTeam); err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if newType != nil {
		_ = s.db.setTeamID(r.Context(), id, teamID)
	}
	// Fan the updated channel out to participants (comms_channel frame).
	if updated, err := s.db.getChannel(r.Context(), id); err == nil {
		if uids, err := s.db.participantUserIDs(r.Context(), id); err == nil {
			s.fx.commsChannel(uids, updated)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// deleteChannel — DELETE /channels/{channel_id}.
func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireRole(w, r, id, RoleOwner); !ok {
		return
	}
	if err := s.db.deleteChannel(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	// Cross-entity cleanup (share permissions, entity rows) is owned by the
	// jobs.delete_chat consumer — mirror the Rust trigger.
	s.publishDeleteChatJob(r.Context(), id)
	w.WriteHeader(http.StatusOK)
}

// joinChannel — POST /channels/{channel_id}/join (self-join public/team).
func (s *Server) joinChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := caller(w, r)
	if !ok {
		return
	}
	ch, err := s.db.getChannel(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	if ch.ChannelType != ChannelPublic && ch.ChannelType != ChannelTeam {
		writeError(w, http.StatusForbidden, "channel requires an invite")
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if err := s.db.addParticipant(r.Context(), tx, id, c.UserID, RoleMember); err != nil {
		writeError(w, http.StatusInternalServerError, "join failed")
		return
	}
	_ = s.db.touchChannel(r.Context(), tx, id)
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// joinChannelByCode — POST /channels/join/{join_code}.
func (s *Server) joinChannelByCode(w http.ResponseWriter, r *http.Request) {
	code, ok := parseUUIDParam(w, r, "join_code")
	if !ok {
		return
	}
	c, ok := caller(w, r)
	if !ok {
		return
	}
	ch, err := s.db.channelByJoinCode(r.Context(), code)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "invalid join code")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if ch.ChannelType != ChannelPrivate && ch.ChannelType != ChannelTeam {
		writeError(w, http.StatusForbidden, "join code not valid for this channel")
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if err := s.db.addParticipant(r.Context(), tx, ch.ID, c.UserID, RoleMember); err != nil {
		writeError(w, http.StatusInternalServerError, "join failed")
		return
	}
	_ = s.db.touchChannel(r.Context(), tx, ch.ID)
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"channel_id": ch.ID.String()})
}

// leaveChannel — POST /channels/{channel_id}/leave.
func (s *Server) leaveChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, id)
	if !ok {
		return
	}
	if err := s.db.removeParticipant(r.Context(), id, c.UserID); errors.Is(err, errNotFound) {
		writeError(w, http.StatusBadRequest, "not a participant")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "leave failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// addParticipants — POST /channels/{channel_id}/participants.
func (s *Server) addParticipants(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := s.requireRole(w, r, id, RoleOwner)
	if !ok {
		return
	}
	var req AddParticipantsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	for _, uid := range req.Participants {
		if err := s.db.addParticipant(r.Context(), tx, id, uid, RoleMember); err != nil {
			continue
		}
	}
	_ = s.db.touchChannel(r.Context(), tx, id)
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	if ch, err := s.db.getChannel(r.Context(), id); err == nil && len(req.Participants) > 0 {
		s.fx.sendIngress(r.Context(), ch, &c.UserID, req.Participants, nil,
			"channel_invite", map[string]any{
				"invitedBy":   c.UserID,
				"channelName": chName(ch),
			}, "Channel invite", "You've been invited to a channel")
	}
	w.WriteHeader(http.StatusOK)
}

// removeParticipants — DELETE /channels/{channel_id}/participants.
func (s *Server) removeParticipants(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireRole(w, r, id, RoleOwner); !ok {
		return
	}
	var req RemoveParticipantsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	for _, uid := range req.Participants {
		_ = s.db.removeParticipant(r.Context(), id, uid)
	}
	w.WriteHeader(http.StatusOK)
}

// getParticipants — GET /channels/{channel_id}/participants.
func (s *Server) getParticipants(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, id); !ok {
		return
	}
	parts, err := s.db.participants(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, parts)
}

// getJoinLink — GET /channels/{channel_id}/join-link.
func (s *Server) getJoinLink(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireRole(w, r, id, RoleOwner); !ok {
		return
	}
	ch, err := s.db.getChannel(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	if ch.ChannelType != ChannelPrivate && ch.ChannelType != ChannelTeam {
		writeError(w, http.StatusForbidden, "join links only for private channels")
		return
	}
	code, err := s.db.getOrCreateJoinCode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "join code failed")
		return
	}
	writeJSON(w, http.StatusOK, ChannelJoinCodeResponse{JoinCode: code})
}

// setChannelPicture — PUT /channels/{channel_id}/profile_picture.
func (s *Server) setChannelPicture(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireRole(w, r, id, RoleOwner); !ok {
		return
	}
	var req struct {
		ProfilePictureID uuid.UUID `json:"profile_picture_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.db.setProfilePicture(r.Context(), id, req.ProfilePictureID); err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if uids, err := s.db.participantUserIDs(r.Context(), id); err == nil {
		s.fx.commsChannelPicture(uids, id)
	}
	w.WriteHeader(http.StatusOK)
}

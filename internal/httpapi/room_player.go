package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

type roomPlayerView struct {
	ID       string `json:"id"`
	Online   bool   `json:"online"`
	Device   string `json:"device,omitempty"`
	RoomID   string `json:"room_id,omitempty"`
	Volume   int    `json:"volume"`
	Muted    bool   `json:"muted"`
	Identity string `json:"identity_name,omitempty"`
}

func (s *Server) getRoomPlayer(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok || !s.requireRoomPlayerVolume(w, r, roomID, identity) {
		return
	}
	binding, err := s.st.GetRoomPlayerBinding(r.Context(), roomID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room player is not bound")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room player binding")
		return
	}
	view := roomPlayerView{ID: binding.PlayerID}
	if info, err := s.ws.Player(binding.PlayerID); err == nil {
		view = roomPlayerViewFromInfo(info)
	} else if !errors.Is(err, wsapi.ErrPlayerNotFound) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room player")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"player": view})
}

func (s *Server) bindRoomPlayer(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		PlayerID string `json:"player_id"`
	}
	if err := decodeIntegrationJSON(r, &body); err != nil || anyBlank(body.PlayerID) {
		writeErr(w, http.StatusBadRequest, "bad_request", "player_id is required")
		return
	}
	if _, err := s.st.GetRoom(r.Context(), roomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}
	info, err := s.ws.Player(body.PlayerID)
	if errors.Is(err, wsapi.ErrPlayerNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "player is not online")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	if !hasPlayerCapability(info.Caps, "volume") {
		writeErr(w, http.StatusConflict, "conflict", "player does not support volume control")
		return
	}
	binding, err := s.st.BindRoomPlayer(r.Context(), roomID, body.PlayerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to bind room player")
		return
	}
	if err := s.ws.JoinPlayerRoom(body.PlayerID, roomID); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "player disconnected while binding")
		return
	}
	info, _ = s.ws.Player(body.PlayerID)
	s.audit(r.Context(), actor.ID, "room.player.bind", roomID, map[string]any{"player_id": body.PlayerID})
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": map[string]any{"room_id": binding.RoomID, "player_id": binding.PlayerID},
		"player":  roomPlayerViewFromInfo(info),
	})
}

func (s *Server) unbindRoomPlayer(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	binding, err := s.st.GetRoomPlayerBinding(r.Context(), roomID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room player is not bound")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room player binding")
		return
	}
	if err := s.st.UnbindRoomPlayer(r.Context(), roomID, binding.PlayerID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to unbind room player")
		return
	}
	s.audit(r.Context(), actor.ID, "room.player.unbind", roomID, map[string]any{"player_id": binding.PlayerID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) setRoomPlayerVolume(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok || !s.requireRoomPlayerVolume(w, r, roomID, identity) {
		return
	}
	var body struct {
		Volume *int `json:"volume"`
	}
	if err := decodeIntegrationJSON(r, &body); err != nil ||
		body.Volume == nil || *body.Volume < 0 || *body.Volume > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "volume must be 0-100")
		return
	}
	binding, err := s.st.GetRoomPlayerBinding(r.Context(), roomID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room player is not bound")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room player binding")
		return
	}
	info, err := s.ws.Player(binding.PlayerID)
	if errors.Is(err, wsapi.ErrPlayerNotFound) {
		writeErr(w, http.StatusConflict, "conflict", "bound player is offline")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load bound player")
		return
	}
	if info.RoomID != roomID {
		writeErr(w, http.StatusConflict, "conflict", "bound player is not in the room")
		return
	}
	if err := s.ws.CommandPlayer(binding.PlayerID, "set_volume", *body.Volume); errors.Is(err, wsapi.ErrPlayerCapability) {
		writeErr(w, http.StatusConflict, "conflict", "bound player does not support volume control")
		return
	} else if err != nil {
		writeErr(w, http.StatusConflict, "conflict", "bound player became unavailable")
		return
	}
	s.audit(r.Context(), identity.ID, "room.player.volume", roomID, map[string]any{
		"player_id": binding.PlayerID, "volume": *body.Volume,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "player_id": binding.PlayerID, "volume": *body.Volume,
	})
}

func (s *Server) requireRoomPlayerVolume(
	w http.ResponseWriter,
	r *http.Request,
	roomID string,
	identity auth.Identity,
) bool {
	capabilities, err := s.controls.RoomCapabilities(r.Context(), roomID, identity)
	if err != nil {
		writeControlErr(w, err)
		return false
	}
	if capabilities.Controller {
		return true
	}
	if identity.IntegrationID == "" || identity.IntegrationRoomID != roomID {
		writeErr(w, http.StatusForbidden, "forbidden", "room player volume permission required")
		return false
	}
	row, err := s.st.GetRoom(r.Context(), roomID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room policy")
		return false
	}
	policy, err := room.ParsePolicy(row.PolicyJSON)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "invalid stored room policy")
		return false
	}
	if !policy.MemberPlayerVolume {
		writeErr(w, http.StatusForbidden, "forbidden", "room does not allow member volume control")
		return false
	}
	return true
}

func roomPlayerViewFromInfo(info wsapi.PlayerInfo) roomPlayerView {
	return roomPlayerView{
		ID: info.ID, Online: true, Device: info.Device, RoomID: info.RoomID,
		Volume: info.Volume, Muted: info.Muted, Identity: info.Identity,
	}
}

func hasPlayerCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

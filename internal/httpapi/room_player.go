package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

type roomPlayerView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Bound  bool   `json:"bound"`
	Online bool   `json:"online"`
	Device string `json:"device,omitempty"`
	RoomID string `json:"room_id,omitempty"`
	Volume int    `json:"volume"`
	Muted  bool   `json:"muted"`
}

type roomOutputView struct {
	Volume    *int  `json:"volume"`
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

func (s *Server) getRoomOutput(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok || !s.requireRoomOutputVolume(w, r, roomID, identity) {
		return
	}
	view, err := s.roomOutputView(r, roomID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room output")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": view})
}

func (s *Server) setRoomOutputVolume(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok || !s.requireRoomOutputVolume(w, r, roomID, identity) {
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
	state, err := s.st.SetRoomOutputVolume(r.Context(), roomID, *body.Volume)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to persist room output volume")
		return
	}
	commandsSent := s.ws.CommandRoomPlayers(roomID, "set_volume", state.Volume)
	s.audit(r.Context(), identity.ID, "room.output.volume", roomID, map[string]any{
		"volume": state.Volume, "commands_sent": commandsSent,
	})
	volume := state.Volume
	writeJSON(w, http.StatusOK, map[string]any{
		"output":   roomOutputView{Volume: &volume, UpdatedAt: state.UpdatedAt},
		"delivery": map[string]any{"commands_sent": commandsSent},
	})
}

func (s *Server) listRoomPlayers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.st.GetRoom(r.Context(), roomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}
	bindings, err := s.st.ListRoomPlayerBindings(r.Context(), roomID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room player bindings")
		return
	}
	playersByID := make(map[string]roomPlayerView, len(bindings))
	for _, binding := range bindings {
		player, err := s.st.GetPlayer(r.Context(), binding.PlayerID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load bound player")
			return
		}
		playersByID[binding.PlayerID] = roomPlayerView{
			ID: binding.PlayerID, Name: player.Name, Active: player.Active, Bound: true,
		}
	}
	for _, info := range s.ws.RoomPlayers(roomID) {
		view := roomPlayerViewFromInfo(info)
		if persisted, exists := playersByID[info.ID]; exists {
			view.Name = persisted.Name
			view.Active = persisted.Active
			view.Bound = persisted.Bound
		}
		playersByID[info.ID] = view
	}
	players := make([]roomPlayerView, 0, len(playersByID))
	for _, player := range playersByID {
		players = append(players, player)
	}
	sort.Slice(players, func(i, j int) bool { return players[i].ID < players[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"players": players})
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
	playerID, ok := controlPathValue(w, r, "player_id")
	if !ok {
		return
	}
	if _, err := s.st.GetRoom(r.Context(), roomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}
	player, err := s.st.GetPlayer(r.Context(), playerID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "player not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	binding, err := s.st.BindRoomPlayer(r.Context(), roomID, playerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to bind room player")
		return
	}
	view := roomPlayerView{
		ID: player.ID, Name: player.Name, Active: player.Active, Bound: true, RoomID: roomID,
	}
	if _, onlineErr := s.ws.Player(playerID); onlineErr == nil {
		if s.ws.JoinPlayerRoom(playerID, roomID) == nil {
			if current, currentErr := s.ws.Player(playerID); currentErr == nil {
				view = roomPlayerViewFromInfo(current)
				view.Name = player.Name
				view.Active = player.Active
				view.Bound = true
			}
		}
	} else if !errors.Is(onlineErr, wsapi.ErrPlayerNotFound) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load online player")
		return
	}
	s.audit(r.Context(), actor.ID, "room.player.bind", roomID, map[string]any{"player_id": playerID})
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": map[string]any{"room_id": binding.RoomID, "player_id": binding.PlayerID},
		"player":  view,
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
	playerID, ok := controlPathValue(w, r, "player_id")
	if !ok {
		return
	}
	if err := s.st.UnbindRoomPlayer(r.Context(), roomID, playerID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room player binding not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to unbind room player")
		return
	}
	if info, err := s.ws.Player(playerID); err == nil && info.RoomID == roomID {
		_ = s.ws.LeavePlayerRoom(playerID)
	}
	s.audit(r.Context(), actor.ID, "room.player.unbind", roomID, map[string]any{"player_id": playerID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) roomOutputView(r *http.Request, roomID string) (roomOutputView, error) {
	state, err := s.st.GetRoomOutputState(r.Context(), roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return roomOutputView{}, nil
	}
	if err != nil {
		return roomOutputView{}, err
	}
	volume := state.Volume
	return roomOutputView{Volume: &volume, UpdatedAt: state.UpdatedAt}, nil
}

func (s *Server) requireRoomOutputVolume(
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
		writeErr(w, http.StatusForbidden, "forbidden", "room output volume permission required")
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
		Volume: info.Volume, Muted: info.Muted,
	}
}

package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

var playerResourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type playerResourceView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Active        bool     `json:"active"`
	KeyConfigured bool     `json:"key_configured"`
	Online        bool     `json:"online"`
	RoomID        string   `json:"room_id,omitempty"`
	Device        string   `json:"device,omitempty"`
	Version       string   `json:"version,omitempty"`
	Caps          []string `json:"caps"`
	Volume        int      `json:"volume,omitempty"`
	Muted         bool     `json:"muted,omitempty"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
	LastSeenAt    *int64   `json:"last_seen_at,omitempty"`
	ConnectedAt   int64    `json:"connected_at,omitempty"`
}

func (s *Server) playerResourceView(ctx context.Context, player store.Player, online *wsapi.PlayerInfo) (playerResourceView, error) {
	view := playerResourceView{
		ID: player.ID, Name: player.Name, Active: player.Active,
		KeyConfigured: player.KeyConfigured, Caps: []string{},
		CreatedAt: player.CreatedAt, UpdatedAt: player.UpdatedAt, LastSeenAt: player.LastSeenAt,
	}
	binding, err := s.st.GetRoomPlayerBindingByPlayer(ctx, player.ID)
	if err == nil {
		view.RoomID = binding.RoomID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return playerResourceView{}, err
	}
	if online != nil {
		view.Online = true
		view.Device = online.Device
		view.Version = online.Version
		view.Caps = append([]string(nil), online.Caps...)
		view.Volume = online.Volume
		view.Muted = online.Muted
		view.ConnectedAt = online.ConnectedAt
		if online.RoomID != "" {
			view.RoomID = online.RoomID
		}
	}
	return view, nil
}

func (s *Server) currentPlayerResourceView(ctx context.Context, player store.Player) (playerResourceView, error) {
	var runtime *wsapi.PlayerInfo
	info, err := s.ws.Player(player.ID)
	if err == nil {
		runtime = &info
	} else if !errors.Is(err, wsapi.ErrPlayerNotFound) {
		return playerResourceView{}, err
	}
	return s.playerResourceView(ctx, player, runtime)
}

func (s *Server) listPlayers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	players, err := s.st.ListPlayers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load players")
		return
	}
	online := make(map[string]wsapi.PlayerInfo)
	for _, info := range s.ws.Players() {
		online[info.ID] = info
	}
	views := make([]playerResourceView, 0, len(players))
	for _, player := range players {
		var runtime *wsapi.PlayerInfo
		if info, ok := online[player.ID]; ok {
			runtime = &info
		}
		view, err := s.playerResourceView(r.Context(), player, runtime)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load player binding")
			return
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"players": views})
}

func (s *Server) getPlayer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	player, err := s.st.GetPlayer(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "player not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	view, err := s.currentPlayerResourceView(r.Context(), player)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player binding")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"player": view})
}

func (s *Server) createPlayer(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeIntegrationJSON(r, &body); err != nil ||
		!playerResourceIDPattern.MatchString(body.ID) || strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "valid id and name are required")
		return
	}
	key, hash, err := auth.NewPlayerKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to generate player key")
		return
	}
	player, err := s.st.CreatePlayer(r.Context(), body.ID, strings.TrimSpace(body.Name), hash)
	if err != nil {
		writeErr(w, http.StatusConflict, "conflict", "player id already exists")
		return
	}
	view, err := s.currentPlayerResourceView(r.Context(), player)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	s.audit(r.Context(), actor.ID, "player.create", player.ID, map[string]any{"name": player.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"player": view, "key": key})
}

func (s *Server) updatePlayer(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	playerID := r.PathValue("id")
	current, err := s.st.GetPlayer(r.Context(), playerID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "player not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	var body struct {
		Name   *string `json:"name"`
		Active *bool   `json:"active"`
	}
	if err := decodeIntegrationJSON(r, &body); err != nil || (body.Name == nil && body.Active == nil) {
		writeErr(w, http.StatusBadRequest, "bad_request", "name or active is required")
		return
	}
	name := current.Name
	active := current.Active
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not be blank")
			return
		}
	}
	if body.Active != nil {
		active = *body.Active
	}
	if active && !current.KeyConfigured {
		writeErr(w, http.StatusConflict, "conflict", "rotate a Player key before enabling")
		return
	}
	player, err := s.st.UpdatePlayer(r.Context(), playerID, name, active)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to update player")
		return
	}
	if !player.Active {
		s.ws.DisconnectPlayer(player.ID, "player disabled")
	}
	view, err := s.currentPlayerResourceView(r.Context(), player)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	s.audit(r.Context(), actor.ID, "player.update", player.ID, map[string]any{
		"name": player.Name, "active": player.Active,
	})
	writeJSON(w, http.StatusOK, map[string]any{"player": view})
}

func (s *Server) rotatePlayerKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	key, hash, err := auth.NewPlayerKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to generate player key")
		return
	}
	player, err := s.st.UpdatePlayerToken(r.Context(), r.PathValue("id"), hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "player not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to rotate player key")
		return
	}
	s.ws.DisconnectPlayer(player.ID, "player key rotated")
	view, err := s.currentPlayerResourceView(r.Context(), player)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load player")
		return
	}
	s.audit(r.Context(), actor.ID, "player.key.rotate", player.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"player": view, "key": key})
}

func (s *Server) deletePlayer(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	playerID := r.PathValue("id")
	if err := s.st.DeletePlayer(r.Context(), playerID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "player not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to delete player")
		return
	}
	s.ws.DisconnectPlayer(playerID, "player deleted")
	s.audit(r.Context(), actor.ID, "player.delete", playerID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

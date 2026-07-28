package wsapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

// ---------- 播放端管理平面（player plane） ----------
//
// 无头渲染端（嵌入式播放器）连上 WS 后经 player.hello 注册为可管理
// 播放端；管理指令（音量/静音）经 WS player.command 下发；换房间由
// 服务端直接迁移连接（room.Join 自动推快照，agent 原样重渲染，
// 无需房间密码——服务端即权威）。

// PlayerInfo 播放端快照（REST 展示）。
type PlayerInfo struct {
	ID          string   `json:"id"`
	Device      string   `json:"device"`
	Version     string   `json:"version,omitempty"`
	Caps        []string `json:"caps"`
	IdentityID  string   `json:"identity_id"`
	Identity    string   `json:"identity_name"`
	RoomID      string   `json:"room_id,omitempty"`
	Volume      int      `json:"volume"`
	Muted       bool     `json:"muted"`
	ConnectedAt int64    `json:"connected_at"`
}

type playerState struct {
	id          string
	device      string
	version     string
	caps        []string
	volume      int
	muted       bool
	connectedAt int64
}

var (
	ErrPlayerNotFound   = errors.New("player not found")
	ErrPlayerCapability = errors.New("player capability unavailable")
	ErrNotAPlayer       = errors.New("connection is not a registered player")
)

// Players 全部在线播放端快照。
func (s *Server) Players() []PlayerInfo {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()
	out := make([]PlayerInfo, 0, len(s.players))
	for _, c := range s.players {
		out = append(out, playerInfo(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RoomPlayers 返回当前位于 Room 的全部在线 headless player。
func (s *Server) RoomPlayers(roomID string) []PlayerInfo {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()
	var out []PlayerInfo
	for _, c := range s.players {
		if c.room != nil && c.room.ID == roomID {
			out = append(out, playerInfo(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) Player(id string) (PlayerInfo, error) {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()
	c, ok := s.players[id]
	if !ok {
		return PlayerInfo{}, ErrPlayerNotFound
	}
	return playerInfo(c), nil
}

func playerInfo(c *client) PlayerInfo {
	info := PlayerInfo{
		ID: c.player.id, Device: c.player.device, Version: c.player.version,
		Caps:   append([]string(nil), c.player.caps...),
		Volume: c.player.volume, Muted: c.player.muted,
		ConnectedAt: c.player.connectedAt,
	}
	if c.identity != nil {
		info.IdentityID = c.identity.ID
		info.Identity = c.identity.Name
	}
	if c.room != nil {
		info.RoomID = c.room.ID
	}
	return info
}

// CommandPlayer 向播放端转发控制指令（set_volume / set_mute）。
func (s *Server) CommandPlayer(id, op string, value any) error {
	c, err := s.playerConn(id)
	if err != nil {
		return err
	}
	requiredCapability := map[string]string{"set_volume": "volume", "set_mute": "mute"}[op]
	if requiredCapability != "" && !playerHasCapability(c.player, requiredCapability) {
		return ErrPlayerCapability
	}
	c.Send(map[string]any{
		"type": "player.command",
		"data": map[string]any{"op": op, "value": value},
	})
	return nil
}

// CommandRoomPlayers 向当前位于 Room 且声明了对应 capability 的全部
// headless player 下发命令，返回已入队的目标数量。
func (s *Server) CommandRoomPlayers(roomID, op string, value any) int {
	requiredCapability := map[string]string{"set_volume": "volume", "set_mute": "mute"}[op]
	s.playersMu.Lock()
	targets := make([]*client, 0, len(s.players))
	for _, c := range s.players {
		if c.room == nil || c.room.ID != roomID {
			continue
		}
		if requiredCapability != "" && !playerHasCapability(c.player, requiredCapability) {
			continue
		}
		targets = append(targets, c)
	}
	s.playersMu.Unlock()
	message := map[string]any{
		"type": "player.command",
		"data": map[string]any{"op": op, "value": value},
	}
	for _, c := range targets {
		c.Send(message)
	}
	return len(targets)
}

func playerHasCapability(player *playerState, capability string) bool {
	for _, current := range player.caps {
		if current == capability {
			return true
		}
	}
	return false
}

// JoinPlayerRoom 服务端直接迁移播放端到目标房间（快照由房间 actor 推送）。
func (s *Server) JoinPlayerRoom(id, roomID string) error {
	c, err := s.playerConn(id)
	if err != nil {
		return err
	}
	r, err := s.control.GetRoom(roomID)
	if err != nil {
		return fmt.Errorf("room not found: %s", roomID)
	}
	if c.room != nil {
		c.room.Leave(c)
	}
	c.room = r
	c.Send(map[string]any{"type": "room.joined", "data": map[string]any{"room_id": roomID}})
	r.Join(c)
	s.applyRoomOutput(c)
	return nil
}

// LeavePlayerRoom removes an online Player from its assigned Room and tells the
// Agent to stop rendering the previous Room.
func (s *Server) LeavePlayerRoom(id string) error {
	c, err := s.playerConn(id)
	if err != nil {
		return err
	}
	if c.room != nil {
		c.room.Leave(c)
		c.room = nil
	}
	c.Send(map[string]any{"type": "room.left", "data": map[string]any{}})
	return nil
}

// DisconnectPlayer immediately revokes an online Player connection. The map
// entry is removed before closing so a reconnect with a newly rotated key wins.
func (s *Server) DisconnectPlayer(id, reason string) bool {
	s.playersMu.Lock()
	c, ok := s.players[id]
	if ok {
		delete(s.players, id)
	}
	s.playersMu.Unlock()
	if ok {
		c.disconnect(reason)
	}
	return ok
}

func (s *Server) applyRoomOutput(c *client) {
	if s.playerBindings == nil || c.player == nil || c.room == nil ||
		!playerHasCapability(c.player, "volume") {
		return
	}
	state, err := s.playerBindings.GetRoomOutputState(context.Background(), c.room.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("player %s: load room %s output: %v", c.player.id, c.room.ID, err)
		return
	}
	c.Send(map[string]any{
		"type": "player.command",
		"data": map[string]any{"op": "set_volume", "value": state.Volume},
	})
}

func (s *Server) playerConn(id string) (*client, error) {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()
	c, ok := s.players[id]
	if !ok {
		return nil, ErrPlayerNotFound
	}
	return c, nil
}

func (s *Server) registerPlayer(c *client, ps *playerState) {
	s.playersMu.Lock()
	previous := s.players[ps.id]
	s.players[ps.id] = c
	s.playersMu.Unlock()
	if previous != nil && previous != c {
		previous.disconnect("player reconnected")
	}
}

func (s *Server) unregisterPlayer(c *client) {
	s.playersMu.Lock()
	if c.player != nil && s.players[c.player.id] == c {
		delete(s.players, c.player.id)
	}
	s.playersMu.Unlock()
}

// ---------- WS 消息处理 ----------

func (c *client) handlePlayerHello(ref string, data json.RawMessage) {
	if c.identity == nil || c.identity.PlayerID == "" || c.playerKey == "" {
		c.replyErr(ref, "forbidden", "player_key authentication required")
		return
	}
	var d struct {
		Device  string   `json:"device"`
		Version string   `json:"version"`
		Caps    []string `json:"caps"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Device == "" {
		c.replyErr(ref, "bad_request", "player.hello requires device")
		return
	}
	if c.player != nil {
		c.replyErr(ref, "bad_request", "already registered as player")
		return
	}

	playerID := c.identity.PlayerID
	boundRoomID := ""
	if c.server.playerBindings != nil {
		binding, err := c.server.playerBindings.GetRoomPlayerBindingByPlayer(
			context.Background(), playerID,
		)
		if err == nil {
			boundRoomID = binding.RoomID
		} else if !errors.Is(err, sql.ErrNoRows) {
			c.replyErr(ref, "internal", "failed to resolve player room binding")
			return
		}
	}

	player := &playerState{
		id: playerID, device: d.Device, version: d.Version,
		caps:   append([]string(nil), d.Caps...),
		volume: 100, connectedAt: time.Now().UnixMilli(),
	}
	c.player = player
	c.server.registerPlayer(c, player)

	current, err := c.server.playerAuth.ValidateKey(context.Background(), c.playerKey)
	if err != nil || current.ID != playerID {
		c.server.unregisterPlayer(c)
		c.player = nil
		c.replyErr(ref, "unauthorized", "player key changed during registration")
		c.disconnect("player credential revoked")
		return
	}
	identity := auth.PlayerIdentity(current)
	c.identity = &identity
	c.Send(map[string]any{
		"type": "player.hello.ok", "ref": ref,
		"data": map[string]any{"player_id": player.id},
	})
	if boundRoomID != "" {
		if c.server.JoinPlayerRoom(player.id, boundRoomID) == nil {
			return
		}
	}
	c.server.applyRoomOutput(c)
}

func (c *client) handlePlayerState(data json.RawMessage) {
	if c.player == nil {
		return
	}
	var d struct {
		Volume *int  `json:"volume"`
		Muted  *bool `json:"muted"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	c.server.playersMu.Lock()
	if d.Volume != nil {
		c.player.volume = *d.Volume
	}
	if d.Muted != nil {
		c.player.muted = *d.Muted
	}
	c.server.playersMu.Unlock()
}

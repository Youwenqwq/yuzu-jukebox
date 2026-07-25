package wsapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	Identity    string   `json:"identity_name"`
	RoomID      string   `json:"room_id,omitempty"`
	Volume      int      `json:"volume"`
	Muted       bool     `json:"muted"`
	ConnectedAt int64    `json:"connected_at"`
}

type playerState struct {
	device      string
	version     string
	caps        []string
	volume      int
	muted       bool
	connectedAt int64
}

var (
	ErrPlayerNotFound = errors.New("player not found")
	ErrNotAPlayer     = errors.New("connection is not a registered player")
)

// Players 全部在线播放端快照。
func (s *Server) Players() []PlayerInfo {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()
	out := make([]PlayerInfo, 0, len(s.players))
	for _, c := range s.players {
		info := PlayerInfo{
			ID: c.id, Device: c.player.device, Version: c.player.version,
			Caps: c.player.caps, Volume: c.player.volume, Muted: c.player.muted,
			ConnectedAt: c.player.connectedAt,
		}
		if c.identity != nil {
			info.Identity = c.identity.Name
		}
		if c.room != nil {
			info.RoomID = c.room.ID
		}
		out = append(out, info)
	}
	return out
}

// CommandPlayer 向播放端转发控制指令（set_volume / set_mute）。
func (s *Server) CommandPlayer(id, op string, value any) error {
	c, err := s.playerConn(id)
	if err != nil {
		return err
	}
	c.Send(map[string]any{
		"type": "player.command",
		"data": map[string]any{"op": op, "value": value},
	})
	return nil
}

// JoinPlayerRoom 服务端直接迁移播放端到目标房间（快照由房间 actor 推送）。
func (s *Server) JoinPlayerRoom(id, roomID string) error {
	c, err := s.playerConn(id)
	if err != nil {
		return err
	}
	r, err := s.rooms.Get(roomID)
	if err != nil {
		return fmt.Errorf("room not found: %s", roomID)
	}
	if c.room != nil {
		c.room.Leave(c)
	}
	c.room = r
	c.Send(map[string]any{"type": "room.joined", "data": map[string]any{"room_id": roomID}})
	r.Join(c)
	return nil
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
	s.players[c.id] = c
	s.playersMu.Unlock()
}

func (s *Server) unregisterPlayer(c *client) {
	s.playersMu.Lock()
	delete(s.players, c.id)
	s.playersMu.Unlock()
}

// ---------- WS 消息处理 ----------

func (c *client) handlePlayerHello(ref string, data json.RawMessage) {
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
	c.player = &playerState{
		device: d.Device, version: d.Version, caps: d.Caps,
		volume: 100, connectedAt: time.Now().UnixMilli(),
	}
	c.server.registerPlayer(c, c.player)
	c.Send(map[string]any{
		"type": "player.hello.ok", "ref": ref,
		"data": map[string]any{"player_id": c.id},
	})
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

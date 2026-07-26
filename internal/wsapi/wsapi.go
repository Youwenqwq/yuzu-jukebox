// Package wsapi 实现 /ws/v1：实时会话通道。
// 职责：校时、认证、房间进出、队列/播放操作的鉴权与转发。
// 状态全部在 room actor 内，这里只做协议适配。
package wsapi

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

type Server struct {
	authm *auth.Manager
	rooms *room.Manager
	reg   *provider.Registry

	playersMu sync.Mutex
	players   map[string]*client // 已注册的播放端（player.hello）
}

func NewServer(authm *auth.Manager, rooms *room.Manager, reg *provider.Registry) *Server {
	return &Server{authm: authm, rooms: rooms, reg: reg, players: map[string]*client{}}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // v1 放开跨域；部署时按需要收紧
	})
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	c := &client{
		server: s,
		conn:   conn,
		id:     newClientID(),
		remote: r.RemoteAddr,
		send:   make(chan any, 64),
	}
	go c.writeLoop()
	c.readLoop()
}

// ---------- 客户端 ----------

type client struct {
	server   *Server
	conn     *websocket.Conn
	id       string
	remote   string
	identity *auth.Identity
	room     *room.Room
	player   *playerState // 非 nil = 已注册播放端
	send     chan any

	closeOnce chan struct{}
}

func (c *client) ID() string { return c.id }

var clientIDCounter uint64

func newClientID() string {
	b := make([]byte, 6)
	crand.Read(b)
	return "c_" + hex.EncodeToString(b)
}

func (c *client) Identity() auth.Identity {
	if c.identity == nil {
		return auth.Identity{}
	}
	return *c.identity
}

// Send 实现 room.ClientConn：非阻塞；缓冲满则断开慢客户端。
func (c *client) Send(msg any) {
	select {
	case c.send <- msg:
	default:
		c.close()
	}
}

func (c *client) close() {
	select {
	case <-c.closeOnce:
	default:
		close(c.closeOnce)
		c.conn.Close(websocket.StatusGoingAway, "slow consumer")
	}
}

func (c *client) writeLoop() {
	ctx := context.Background()
	for msg := range c.send {
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = c.conn.Write(wctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			return
		}
	}
}

func (c *client) readLoop() {
	ctx := context.Background()
	defer func() {
		if c.room != nil {
			c.room.Leave(c)
		}
		if c.player != nil {
			c.server.unregisterPlayer(c)
		}
		c.conn.Close(websocket.StatusNormalClosure, "")
		close(c.send)
	}()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type string          `json:"type"`
			Ref  string          `json:"ref"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			c.replyErr("", "bad_request", "invalid message envelope")
			continue
		}
		c.dispatch(msg.Type, msg.Ref, msg.Data)
	}
}

// ---------- 协议处理 ----------

func (c *client) dispatch(typ, ref string, data json.RawMessage) {
	switch typ {
	case "ping":
		var d struct {
			ClientTime int64 `json:"client_time"`
		}
		json.Unmarshal(data, &d)
		c.Send(map[string]any{
			"type": "pong", "ref": ref,
			"data": map[string]any{"client_time": d.ClientTime, "server_time": time.Now().UnixMilli()},
		})

	case "auth":
		var d struct {
			Name         string `json:"name"`
			Password     string `json:"password"`      // 全局管理员口令（可选）
			SessionToken string `json:"session_token"` // 已有会话（REST 登录所得，可选）
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid auth payload")
			return
		}
		// 两条路径：session_token（REST 登录过，如 OIDC）或 guest 现场认证
		if d.SessionToken != "" {
			id, err := c.server.authm.Session(d.SessionToken)
			if err != nil {
				c.replyErr(ref, "unauthorized", "invalid or expired session_token")
				return
			}
			c.identity = &id
			via := "guest-password"
			if id.Kind == "oidc" {
				via = "oidc"
			}
			auth.LogAdminGrant(id, via, c.remote)
			c.Send(map[string]any{
				"type": "auth.ok", "ref": ref,
				"data": map[string]any{"identity": id, "session_token": d.SessionToken},
			})
			return
		}
		id, token, err := c.server.authm.GuestAuth(d.Name, d.Password, c.remote)
		if err != nil {
			if errors.Is(err, auth.ErrPasswordProbeRateLimited) {
				c.replyErr(ref, "rate_limited", err.Error())
				return
			}
			c.replyErr(ref, "bad_request", err.Error())
			return
		}
		c.identity = &id
		auth.LogAdminGrant(id, "guest-password", c.remote)
		c.Send(map[string]any{
			"type": "auth.ok", "ref": ref,
			"data": map[string]any{"identity": id, "session_token": token},
		})

	case "player.hello":
		if !c.requireAuth(ref) {
			return
		}
		c.handlePlayerHello(ref, data)

	case "player.state":
		c.handlePlayerState(data)

	case "room.join":
		var d struct {
			RoomID   string `json:"room_id"`
			Password string `json:"password"` // 房间访客密码
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid join payload")
			return
		}
		if !c.requireAuth(ref) {
			return
		}
		r, err := c.server.rooms.Get(d.RoomID)
		if err != nil {
			c.replyErr(ref, "not_found", "room not found")
			return
		}
		if !r.CheckPassword(d.Password) {
			c.replyErr(ref, "forbidden", "wrong room password")
			return
		}
		if c.room != nil {
			c.room.Leave(c)
		}
		c.room = r
		c.Send(map[string]any{"type": "room.joined", "ref": ref, "data": map[string]any{"room_id": d.RoomID}})
		r.Join(c) // 快照由 actor 随后推送

	case "room.leave":
		if c.room == nil {
			c.replyErr(ref, "bad_request", "not in a room")
			return
		}
		c.room.Leave(c)
		c.room = nil
		c.Send(map[string]any{"type": "room.left", "ref": ref, "data": map[string]any{}})

	case "queue.add":
		var d struct {
			RoomID    string   `json:"room_id"`
			TrackRef  string   `json:"track_ref"`
			TrackRefs []string `json:"track_refs"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.add payload")
			return
		}
		// 双形态：track_ref（单条）或 track_refs（1–100 条原子批量），二选一
		var refs []string
		switch {
		case d.TrackRef != "" && len(d.TrackRefs) > 0:
			c.replyErr(ref, "bad_request", "track_ref and track_refs are mutually exclusive")
			return
		case d.TrackRef != "":
			refs = []string{d.TrackRef}
		case len(d.TrackRefs) > 0:
			refs = d.TrackRefs
		default:
			c.replyErr(ref, "bad_request", "track_ref or track_refs required")
			return
		}
		if len(refs) > 100 {
			c.replyErr(ref, "bad_request", "track_refs limited to 100")
			return
		}
		if !c.requireRole(ref, auth.RoleRequester) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		// 整体预取：任一 ref 无效或 provider 失败，一条都不入队
		entries := make([]room.QueueEntry, 0, len(refs))
		for _, tr := range refs {
			p, _, err := c.server.reg.ForRef(provider.TrackRef(tr))
			if err != nil {
				c.replyErr(ref, "bad_request", err.Error())
				return
			}
			track, err := p.GetTrack(context.Background(), provider.TrackRef(tr))
			if err != nil {
				c.replyErr(ref, "provider_error", err.Error())
				return
			}
			entries = append(entries, room.EntryFromTrack(track, c.identity.ID))
		}
		if err := c.room.AddBatchFor(c.Identity(), entries); err != nil {
			c.replyResult(ref, err)
			return
		}
		ids := make([]string, len(entries))
		for i, e := range entries {
			ids[i] = e.EntryID
		}
		c.Send(map[string]any{"type": "ack", "ref": ref, "data": map[string]any{"entry_ids": ids}})

	case "queue.remove":
		var d struct {
			RoomID  string `json:"room_id"`
			EntryID string `json:"entry_id"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.remove payload")
			return
		}
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		// 权限：room_admin 或条目所有者（actor 内校验）
		err := c.room.RemoveFor(c.Identity(), d.EntryID)
		c.replyResult(ref, err)

	case "queue.move":
		var d struct {
			RoomID  string `json:"room_id"`
			EntryID string `json:"entry_id"`
			ToIndex int    `json:"to_index"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.move payload")
			return
		}
		if !c.requireRole(ref, auth.RoleRoomAdmin) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.room.Move(d.EntryID, d.ToIndex))

	case "radio.play":
		var d struct {
			RoomID  string `json:"room_id"`
			Source  string `json:"source"`
			Shuffle bool   `json:"shuffle"`
			Once    bool   `json:"once"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid radio.play payload")
			return
		}
		if !c.requireRole(ref, auth.RoleRoomAdmin) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.room.PlayRadio(d.Source, d.Shuffle, d.Once))

	case "radio.stop":
		var d struct {
			RoomID string `json:"room_id"`
		}
		json.Unmarshal(data, &d)
		if !c.requireRole(ref, auth.RoleRoomAdmin) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.room.StopRadio())

	case "playback.pause", "playback.resume", "playback.skip", "playback.seek":
		var d struct {
			RoomID     string `json:"room_id"`
			PositionMs int64  `json:"position_ms"`
		}
		json.Unmarshal(data, &d)
		if !c.requireRole(ref, auth.RoleRoomAdmin) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		var err error
		switch typ {
		case "playback.pause":
			err = c.room.Pause()
		case "playback.resume":
			err = c.room.Resume()
		case "playback.skip":
			err = c.room.Skip()
		case "playback.seek":
			err = c.room.SeekTo(d.PositionMs)
		}
		c.replyResult(ref, err)

	default:
		c.replyErr(ref, "bad_request", "unknown message type: "+typ)
	}
}

// ---------- 辅助 ----------

func (c *client) requireAuth(ref string) bool {
	if c.identity == nil {
		c.replyErr(ref, "unauthorized", "auth first")
		return false
	}
	return true
}

func (c *client) requireRole(ref, role string) bool {
	if !c.requireAuth(ref) {
		return false
	}
	if !c.identity.HasRole(role) {
		c.replyErr(ref, "forbidden", "role required: "+role)
		return false
	}
	return true
}

func (c *client) requireRoom(ref, roomID string) bool {
	if c.room == nil {
		c.replyErr(ref, "bad_request", "not in a room")
		return false
	}
	if roomID != "" && c.room.ID != roomID {
		c.replyErr(ref, "bad_request", "room_id mismatch")
		return false
	}
	return true
}

func (c *client) replyResult(ref string, err error) {
	if err == nil {
		c.Send(map[string]any{"type": "ack", "ref": ref, "data": map[string]any{}})
		return
	}
	code := "internal"
	switch {
	case errors.Is(err, room.ErrEntryNotFound):
		code = "not_found"
	case errors.Is(err, room.ErrNoPlayback), errors.Is(err, room.ErrQueueEmpty), errors.Is(err, room.ErrInvalidSource),
		errors.Is(err, room.ErrInvalidPolicy):
		code = "bad_request"
	case errors.Is(err, room.ErrQueueFull):
		code = "queue_full"
	case errors.Is(err, room.ErrQuotaExceeded):
		code = "quota_exceeded"
	case errors.Is(err, room.ErrForbidden):
		code = "forbidden"
	}
	c.replyErr(ref, code, err.Error())
}

func (c *client) replyErr(ref, code, message string) {
	c.Send(map[string]any{
		"type": "error", "ref": ref,
		"data": map[string]any{"code": code, "message": message},
	})
}

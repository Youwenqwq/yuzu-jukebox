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
}

func NewServer(authm *auth.Manager, rooms *room.Manager, reg *provider.Registry) *Server {
	return &Server{authm: authm, rooms: rooms, reg: reg}
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
	identity *auth.Identity
	room     *room.Room
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
			Name     string `json:"name"`
			Password string `json:"password"` // 全局管理员口令（可选）
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid auth payload")
			return
		}
		id, token, err := c.server.authm.GuestAuth(d.Name, d.Password)
		if err != nil {
			c.replyErr(ref, "bad_request", err.Error())
			return
		}
		c.identity = &id
		c.Send(map[string]any{
			"type": "auth.ok", "ref": ref,
			"data": map[string]any{"identity": id, "session_token": token},
		})

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
		r.Join(c) // 快照由 actor 直接推送给该客户端
		c.Send(map[string]any{"type": "room.joined", "ref": ref, "data": map[string]any{"room_id": d.RoomID}})

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
			RoomID   string `json:"room_id"`
			TrackRef string `json:"track_ref"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.add payload")
			return
		}
		if !c.requireRole(ref, auth.RoleRequester) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		p, _, err := c.server.reg.ForRef(provider.TrackRef(d.TrackRef))
		if err != nil {
			c.replyErr(ref, "bad_request", err.Error())
			return
		}
		track, err := p.GetTrack(context.Background(), provider.TrackRef(d.TrackRef))
		if err != nil {
			c.replyErr(ref, "provider_error", err.Error())
			return
		}
		err = c.room.Add(room.QueueEntry{
			EntryID:     room.NewEntryID(),
			TrackRef:    track.Ref.String(),
			Title:       track.Title,
			Artist:      track.Artist,
			DurationMs:  track.DurationMs,
			RequestedBy: c.identity.ID,
			AddedAt:     time.Now().UnixMilli(),
		})
		c.replyResult(ref, err)

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
	case errors.Is(err, room.ErrNoPlayback), errors.Is(err, room.ErrQueueEmpty):
		code = "bad_request"
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


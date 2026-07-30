// Package wsapi implements the /ws/v1 transport adapter.
// It owns envelopes and connection state; room authorization and commands live in control.Service.
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
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type PlayerBindingStore interface {
	GetRoomPlayerBindingByPlayer(ctx context.Context, playerID string) (store.RoomPlayerBinding, error)
	GetRoomOutputState(ctx context.Context, roomID string) (store.RoomOutputState, error)
}

type Server struct {
	authm          *auth.Manager
	playerAuth     *auth.PlayerRegistry
	control        *control.Service
	playerBindings PlayerBindingStore
	accessProbes   *accessProbeLimiter

	playersMu sync.Mutex
	players   map[string]*client // 已注册的播放端（player.hello）
}

func NewServer(
	authm *auth.Manager,
	playerAuth *auth.PlayerRegistry,
	controlService *control.Service,
	playerBindings PlayerBindingStore,
) *Server {
	return &Server{
		authm: authm, playerAuth: playerAuth, control: controlService, playerBindings: playerBindings,
		accessProbes: newAccessProbeLimiter(),
		players:      map[string]*client{},
	}
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
		server:    s,
		conn:      conn,
		id:        newClientID(),
		remote:    r.RemoteAddr,
		send:      make(chan any, 64),
		closeOnce: make(chan struct{}),
	}
	go c.writeLoop()
	c.readLoop()
}

// ---------- 客户端 ----------

type client struct {
	server    *Server
	conn      *websocket.Conn
	id        string
	remote    string
	identity  *auth.Identity
	playerKey string
	room      *room.Room
	player    *playerState // 非 nil = 已注册播放端
	send      chan any

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

// Interests 实现 room.ClientConn。
// 普通 Client / Integration 默认 InterestAll；
// Player-key 会话只订阅 playback，避免长队列撑爆 WS 帧。
func (c *client) Interests() room.RoomInterest {
	if c.identity != nil && (c.identity.PlayerID != "" || c.identity.Kind == "player") {
		return room.InterestPlayback
	}
	return room.InterestAll
}

// Send 实现 room.ClientConn：非阻塞；缓冲满则断开慢客户端。
func (c *client) Send(msg any) {
	select {
	case <-c.closeOnce:
		return
	case c.send <- msg:
	default:
		c.close()
	}
}

func (c *client) close() {
	c.disconnect("slow consumer")
}

func (c *client) disconnect(reason string) {
	select {
	case <-c.closeOnce:
	default:
		close(c.closeOnce)
		c.conn.Close(websocket.StatusGoingAway, reason)
	}
}

func (c *client) writeLoop() {
	ctx := context.Background()
	for {
		var msg any
		select {
		case <-c.closeOnce:
			return
		case msg = <-c.send:
		}
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
		c.disconnect("connection closed")
		if c.room != nil {
			c.room.Leave(c)
		}
		if c.player != nil {
			c.server.unregisterPlayer(c)
		}
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
	if c.identity != nil && c.identity.PlayerID != "" {
		switch typ {
		case "ping", "player.hello", "player.state":
		default:
			c.replyErr(ref, "forbidden", "Player connections only accept player-plane messages")
			return
		}
	}
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
			PlayerKey    string `json:"player_key"`    // Headless Player 凭据
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid auth payload")
			return
		}
		if d.PlayerKey != "" {
			if d.SessionToken != "" || d.Name != "" || d.Password != "" {
				c.replyErr(ref, "bad_request", "player_key cannot be combined with user authentication")
				return
			}
			player, err := c.server.playerAuth.ResolveKey(context.Background(), d.PlayerKey)
			if err != nil {
				c.replyErr(ref, "unauthorized", "invalid player_key")
				return
			}
			id := auth.PlayerIdentity(player)
			c.identity = &id
			c.playerKey = d.PlayerKey
			c.Send(map[string]any{
				"type": "auth.ok", "ref": ref,
				"data": map[string]any{"identity": id},
			})
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
			Password string `json:"password"` // 房间访问凭据：静态密码或动态验证码
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid join payload")
			return
		}
		if !c.requireAuth(ref) {
			return
		}
		admission, err := c.server.control.AdmitRoom(
			context.Background(), d.RoomID, c.Identity(), d.Password,
		)
		if admission.CredentialChecked {
			matched := err == nil
			if !c.server.accessProbes.allow(d.RoomID, c.remote, matched) {
				c.replyErr(ref, "rate_limited", "too many incorrect room access attempts; try again later")
				return
			}
		}
		if err != nil {
			if errors.Is(err, room.ErrRoomNotFound) {
				c.replyErr(ref, "not_found", "room not found")
			} else {
				c.replyResult(ref, err)
			}
			return
		}
		r := admission.Room
		if c.room != nil {
			c.room.Leave(c)
		}
		c.room = r
		c.Send(map[string]any{"type": "room.joined", "ref": ref, "data": map[string]any{"room_id": d.RoomID}})
		r.Join(c) // 快照由 actor 随后推送
		if c.player != nil {
			c.server.applyRoomOutput(c)
		}

	case "room.leave":
		if c.room == nil {
			c.replyErr(ref, "bad_request", "not in a room")
			return
		}
		c.room.Leave(c)
		c.room = nil
		c.Send(map[string]any{"type": "room.left", "ref": ref, "data": map[string]any{}})

	case "queue.sync":
		var d struct {
			RoomID string `json:"room_id"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.sync payload")
			return
		}
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.room.SyncQueue(c))

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
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		ids, err := c.server.control.QueueAdd(context.Background(), d.RoomID, c.Identity(), refs)
		if err != nil {
			c.replyResult(ref, err)
			return
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
		c.replyResult(ref, c.server.control.QueueRemove(
			context.Background(), d.RoomID, c.Identity(), d.EntryID,
		))

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
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.server.control.QueueMove(
			context.Background(), d.RoomID, c.Identity(), d.EntryID, d.ToIndex,
		))

	case "queue.clear":
		var d struct {
			RoomID string `json:"room_id"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			c.replyErr(ref, "bad_request", "invalid queue.clear payload")
			return
		}
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.server.control.QueueClear(
			context.Background(), d.RoomID, c.Identity(),
		))

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
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.server.control.RadioPlay(
			context.Background(), d.RoomID, c.Identity(), d.Source, d.Shuffle, d.Once,
		))

	case "radio.stop":
		var d struct {
			RoomID string `json:"room_id"`
		}
		json.Unmarshal(data, &d)
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		c.replyResult(ref, c.server.control.RadioStop(context.Background(), d.RoomID, c.Identity()))

	case "playback.pause", "playback.resume", "playback.skip", "playback.seek":
		var d struct {
			RoomID     string `json:"room_id"`
			PositionMs int64  `json:"position_ms"`
		}
		json.Unmarshal(data, &d)
		if !c.requireAuth(ref) || !c.requireRoom(ref, d.RoomID) {
			return
		}
		ctx := context.Background()
		var err error
		switch typ {
		case "playback.pause":
			err = c.server.control.Pause(ctx, d.RoomID, c.Identity())
		case "playback.resume":
			err = c.server.control.Resume(ctx, d.RoomID, c.Identity())
		case "playback.skip":
			err = c.server.control.Skip(ctx, d.RoomID, c.Identity())
		case "playback.seek":
			err = c.server.control.Seek(ctx, d.RoomID, c.Identity(), d.PositionMs)
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
	case errors.Is(err, control.ErrProvider):
		code = "provider_error"
	case errors.Is(err, control.ErrInvalidArgument):
		code = "bad_request"
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

// Package client 是 yuzu-jukebox 的 Go 协议客户端库。
// 实现 spec-v1 的 WS 会话：校时、认证、房间进出、事件流、队列/播放操作。
// yuzu-agent（播放代理）与 yuzu-cli（控制端）共用此库。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Message 是一条 WS 消息（信封）。
type Message struct {
	Type string          `json:"type"`
	Ref  string          `json:"ref"`
	Data json.RawMessage `json:"data"`
}

// ErrorMsg 是 error 消息的 data。
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e ErrorMsg) Error() string { return e.Code + ": " + e.Message }

type Client struct {
	conn *websocket.Conn

	wmu sync.Mutex // 写串行化

	refSeq  atomic.Uint64
	pmu     sync.Mutex
	pending map[string]chan Message

	events   chan Message
	offsetMs int64 // server_now = local_now + offsetMs
	synced   bool
}

// Dial 建立 WS 连接并启动读泵。server 为 HTTP 基址，如 "http://127.0.0.1:8080"。
func Dial(ctx context.Context, server string) (*Client, error) {
	wsURL := strings.Replace(server, "http", "ws", 1) + "/ws/v1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	c := &Client{
		conn:    conn,
		pending: map[string]chan Message{},
		events:  make(chan Message, 32),
	}
	go c.readPump()
	return c, nil
}

func (c *Client) Close() {
	c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *Client) readPump() {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			// 连接断开：唤醒所有 pending，关闭事件流
			c.pmu.Lock()
			for ref, ch := range c.pending {
				ch <- Message{Type: "error", Data: json.RawMessage(`{"code":"closed","message":"connection closed"}`)}
				delete(c.pending, ref)
			}
			c.pmu.Unlock()
			close(c.events)
			return
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Ref != "" {
			c.pmu.Lock()
			if ch, ok := c.pending[m.Ref]; ok {
				ch <- m
				delete(c.pending, m.Ref)
				c.pmu.Unlock()
				continue
			}
			c.pmu.Unlock()
		}
		// 广播类消息（ack 之外的 ref 消息也按事件处理）
		c.events <- m
	}
}

// Events 返回广播事件流（playback.changed / queue.changed / listeners.changed 等）。
func (c *Client) Events() <-chan Message { return c.events }

// call 发送请求并等待对应 ref 的响应。
func (c *Client) call(ctx context.Context, typ string, data any) (Message, error) {
	ref := fmt.Sprintf("r%d", c.refSeq.Add(1))
	payload, err := json.Marshal(map[string]any{"type": typ, "ref": ref, "data": data})
	if err != nil {
		return Message{}, err
	}
	ch := make(chan Message, 1)
	c.pmu.Lock()
	c.pending[ref] = ch
	c.pmu.Unlock()

	c.wmu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, payload)
	c.wmu.Unlock()
	if err != nil {
		c.pmu.Lock()
		delete(c.pending, ref)
		c.pmu.Unlock()
		return Message{}, err
	}

	select {
	case m := <-ch:
		if m.Type == "error" {
			var em ErrorMsg
			json.Unmarshal(m.Data, &em)
			return m, em
		}
		return m, nil
	case <-ctx.Done():
		c.pmu.Lock()
		delete(c.pending, ref)
		c.pmu.Unlock()
		return Message{}, ctx.Err()
	}
}

// ---------- 校时 ----------

// ClockSync 进行 rounds 轮 ping/pong，取最小 RTT 样本估算时钟偏移。
func (c *Client) ClockSync(ctx context.Context, rounds int) error {
	var bestOffset int64
	bestRTT := int64(1 << 62)
	for i := range rounds {
		t0 := time.Now().UnixMilli()
		m, err := c.call(ctx, "ping", map[string]any{"client_time": t0})
		if err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		var pong struct {
			ServerTime int64 `json:"server_time"`
		}
		if err := json.Unmarshal(m.Data, &pong); err != nil {
			return err
		}
		rtt := now - t0
		if rtt < bestRTT {
			bestRTT = rtt
			bestOffset = pong.ServerTime + rtt/2 - now
		}
		if i < rounds-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	c.offsetMs = bestOffset
	c.synced = true
	return nil
}

// ServerNow 推算当前服务器时钟（毫秒）。未校时返回本地时间。
func (c *Client) ServerNow() int64 {
	return time.Now().UnixMilli() + c.offsetMs
}

// ---------- 会话 ----------

type Identity struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	Roles []string `json:"roles"`
}

// Auth 访客认证。password 为全局管理员口令（可选）。
func (c *Client) Auth(ctx context.Context, name, password string) (Identity, error) {
	m, err := c.call(ctx, "auth", map[string]any{"name": name, "password": password})
	if err != nil {
		return Identity{}, err
	}
	var out struct {
		Identity Identity `json:"identity"`
	}
	if err := json.Unmarshal(m.Data, &out); err != nil {
		return Identity{}, err
	}
	return out.Identity, nil
}

// AuthToken 用已有 session token（REST 登录所得，如 OIDC）完成 WS 认证。
func (c *Client) AuthToken(ctx context.Context, sessionToken string) (Identity, error) {
	m, err := c.call(ctx, "auth", map[string]any{"session_token": sessionToken})
	if err != nil {
		return Identity{}, err
	}
	var out struct {
		Identity Identity `json:"identity"`
	}
	if err := json.Unmarshal(m.Data, &out); err != nil {
		return Identity{}, err
	}
	return out.Identity, nil
}

// Join 加入房间。成功后快照经 Events() 推送。
func (c *Client) Join(ctx context.Context, roomID, password string) error {
	_, err := c.call(ctx, "room.join", map[string]any{"room_id": roomID, "password": password})
	return err
}

func (c *Client) Leave(ctx context.Context, roomID string) error {
	_, err := c.call(ctx, "room.leave", map[string]any{"room_id": roomID})
	return err
}

// ---------- 队列与播放操作 ----------

func (c *Client) QueueAdd(ctx context.Context, roomID, trackRef string) error {
	_, err := c.call(ctx, "queue.add", map[string]any{"room_id": roomID, "track_ref": trackRef})
	return err
}

// QueueAddMany 原子批量点歌：整体校验，任一失败一条不加；
// 全部通过按顺序队尾追加。返回按追加顺序的 entry_id 列表。
func (c *Client) QueueAddMany(ctx context.Context, roomID string, trackRefs []string) ([]string, error) {
	m, err := c.call(ctx, "queue.add", map[string]any{"room_id": roomID, "track_refs": trackRefs})
	if err != nil {
		return nil, err
	}
	var out struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := json.Unmarshal(m.Data, &out); err != nil {
		return nil, err
	}
	return out.EntryIDs, nil
}

func (c *Client) QueueRemove(ctx context.Context, roomID, entryID string) error {
	_, err := c.call(ctx, "queue.remove", map[string]any{"room_id": roomID, "entry_id": entryID})
	return err
}

func (c *Client) QueueMove(ctx context.Context, roomID, entryID string, toIndex int) error {
	_, err := c.call(ctx, "queue.move", map[string]any{"room_id": roomID, "entry_id": entryID, "to_index": toIndex})
	return err
}

// PlaybackOp 发送播放控制操作（pause/resume/skip/seek）。
func (c *Client) PlaybackOp(ctx context.Context, op, roomID string, positionMs int64) error {
	data := map[string]any{"room_id": roomID}
	if op == "playback.seek" {
		data["position_ms"] = positionMs
	}
	_, err := c.call(ctx, op, data)
	return err
}

// RadioPlay 让房间进入电台模式。
func (c *Client) RadioPlay(ctx context.Context, roomID, source string, shuffle, once bool) error {
	_, err := c.call(ctx, "radio.play", map[string]any{
		"room_id": roomID, "source": source, "shuffle": shuffle, "once": once,
	})
	return err
}

// RadioStop 退出电台模式。
func (c *Client) RadioStop(ctx context.Context, roomID string) error {
	_, err := c.call(ctx, "radio.stop", map[string]any{"room_id": roomID})
	return err
}

// ---------- 房间状态（客户端视图） ----------

type QueueEntry struct {
	EntryID       string `json:"entry_id"`
	TrackRef      string `json:"track_ref"`
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	DurationMs    int64  `json:"duration_ms"`
	Album         string `json:"album,omitempty"`
	CoverURL      string `json:"cover_url,omitempty"`
	SourceURL     string `json:"source_url,omitempty"`
	RequestedBy   string `json:"requested_by"`
	RequesterName string `json:"requester_name"`
	AddedAt       int64  `json:"added_at"`
	StreamURL     string `json:"stream_url,omitempty"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	BitrateKbps   int    `json:"bitrate_kbps,omitempty"`
}

type Playback struct {
	Current    *QueueEntry `json:"current"`
	PositionMs int64       `json:"position_ms"`
	UpdatedAt  int64       `json:"updated_at"`
	Playing    bool        `json:"playing"`
	Rate       float64     `json:"rate"`
}

// ShouldBeMs 由五元组推算 serverNow 时刻的播放位置。
func (p Playback) ShouldBeMs(serverNow int64) int64 {
	if p.Playing {
		return p.PositionMs + int64(float64(serverNow-p.UpdatedAt)*p.Rate)
	}
	return p.PositionMs
}

// ParsePlayback 解析 playback.changed 事件数据。
func ParsePlayback(m Message) (Playback, error) {
	var p Playback
	err := json.Unmarshal(m.Data, &p)
	return p, err
}

// ParseQueue 解析 queue.changed 事件数据。
func ParseQueue(m Message) ([]QueueEntry, error) {
	var d struct {
		Queue []QueueEntry `json:"queue"`
	}
	err := json.Unmarshal(m.Data, &d)
	return d.Queue, err
}

// RadioInfo 电台绑定状态（radio.changed 事件 data.radio；null 表示未绑定）。
type RadioInfo struct {
	Source      string `json:"source"`
	Description string `json:"description"`
	Finite      bool   `json:"finite"`
	Shuffle     bool   `json:"shuffle"`
	Once        bool   `json:"once"`
}

// ParseRadio 解析 radio.changed；未绑定时返回 nil。
func ParseRadio(m Message) *RadioInfo {
	var d struct {
		Radio *RadioInfo `json:"radio"`
	}
	if json.Unmarshal(m.Data, &d) != nil {
		return nil
	}
	return d.Radio
}

// Listener 听众条目（listeners.changed）。
type Listener struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Snapshot struct {
	Playback  Playback     `json:"playback"`
	Queue     []QueueEntry `json:"queue"`
	Radio     *RadioInfo   `json:"radio"`
	Listeners []Listener   `json:"listeners"`
}

// AwaitSnapshot 在 Join 之后调用，收齐服务端按序推送的快照消息。
func (c *Client) AwaitSnapshot(timeout time.Duration) (*Snapshot, error) {
	snap := &Snapshot{}
	var gotPlayback, gotQueue, gotRadio, gotListeners bool
	deadline := time.After(timeout)
	for !(gotPlayback && gotQueue && gotRadio && gotListeners) {
		select {
		case m, ok := <-c.events:
			if !ok {
				return nil, fmt.Errorf("connection closed while waiting for snapshot")
			}
			switch m.Type {
			case "playback.changed":
				if pb, err := ParsePlayback(m); err == nil {
					snap.Playback = pb
					gotPlayback = true
				}
			case "queue.changed":
				snap.Queue, _ = ParseQueue(m)
				gotQueue = true
			case "radio.changed":
				snap.Radio = ParseRadio(m)
				gotRadio = true
			case "listeners.changed":
				var d struct {
					Listeners []Listener `json:"listeners"`
				}
				if json.Unmarshal(m.Data, &d) == nil {
					snap.Listeners = d.Listeners
					gotListeners = true
				}
			}
		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for room snapshot")
		}
	}
	return snap, nil
}

// RoomState 是无状态控制 REST 返回的房间完整状态。
type RoomState = Snapshot

// RoomCapabilities 是当前身份在指定 Room 的有效 capability。
type RoomCapabilities struct {
	Controller bool `json:"controller"`
}

// RESTRoomCapabilities 查询当前标准 session 在 Room 中的有效 capability。
func RESTRoomCapabilities(ctx context.Context, server, actorToken, roomID string) (RoomCapabilities, error) {
	var out struct {
		Capabilities RoomCapabilities `json:"capabilities"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/capabilities", actorToken, nil, &out)
	return out.Capabilities, err
}

// RoomQueueAddRequest 是 POST /api/v1/rooms/{id}/queue 的请求。
// TrackRef 与 TrackRefs 必须二选一。
type RoomQueueAddRequest struct {
	TrackRef  string   `json:"track_ref,omitempty"`
	TrackRefs []string `json:"track_refs,omitempty"`
}

// RoomQueueAddResponse 是入队结果，EntryIDs 与请求顺序一致。
type RoomQueueAddResponse struct {
	EntryIDs []string `json:"entry_ids"`
}

// RoomQueueMoveRequest 是队列移动请求。
type RoomQueueMoveRequest struct {
	ToIndex int `json:"to_index"`
}

// RoomPlaybackSeekRequest 是播放定位请求。
type RoomPlaybackSeekRequest struct {
	PositionMs int64 `json:"position_ms"`
}

// RoomRadioPlayRequest 是电台播放请求。
type RoomRadioPlayRequest struct {
	Source  string `json:"source"`
	Shuffle bool   `json:"shuffle"`
	Once    bool   `json:"once"`
}

// RESTRoomState 无副作用地读取指定身份可见的房间状态。
func RESTRoomState(ctx context.Context, server, actorToken, roomID string) (RoomState, error) {
	var out RoomState
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/state", actorToken, nil, &out)
	return out, err
}

// RESTRoomQueueAdd 添加一首曲目并返回创建的队列 entry_id。
func RESTRoomQueueAdd(ctx context.Context, server, actorToken, roomID, trackRef string) (string, error) {
	out, err := restRoomQueueAdd(ctx, server, actorToken, roomID, RoomQueueAddRequest{TrackRef: trackRef})
	if err != nil {
		return "", err
	}
	if len(out.EntryIDs) != 1 {
		return "", fmt.Errorf("queue add returned %d entry IDs, want 1", len(out.EntryIDs))
	}
	return out.EntryIDs[0], nil
}

// RESTRoomQueueAddMany 原子批量添加曲目并返回对应的 entry_id。
func RESTRoomQueueAddMany(ctx context.Context, server, actorToken, roomID string, trackRefs []string) ([]string, error) {
	out, err := restRoomQueueAdd(ctx, server, actorToken, roomID, RoomQueueAddRequest{TrackRefs: trackRefs})
	return out.EntryIDs, err
}

func restRoomQueueAdd(ctx context.Context, server, actorToken, roomID string, body RoomQueueAddRequest) (RoomQueueAddResponse, error) {
	var out RoomQueueAddResponse
	err := restCall(ctx, server, http.MethodPost,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/queue", actorToken, body, &out)
	return out, err
}

// RESTRoomQueueRemove 移除指定队列条目。
func RESTRoomQueueRemove(ctx context.Context, server, actorToken, roomID, entryID string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/queue/"+url.PathEscape(entryID),
		actorToken, nil, &struct{}{})
}

// RESTRoomQueueMove 移动指定队列条目。
func RESTRoomQueueMove(ctx context.Context, server, actorToken, roomID, entryID string, toIndex int) error {
	return restCall(ctx, server, http.MethodPatch,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/queue/"+url.PathEscape(entryID),
		actorToken, RoomQueueMoveRequest{ToIndex: toIndex}, &struct{}{})
}

// RESTRoomPlaybackPause 暂停房间播放。
func RESTRoomPlaybackPause(ctx context.Context, server, actorToken, roomID string) error {
	return restRoomPlaybackOp(ctx, server, actorToken, roomID, "pause", nil)
}

// RESTRoomPlaybackResume 恢复房间播放。
func RESTRoomPlaybackResume(ctx context.Context, server, actorToken, roomID string) error {
	return restRoomPlaybackOp(ctx, server, actorToken, roomID, "resume", nil)
}

// RESTRoomPlaybackSkip 跳过当前曲目。
func RESTRoomPlaybackSkip(ctx context.Context, server, actorToken, roomID string) error {
	return restRoomPlaybackOp(ctx, server, actorToken, roomID, "skip", nil)
}

// RESTRoomPlaybackSeek 定位到指定毫秒位置。
func RESTRoomPlaybackSeek(ctx context.Context, server, actorToken, roomID string, positionMs int64) error {
	return restRoomPlaybackOp(ctx, server, actorToken, roomID, "seek",
		RoomPlaybackSeekRequest{PositionMs: positionMs})
}

func restRoomPlaybackOp(ctx context.Context, server, actorToken, roomID, op string, body any) error {
	return restCall(ctx, server, http.MethodPost,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/playback/"+url.PathEscape(op),
		actorToken, body, &struct{}{})
}

// RESTRoomRadioPlay 启动房间电台模式。
func RESTRoomRadioPlay(ctx context.Context, server, actorToken, roomID, source string, shuffle, once bool) error {
	return restCall(ctx, server, http.MethodPost,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/radio", actorToken,
		RoomRadioPlayRequest{Source: source, Shuffle: shuffle, Once: once}, &struct{}{})
}

// RESTRoomRadioStop 停止房间电台模式。
func RESTRoomRadioStop(ctx context.Context, server, actorToken, roomID string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/radio", actorToken, nil, &struct{}{})
}

// IntegrationInfo is the public persistent Integration metadata. It never
// contains the credential hash or plaintext token.
type IntegrationInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
}

type IntegrationCredentialResult struct {
	Integration IntegrationInfo `json:"integration"`
	Token       string          `json:"token"`
}

type UpdateIntegrationRequest struct {
	Name   *string `json:"name,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

// RESTListIntegrations 列出已配置的 Integration，不包含 token。
func RESTListIntegrations(ctx context.Context, server, actorToken string) ([]IntegrationInfo, error) {
	var out struct {
		Integrations []IntegrationInfo `json:"integrations"`
	}
	err := restCall(ctx, server, http.MethodGet, "/api/v1/integrations", actorToken, nil, &out)
	return out.Integrations, err
}

func RESTCreateIntegration(
	ctx context.Context,
	server, actorToken, id, name string,
) (IntegrationCredentialResult, error) {
	var out IntegrationCredentialResult
	err := restCall(ctx, server, http.MethodPost, "/api/v1/integrations", actorToken,
		map[string]string{"id": id, "name": name}, &out)
	return out, err
}

func RESTUpdateIntegration(
	ctx context.Context,
	server, actorToken, id string,
	update UpdateIntegrationRequest,
) (IntegrationInfo, error) {
	var out struct {
		Integration IntegrationInfo `json:"integration"`
	}
	err := restCall(ctx, server, http.MethodPatch,
		"/api/v1/integrations/"+url.PathEscape(id), actorToken, update, &out)
	return out.Integration, err
}

func RESTRotateIntegrationToken(
	ctx context.Context,
	server, actorToken, id string,
) (IntegrationCredentialResult, error) {
	var out IntegrationCredentialResult
	err := restCall(ctx, server, http.MethodPost,
		"/api/v1/integrations/"+url.PathEscape(id)+"/token",
		actorToken, nil, &out)
	return out, err
}

func RESTDeleteIntegration(ctx context.Context, server, actorToken, id string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/integrations/"+url.PathEscape(id), actorToken, nil, &struct{}{})
}

// IntegrationActorScope 标识 Integration 上报的外部作用域。
type IntegrationActorScope struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// IntegrationActorSubject 标识 Integration 上报的外部用户。
type IntegrationActorSubject struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// IntegrationActorResolveRequest 是可信 Integration 的 actor 解析请求。
type IntegrationActorResolveRequest struct {
	AdapterID string                  `json:"adapter_id"`
	Scope     IntegrationActorScope   `json:"scope"`
	Subject   IntegrationActorSubject `json:"subject"`
}

// IntegrationActorResolveResponse 返回可供标准 REST/WS 使用的短期 actor token。
type IntegrationActorResolveResponse struct {
	Identity      Identity `json:"identity"`
	DefaultRoomID string   `json:"default_room_id,omitempty"`
	ActorToken    string   `json:"actor_token"`
	ExpiresAt     int64    `json:"expires_at"`
}

// RESTResolveIntegrationActor 用 Integration token 解析外部身份。
func RESTResolveIntegrationActor(ctx context.Context, server, integrationToken string, request IntegrationActorResolveRequest) (IntegrationActorResolveResponse, error) {
	var out IntegrationActorResolveResponse
	err := restCall(ctx, server, http.MethodPost, "/api/v1/integrations/actors/resolve",
		integrationToken, request, &out)
	return out, err
}

// IntegrationScopeBinding 是 external scope 与默认 Room 的管理请求。
type IntegrationScopeBinding struct {
	AdapterID string `json:"adapter_id"`
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	RoomID    string `json:"room_id"`
}

// IntegrationScopeBindingInfo 是 Integration scope 绑定的只读表示。
type IntegrationScopeBindingInfo struct {
	IntegrationID string `json:"integration_id"`
	AdapterID     string `json:"adapter_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	RoomID        string `json:"room_id"`
}

// RESTListIntegrationScopes 列出 Integration 的全部 external scope 绑定。
func RESTListIntegrationScopes(ctx context.Context, server, actorToken, integrationID string) ([]IntegrationScopeBindingInfo, error) {
	var out struct {
		Scopes []IntegrationScopeBindingInfo `json:"scopes"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/scopes",
		actorToken, nil, &out)
	return out.Scopes, err
}

// RESTBindIntegrationScope 创建或更新 external scope 的默认 Room。
func RESTBindIntegrationScope(ctx context.Context, server, actorToken, integrationID string, binding IntegrationScopeBinding) error {
	return restCall(ctx, server, http.MethodPut,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/scopes",
		actorToken, binding, &struct{}{})
}

// RESTUnbindIntegrationScope 删除 external scope 的默认 Room 绑定。
func RESTUnbindIntegrationScope(ctx context.Context, server, actorToken, integrationID string, binding IntegrationScopeBinding) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/scopes",
		actorToken, binding, &struct{}{})
}

// IntegrationSubjectLink 是 external subject 与持久 Principal 的管理请求。
type IntegrationSubjectLink struct {
	AdapterID   string `json:"adapter_id"`
	ScopeType   string `json:"scope_type"`
	ScopeID     string `json:"scope_id"`
	SubjectID   string `json:"subject_id"`
	PrincipalID string `json:"principal_id"`
}

// IntegrationSubjectLinkInfo 是 Integration subject 链接的只读表示。
type IntegrationSubjectLinkInfo struct {
	IntegrationID string `json:"integration_id"`
	AdapterID     string `json:"adapter_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	SubjectID     string `json:"subject_id"`
	PrincipalID   string `json:"principal_id"`
}

// RESTListIntegrationSubjects 列出 Integration 的全部 external subject 链接。
func RESTListIntegrationSubjects(ctx context.Context, server, actorToken, integrationID string) ([]IntegrationSubjectLinkInfo, error) {
	var out struct {
		Subjects []IntegrationSubjectLinkInfo `json:"subjects"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/subjects",
		actorToken, nil, &out)
	return out.Subjects, err
}

// RESTLinkIntegrationSubject 创建或更新 external subject 的 Principal 关联。
func RESTLinkIntegrationSubject(ctx context.Context, server, actorToken, integrationID string, link IntegrationSubjectLink) error {
	return restCall(ctx, server, http.MethodPut,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/subjects",
		actorToken, link, &struct{}{})
}

// RESTUnlinkIntegrationSubject 删除 external subject 的 Principal 关联。
func RESTUnlinkIntegrationSubject(ctx context.Context, server, actorToken, integrationID string, link IntegrationSubjectLink) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/integrations/"+url.PathEscape(integrationID)+"/subjects",
		actorToken, link, &struct{}{})
}

// RoomControllerGrant 是 Room controller capability 的显式管理请求。
type RoomControllerGrant struct {
	RoomID      string `json:"room_id"`
	PrincipalID string `json:"principal_id"`
	Capability  string `json:"capability"`
}

// RESTListRoomGrants 列出 Room 的显式 controller grants。
func RESTListRoomGrants(ctx context.Context, server, actorToken, roomID string) ([]RoomControllerGrant, error) {
	var out struct {
		Grants []RoomControllerGrant `json:"grants"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/grants", actorToken, nil, &out)
	return out.Grants, err
}

// PrincipalInfo 是管理查询公开的 Principal，不包含 OIDC subject。
type PrincipalInfo struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Roles  []string `json:"roles"`
	Active bool     `json:"active"`
}

// RESTListPrincipals 按 ID 或名称搜索 Principal；limit <= 0 时使用服务端默认值。
func RESTListPrincipals(ctx context.Context, server, actorToken, query string, limit int) ([]PrincipalInfo, error) {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/principals"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out struct {
		Principals []PrincipalInfo `json:"principals"`
	}
	err := restCall(ctx, server, http.MethodGet, path, actorToken, nil, &out)
	return out.Principals, err
}

// RESTGrantRoomController 为 Principal 授予指定 Room 的 controller capability。
func RESTGrantRoomController(ctx context.Context, server, actorToken, roomID, principalID string) error {
	body := RoomControllerGrant{RoomID: roomID, PrincipalID: principalID, Capability: "controller"}
	return restCall(ctx, server, http.MethodPut,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/grants/"+url.PathEscape(principalID),
		actorToken, body, &struct{}{})
}

// RESTRevokeRoomController 撤销 Principal 在指定 Room 的 controller capability。
func RESTRevokeRoomController(ctx context.Context, server, actorToken, roomID, principalID string) error {
	body := RoomControllerGrant{RoomID: roomID, PrincipalID: principalID, Capability: "controller"}
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/grants/"+url.PathEscape(principalID),
		actorToken, body, &struct{}{})
}

var ErrNoSuchRoom = errors.New("room not found")

// ---------- REST 辅助（低频管理操作） ----------

// RESTAuth 走 REST 认证拿 session token（CLI 用）。
func RESTAuth(ctx context.Context, server, name, password string) (token string, err error) {
	var out struct {
		SessionToken string `json:"session_token"`
	}
	err = restCall(ctx, server, "POST", "/api/v1/auth/guest", "", map[string]any{
		"name": name, "password": password,
	}, &out)
	return out.SessionToken, err
}

// RESTOIDCAuth 用 IdP 签发的 id_token 换 yuzu session token。
// accessToken 可选：id_token 缺 preferred_username 时服务端用它调 userinfo 补齐。
func RESTOIDCAuth(ctx context.Context, server, idToken, accessToken string) (identity Identity, token string, err error) {
	var out struct {
		Identity     Identity `json:"identity"`
		SessionToken string   `json:"session_token"`
	}
	err = restCall(ctx, server, "POST", "/api/v1/auth/oidc", "", map[string]any{
		"id_token": idToken, "access_token": accessToken,
	}, &out)
	return out.Identity, out.SessionToken, err
}

type RoomNowPlaying struct {
	Title      string  `json:"title"`
	Artist     string  `json:"artist"`
	DurationMs int64   `json:"duration_ms"`
	CoverURL   string  `json:"cover_url"`
	PositionMs int64   `json:"position_ms"`
	UpdatedAt  int64   `json:"updated_at"`
	Playing    bool    `json:"playing"`
	Rate       float64 `json:"rate"`
}

type RoomInfo struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Policy        json.RawMessage `json:"policy"`
	ListenerCount int             `json:"listener_count"`
	NowPlaying    *RoomNowPlaying `json:"now_playing"`
}

func RESTListRooms(ctx context.Context, server, token string) ([]RoomInfo, error) {
	var out struct {
		Rooms []RoomInfo `json:"rooms"`
	}
	err := restCall(ctx, server, "GET", "/api/v1/rooms", token, nil, &out)
	return out.Rooms, err
}

type ProviderInfo struct {
	ID               string `json:"id"`
	CredentialStatus string `json:"credential_status,omitempty"`
}

func RESTListProviders(ctx context.Context, server, token string) ([]ProviderInfo, error) {
	var out struct {
		Providers []ProviderInfo `json:"providers"`
	}
	err := restCall(ctx, server, "GET", "/api/v1/providers", token, nil, &out)
	return out.Providers, err
}

// SplitRef 拆解 track_ref / 源规格为 provider 与 id 部分。
func SplitRef(spec string) (providerID, id string, err error) {
	p, rest, ok := strings.Cut(spec, ":")
	if !ok || p == "" || rest == "" {
		return "", "", fmt.Errorf("invalid ref %q, want \"provider:id\"", spec)
	}
	return p, rest, nil
}

type Track struct {
	Ref        string `json:"track_ref"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
}

// ---------- 歌单 ----------

type Playlist struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	TrackCount  int    `json:"track_count"`
}

type PlaylistItem struct {
	Ord        int    `json:"ord"`
	TrackRef   string `json:"track_ref"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
	AddedAt    int64  `json:"added_at"`
}

func RESTListPlaylists(ctx context.Context, server, token string) ([]Playlist, error) {
	var out struct {
		Playlists []Playlist `json:"playlists"`
	}
	err := restCall(ctx, server, "GET", "/api/v1/playlists", token, nil, &out)
	return out.Playlists, err
}

func RESTGetPlaylist(ctx context.Context, server, token, id string, offset, limit int) (Playlist, []PlaylistItem, error) {
	var out struct {
		Playlist Playlist       `json:"playlist"`
		Items    []PlaylistItem `json:"items"`
	}
	path := fmt.Sprintf("/api/v1/playlists/%s?offset=%d&limit=%d", id, offset, limit)
	err := restCall(ctx, server, "GET", path, token, nil, &out)
	return out.Playlist, out.Items, err
}

func RESTCreatePlaylist(ctx context.Context, server, token, name, description string) (Playlist, error) {
	var out struct {
		Playlist Playlist `json:"playlist"`
	}
	err := restCall(ctx, server, "POST", "/api/v1/playlists", token,
		map[string]any{"name": name, "description": description}, &out)
	return out.Playlist, err
}

func RESTDeletePlaylist(ctx context.Context, server, token, id string) error {
	return restCall(ctx, server, "DELETE", "/api/v1/playlists/"+id, token, nil, &struct{}{})
}

func RESTAddPlaylistItems(ctx context.Context, server, token, id string, refs []string) error {
	return restCall(ctx, server, "POST", "/api/v1/playlists/"+id+"/items", token,
		map[string]any{"track_refs": refs}, &struct{}{})
}

func RESTDeletePlaylistItem(ctx context.Context, server, token, id string, ord int) error {
	return restCall(ctx, server, "DELETE",
		fmt.Sprintf("/api/v1/playlists/%s/items/%d", id, ord), token, nil, &struct{}{})
}

func RESTMovePlaylistItem(ctx context.Context, server, token, id string, ord, toOrd int) error {
	return restCall(ctx, server, "PATCH",
		fmt.Sprintf("/api/v1/playlists/%s/items/%d", id, ord), token,
		map[string]any{"to_ord": toOrd}, &struct{}{})
}

// RESTImportPlaylist 导入外部歌单或曲目源快照。provider+playlistID 或 source 二选一。
func RESTImportPlaylist(ctx context.Context, server, token, prov, playlistID, source, name string) (Playlist, error) {
	var out struct {
		Playlist Playlist `json:"playlist"`
	}
	body := map[string]any{}
	if source != "" {
		body["source"] = source
	} else {
		body["provider"] = prov
		body["playlist_id"] = playlistID
	}
	if name != "" {
		body["name"] = name
	}
	err := restCall(ctx, server, "POST", "/api/v1/playlists/import", token, body, &out)
	return out.Playlist, err
}

func RESTSearch(ctx context.Context, server, token, provider, query string) ([]Track, error) {
	var out struct {
		Tracks []Track `json:"tracks"`
	}
	path := fmt.Sprintf("/api/v1/search?provider=%s&q=%s", provider, url.QueryEscape(query))
	err := restCall(ctx, server, "GET", path, token, nil, &out)
	return out.Tracks, err
}

// RESTCreateRoom 创建房间（room_admin）。guestPassword 为空表示无密码房间。
func RESTCreateRoom(ctx context.Context, server, token, id, roomName, guestPassword string) error {
	return restCall(ctx, server, "POST", "/api/v1/rooms", token, map[string]any{
		"id": id, "name": roomName, "guest_password": guestPassword,
	}, &struct{}{})
}

// RESTUpdateRoomPolicy 热更新房间治理策略（room_admin）。policy 为 JSON 文本。
func RESTUpdateRoomPolicy(ctx context.Context, server, token, roomID, policy string) error {
	return restCall(ctx, server, "PATCH", "/api/v1/rooms/"+roomID, token, map[string]any{
		"policy": policy,
	}, &struct{}{})
}

// RESTDeleteRoom 删除房间（room_admin）。队列与历史级联清理。
func RESTDeleteRoom(ctx context.Context, server, token, roomID string) error {
	return restCall(ctx, server, "DELETE", "/api/v1/rooms/"+roomID, token, nil, &struct{}{})
}

// RESTLogout 服务端吊销会话。幂等。
func RESTLogout(ctx context.Context, server, token string) error {
	return restCall(ctx, server, "DELETE", "/api/v1/auth/session", token, nil, &struct{}{})
}

// HistoryEntry 播放历史条目。
type HistoryEntry struct {
	TrackRef    string `json:"track_ref"`
	Title       string `json:"title"`
	RequestedBy string `json:"requested_by"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     int64  `json:"ended_at"`
	EndReason   string `json:"end_reason"`
}

// RESTRoomHistory 房间播放历史（最新在前）。
func RESTRoomHistory(ctx context.Context, server, token, roomID string, offset, limit int) ([]HistoryEntry, error) {
	var out struct {
		History []HistoryEntry `json:"history"`
	}
	path := fmt.Sprintf("/api/v1/rooms/%s/history?offset=%d&limit=%d", roomID, offset, limit)
	err := restCall(ctx, server, "GET", path, token, nil, &out)
	return out.History, err
}

// TrackStat 曲目热度统计。
type TrackStat struct {
	TrackRef      string `json:"track_ref"`
	Title         string `json:"title"`
	PlayCount     int    `json:"play_count"`
	FirstPlayedAt int64  `json:"first_played_at"`
	LastPlayedAt  int64  `json:"last_played_at"`
}

// RESTRoomStats 房间曲目热度榜。
func RESTRoomStats(ctx context.Context, server, token, roomID string, limit int) ([]TrackStat, error) {
	var out struct {
		Stats []TrackStat `json:"stats"`
	}
	path := fmt.Sprintf("/api/v1/rooms/%s/stats?limit=%d", roomID, limit)
	err := restCall(ctx, server, "GET", path, token, nil, &out)
	return out.Stats, err
}

// RESTUpload 上传本地媒体文件（media_admin）。
func RESTUpload(ctx context.Context, server, token, filePath, title, artist string, durationMs int64) (Track, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Track{}, err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return Track{}, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return Track{}, err
	}
	if title != "" {
		mw.WriteField("title", title)
	}
	if artist != "" {
		mw.WriteField("artist", artist)
	}
	if durationMs > 0 {
		mw.WriteField("duration_ms", strconv.FormatInt(durationMs, 10))
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", server+"/api/v1/media/upload", &buf)
	if err != nil {
		return Track{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Track{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error ErrorMsg `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return Track{}, fmt.Errorf("HTTP %d: %s %s", resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	var out struct {
		Track Track `json:"track"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Track{}, err
	}
	return out.Track, nil
}

// RESTCacheView 返回缓存全貌（media_admin）。
type CacheView struct {
	Entries []struct {
		TrackRef  string `json:"track_ref"`
		FilePath  string `json:"file_path"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"entries"`
	Downloads []DownloadStatus `json:"downloads"`
	History   []DownloadStatus `json:"history"`
}

type DownloadStatus struct {
	TrackRef   string `json:"track_ref"`
	Fetched    int64  `json:"fetched_bytes"`
	Total      int64  `json:"total_bytes"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

func RESTCacheView(ctx context.Context, server, token string) (CacheView, error) {
	var out CacheView
	err := restCall(ctx, server, "GET", "/api/v1/media/cache", token, nil, &out)
	return out, err
}

// RESTSetCredential 热更新 provider 凭据。
func RESTSetCredential(ctx context.Context, server, token, providerID, payload string) error {
	return restCall(ctx, server, "POST", "/api/v1/providers/"+providerID+"/credential",
		token, map[string]any{"payload": payload}, &struct{}{})
}

// QRLoginSession 是二维码登录会话。
type QRLoginSession struct {
	Key       string `json:"key"`
	QRContent string `json:"qr_content"`
}

func RESTQRLoginStart(ctx context.Context, server, token, providerID string) (QRLoginSession, error) {
	var out QRLoginSession
	err := restCall(ctx, server, "POST", "/api/v1/providers/"+providerID+"/qrlogin",
		token, map[string]any{}, &out)
	return out, err
}

type QRLoginResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func RESTQRLoginPoll(ctx context.Context, server, token, providerID, key string) (QRLoginResult, error) {
	var out QRLoginResult
	err := restCall(ctx, server, "GET", "/api/v1/providers/"+providerID+"/qrlogin/"+key,
		token, nil, &out)
	return out, err
}

type idempotencyContextKey struct{}

// WithIdempotencyKey attaches a platform event/request ID to subsequent REST
// writes. Integration actor Room mutations require this value.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyContextKey{}, key)
}

func restCall(ctx context.Context, server, method, path, token string, body, out any) error {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, server+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key, _ := ctx.Value(idempotencyContextKey{}).(string); key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error ErrorMsg `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("HTTP %d: %s %s", resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

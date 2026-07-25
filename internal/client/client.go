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

// ---------- 房间状态（客户端视图） ----------

type QueueEntry struct {
	EntryID     string `json:"entry_id"`
	TrackRef    string `json:"track_ref"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	DurationMs  int64  `json:"duration_ms"`
	RequestedBy string `json:"requested_by"`
	AddedAt     int64  `json:"added_at"`
	StreamURL   string `json:"stream_url,omitempty"`
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

type RoomInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

type Track struct {
	Ref        string `json:"track_ref"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
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

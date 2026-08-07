package app_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/youwenqwq/yuzu-jukebox/internal/app"
	"github.com/youwenqwq/yuzu-jukebox/internal/client"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

// ---------- 测试辅助 ----------

type env struct {
	t       *testing.T
	srv     *httptest.Server
	a       *app.App
	cancels []context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Addr:          "127.0.0.1:0",
		DBPath:        filepath.Join(dir, "test.db"),
		MediaDir:      filepath.Join(dir, "media"),
		CacheDir:      filepath.Join(dir, "cache"),
		CacheMaxBytes: 1 << 30,
		AdminPassword: "admin123",
		SecretKey:     "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		Media:         config.MediaConfig{MaxUploadBytes: 1 << 30},
	}
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { a.Store.Close() })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, a: a}
}

func (e *env) post(token, path string, body any) *http.Response {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (e *env) get(token, path string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL, err)
	}
}

// guestAuth 走 REST 认证，返回 (identityID, sessionToken)。
func (e *env) guestAuth(name, password string) (string, string) {
	e.t.Helper()
	resp := e.post("", "/api/v1/auth/guest", map[string]any{"name": name, "password": password})
	if resp.StatusCode != 200 {
		e.t.Fatalf("guest auth status %d", resp.StatusCode)
	}
	var out struct {
		Identity struct {
			ID    string   `json:"id"`
			Roles []string `json:"roles"`
		} `json:"identity"`
		SessionToken string `json:"session_token"`
	}
	decode(e.t, resp, &out)
	return out.Identity.ID, out.SessionToken
}

// makeWAV 生成 PCM WAV：8kHz 16-bit 单声道，secs 秒静音。
func makeWAV(secs int) []byte {
	const sampleRate = 8000
	n := sampleRate * secs
	dataSize := n * 2
	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16)) // fmt chunk size
	binary.Write(buf, binary.LittleEndian, uint16(1))  // PCM
	binary.Write(buf, binary.LittleEndian, uint16(1))  // mono
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(buf, binary.LittleEndian, uint16(16))           // bits
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

// wsClient 是一个最小 WS 测试客户端；它按 revision 原子维护队列副本。
type wsClient struct {
	t     *testing.T
	conn  *websocket.Conn
	seen  []wsMsg // 所有读到的消息（含被跳过的广播）
	queue wsQueueReplica
}

type wsQueueReplica struct {
	revision uint64
	items    []client.QueueEntry
	ready    bool
	snapshot *wsQueueSnapshotAssembly
	patch    *wsQueuePatchAssembly
}

type wsQueueSnapshotAssembly struct {
	revision uint64
	nextPart int
	items    []client.QueueEntry
}

type wsQueuePatchAssembly struct {
	baseRevision uint64
	revision     uint64
	nextPart     int
	ops          []client.QueuePatchOp
}

func (e *env) dialWS() *wsClient {
	e.t.Helper()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/v1"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		e.t.Fatalf("ws dial: %v", err)
	}
	e.t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return &wsClient{t: e.t, conn: conn}
}

type wsMsg struct {
	Type string          `json:"type"`
	Ref  string          `json:"ref"`
	Data json.RawMessage `json:"data"`
}

func (c *wsClient) send(typ, ref string, data any) {
	c.t.Helper()
	d, _ := json.Marshal(data)
	msg, _ := json.Marshal(map[string]any{"type": typ, "ref": ref, "data": json.RawMessage(d)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		c.t.Fatalf("ws write %s: %v", typ, err)
	}
}

func (c *wsClient) read(deadline time.Time) wsMsg {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("ws read: %v", err)
	}
	var m wsMsg
	if err := json.Unmarshal(data, &m); err != nil {
		c.t.Fatalf("ws unmarshal: %v", err)
	}
	c.seen = append(c.seen, m)
	if err := c.queue.accept(m); err != nil {
		c.t.Fatalf("invalid %s stream: %v", m.Type, err)
	}
	return m
}

// waitFor 读消息直到匹配类型（记录所有读到的消息），5 秒超时。
func (c *wsClient) waitFor(typ string) wsMsg {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := c.read(deadline); m.Type == typ {
			return m
		}
	}
	c.t.Fatalf("timeout waiting for %s", typ)
	return wsMsg{}
}

// waitForQueueState 等到一个完整 snapshot/patch 原子落地后再检查最终队列。
func (c *wsClient) waitForQueueState(description string, matches func([]client.QueueEntry) bool) []client.QueueEntry {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.queue.ready {
			items := append([]client.QueueEntry(nil), c.queue.items...)
			if matches(items) {
				return items
			}
		}
		c.read(deadline)
	}
	c.t.Fatalf("timeout waiting for queue state: %s", description)
	return nil
}

func (q *wsQueueReplica) accept(m wsMsg) error {
	switch m.Type {
	case "queue.snapshot":
		var part client.QueueSnapshotPart
		if err := json.Unmarshal(m.Data, &part); err != nil {
			return err
		}
		if part.Part < 0 {
			return fmt.Errorf("negative snapshot part %d", part.Part)
		}
		if part.Part == 0 {
			q.snapshot = &wsQueueSnapshotAssembly{revision: part.Revision}
			q.patch = nil
		}
		assembly := q.snapshot
		if assembly == nil || assembly.revision != part.Revision || assembly.nextPart != part.Part {
			return fmt.Errorf("snapshot sequence revision=%d part=%d", part.Revision, part.Part)
		}
		assembly.items = append(assembly.items, part.Items...)
		assembly.nextPart++
		if part.Done {
			q.items = append([]client.QueueEntry(nil), assembly.items...)
			q.revision = assembly.revision
			q.ready = true
			q.snapshot = nil
		}
	case "queue.patch":
		var part client.QueuePatchPart
		if err := json.Unmarshal(m.Data, &part); err != nil {
			return err
		}
		if part.Part < 0 || part.Revision != part.BaseRevision+1 {
			return fmt.Errorf("invalid patch revision=%d base=%d part=%d", part.Revision, part.BaseRevision, part.Part)
		}
		if part.Part == 0 {
			q.patch = &wsQueuePatchAssembly{
				baseRevision: part.BaseRevision,
				revision:     part.Revision,
			}
		}
		assembly := q.patch
		if assembly == nil || assembly.baseRevision != part.BaseRevision ||
			assembly.revision != part.Revision || assembly.nextPart != part.Part {
			return fmt.Errorf("patch sequence revision=%d part=%d", part.Revision, part.Part)
		}
		assembly.ops = append(assembly.ops, part.Ops...)
		assembly.nextPart++
		if !part.Done {
			return nil
		}
		if !q.ready || q.revision != assembly.baseRevision {
			return fmt.Errorf("patch base revision=%d, local revision=%d", assembly.baseRevision, q.revision)
		}
		next, err := applyWSQueuePatch(q.items, assembly.ops)
		if err != nil {
			return err
		}
		q.items = next
		q.revision = assembly.revision
		q.patch = nil
	}
	return nil
}

func applyWSQueuePatch(current []client.QueueEntry, ops []client.QueuePatchOp) ([]client.QueueEntry, error) {
	next := append([]client.QueueEntry(nil), current...)
	indexOf := func(entryID string) int {
		for i := range next {
			if next[i].EntryID == entryID {
				return i
			}
		}
		return -1
	}
	for _, op := range ops {
		switch op.Op {
		case "add":
			if op.Item == nil || op.Index < 0 || op.Index > len(next) || indexOf(op.Item.EntryID) >= 0 {
				return nil, fmt.Errorf("invalid add operation")
			}
			next = append(next, client.QueueEntry{})
			copy(next[op.Index+1:], next[op.Index:])
			next[op.Index] = *op.Item
		case "remove":
			index := indexOf(op.EntryID)
			if index < 0 {
				return nil, fmt.Errorf("remove missing entry %q", op.EntryID)
			}
			copy(next[index:], next[index+1:])
			next = next[:len(next)-1]
		case "move":
			index := indexOf(op.EntryID)
			if index < 0 || op.ToIndex < 0 || op.ToIndex >= len(next) {
				return nil, fmt.Errorf("invalid move operation")
			}
			item := next[index]
			if index < op.ToIndex {
				copy(next[index:op.ToIndex], next[index+1:op.ToIndex+1])
			} else if index > op.ToIndex {
				copy(next[op.ToIndex+1:index+1], next[op.ToIndex:index])
			}
			next[op.ToIndex] = item
		case "clear":
			next = []client.QueueEntry{}
		default:
			return nil, fmt.Errorf("unknown queue operation %q", op.Op)
		}
	}
	return next, nil
}

// lastSeen 返回最近一条该类型消息（广播可能先于 ack 到达被记录）。
func (c *wsClient) lastSeen(typ string) (wsMsg, bool) {
	for i := len(c.seen) - 1; i >= 0; i-- {
		if c.seen[i].Type == typ {
			return c.seen[i], true
		}
	}
	return wsMsg{}, false
}

// ---------- 冒烟测试 ----------

func TestSmokeEndToEnd(t *testing.T) {
	e := newEnv(t)

	// 1. 管理员认证（全局口令 → 管理角色）
	_, adminToken := e.guestAuth("admin", "admin123")

	// 2. 创建房间（带访客密码）
	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
		"id": "lobby", "name": "大厅", "guest_password": "room123",
	})
	if resp.StatusCode != 201 {
		e.t.Fatalf("create room status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. 上传媒体（10 秒 WAV，避免自然结束与 skip 竞争）
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "test.wav")
	fw.Write(makeWAV(10))
	mw.WriteField("title", "Test Song")
	mw.WriteField("artist", "Yuzu")
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/media/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+adminToken)
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("upload: %v", err)
	}
	if upResp.StatusCode != 201 {
		b, _ := io.ReadAll(upResp.Body)
		e.t.Fatalf("upload status %d: %s", upResp.StatusCode, b)
	}
	var upOut struct {
		Track struct {
			Ref        string `json:"track_ref"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"track"`
	}
	decode(t, upResp, &upOut)
	if upOut.Track.Ref == "" || upOut.Track.DurationMs < 9000 || upOut.Track.DurationMs > 11000 {
		e.t.Fatalf("bad uploaded track: %+v", upOut.Track)
	}

	// 4. 搜索能命中
	resp = e.get(adminToken, "/api/v1/search?provider=local&q=Test")
	var searchOut struct {
		Tracks []struct {
			Ref string `json:"track_ref"`
		} `json:"tracks"`
	}
	decode(t, resp, &searchOut)
	if len(searchOut.Tracks) == 0 {
		e.t.Fatal("search returned no tracks")
	}

	// 5. WS：访客校时 + 认证
	c := e.dialWS()
	c.send("ping", "p1", map[string]any{"client_time": time.Now().UnixMilli()})
	pong := c.waitFor("pong")
	var pongData struct {
		ClientTime int64 `json:"client_time"`
		ServerTime int64 `json:"server_time"`
	}
	json.Unmarshal(pong.Data, &pongData)
	if pongData.ServerTime == 0 || pong.Ref != "p1" {
		e.t.Fatalf("bad pong: %+v", pongData)
	}

	c.send("auth", "a1", map[string]any{"name": "listener1"})
	authOK := c.waitFor("auth.ok")
	var authData struct {
		Identity struct {
			Kind  string   `json:"kind"`
			Roles []string `json:"roles"`
		} `json:"identity"`
	}
	json.Unmarshal(authOK.Data, &authData)
	if authData.Identity.Kind != "guest" {
		e.t.Fatalf("bad auth.ok: %+v", authData.Identity)
	}

	// 6. 错误房间密码被拒
	c.send("room.join", "j1", map[string]any{"room_id": "lobby", "password": "wrong"})
	errMsg := c.waitFor("error")
	var errData struct {
		Code string `json:"code"`
	}
	json.Unmarshal(errMsg.Data, &errData)
	if errData.Code != "forbidden" {
		e.t.Fatalf("want forbidden, got %s", errData.Code)
	}

	// 7. 正确密码进房 → 快照（空闲 playback + 空队列 + 听众列表）
	c.send("room.join", "j2", map[string]any{"room_id": "lobby", "password": "room123"})
	c.waitFor("room.joined")
	snap := c.waitFor("playback.changed")
	var pb struct {
		Current *struct {
			TrackRef  string `json:"track_ref"`
			StreamURL string `json:"stream_url"`
		} `json:"current"`
		Playing bool    `json:"playing"`
		Rate    float64 `json:"rate"`
	}
	json.Unmarshal(snap.Data, &pb)
	if pb.Current != nil || pb.Playing {
		e.t.Fatalf("room should be idle, got %+v", pb)
	}
	c.waitForQueueState("empty join snapshot", func(items []client.QueueEntry) bool { return len(items) == 0 })
	c.waitFor("listeners.changed")

	// 8. 点歌 → 自动开播，playback.changed 带 stream_url
	c.send("queue.add", "q1", map[string]any{"room_id": "lobby", "track_ref": upOut.Track.Ref})
	c.waitFor("ack")
	play, ok := c.lastSeen("playback.changed")
	if !ok {
		play = c.waitFor("playback.changed")
	}
	json.Unmarshal(play.Data, &pb)
	if pb.Current == nil || pb.Current.TrackRef != upOut.Track.Ref {
		e.t.Fatalf("want playing %s, got %+v", upOut.Track.Ref, pb)
	}
	if !pb.Playing || pb.Rate != 1.0 {
		e.t.Fatalf("bad playback state: %+v", pb)
	}
	if pb.Current.StreamURL == "" {
		e.t.Fatal("stream_url missing")
	}

	// 9. 拉流：票据鉴权 + 内容正确（WAV 以 RIFF 开头）
	sResp, err := http.Get(e.srv.URL + pb.Current.StreamURL)
	if err != nil {
		e.t.Fatalf("stream get: %v", err)
	}
	body, _ := io.ReadAll(sResp.Body)
	sResp.Body.Close()
	if sResp.StatusCode != 200 {
		e.t.Fatalf("stream status %d", sResp.StatusCode)
	}
	if len(body) < 44 || string(body[:4]) != "RIFF" {
		e.t.Fatalf("stream body not a WAV (len=%d)", len(body))
	}

	// 无票据必须被拒
	noTicket, _ := http.Get(e.srv.URL + "/stream/v1/" + upOut.Track.Ref)
	if noTicket.StatusCode != 401 {
		e.t.Fatalf("stream without ticket: want 401, got %d", noTicket.StatusCode)
	}
	noTicket.Body.Close()

	// 10. 普通访客无 playback.skip 权限
	c.send("playback.skip", "s1", map[string]any{"room_id": "lobby"})
	errMsg = c.waitFor("error")
	json.Unmarshal(errMsg.Data, &errData)
	if errData.Code != "forbidden" {
		e.t.Fatalf("guest skip: want forbidden, got %s", errData.Code)
	}

	// 11. 管理员 WS 进房切歌 → 空闲 + 播放历史落库
	admin := e.dialWS()
	admin.send("auth", "a1", map[string]any{"name": "dj", "password": "admin123"})
	admin.waitFor("auth.ok")
	admin.send("room.join", "j1", map[string]any{"room_id": "lobby", "password": "room123"})
	admin.waitFor("room.joined")
	admin.waitFor("playback.changed")
	admin.send("playback.skip", "s1", map[string]any{"room_id": "lobby"})
	admin.waitFor("ack")
	idle, ok := admin.lastSeen("playback.changed")
	if !ok {
		idle = admin.waitFor("playback.changed")
	}
	json.Unmarshal(idle.Data, &pb)
	if pb.Current != nil || pb.Playing {
		e.t.Fatalf("after skip room should be idle, got %+v", pb)
	}

	// 12. 播放历史
	var histCount int
	row := e.a.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM play_history WHERE room_id = 'lobby' AND track_ref = ?`, upOut.Track.Ref)
	if err := row.Scan(&histCount); err != nil || histCount != 1 {
		e.t.Fatalf("play_history count=%d err=%v, want 1", histCount, err)
	}
	var reason string
	row = e.a.Store.DB().QueryRow(
		`SELECT end_reason FROM play_history WHERE room_id = 'lobby' AND track_ref = ?`, upOut.Track.Ref)
	if err := row.Scan(&reason); err != nil || reason != "skipped" {
		e.t.Fatalf("end_reason=%q err=%v, want skipped", reason, err)
	}
	fmt.Println("smoke test OK")
}

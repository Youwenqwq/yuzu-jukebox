package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

// uploadTrack 上传 30 秒 WAV（避免测试期间自然播完），返回 track_ref。
func uploadTrack(t *testing.T, e *env, token, name string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name+".wav")
	fw.Write(makeWAV(30))
	mw.WriteField("title", name)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/media/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload %s status %d: %s", name, resp.StatusCode, b)
	}
	var out struct {
		Track struct {
			Ref string `json:"track_ref"`
		} `json:"track"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Track.Ref == "" {
		t.Fatalf("upload %s: bad response (err=%v)", name, err)
	}
	return out.Track.Ref
}

type ackData struct {
	EntryIDs []string `json:"entry_ids"`
}

func trackRefsOf(q []client.QueueEntry) []string {
	refs := []string{}
	for _, e := range q {
		refs = append(refs, e.TrackRef)
	}
	return refs
}

// TestQueueAddBatchWS 批量入队协议：双形态解析、ack 携带 entry_ids（按追加顺序）、
// 冲突/缺参 bad_request、批内含坏 ref 时原子零追加。
func TestQueueAddBatchWS(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")

	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{"id": "bq", "name": "批量"})
	if resp.StatusCode != 201 {
		t.Fatalf("create room status %d", resp.StatusCode)
	}
	resp.Body.Close()

	refs := make([]string, 0, 6)
	for _, name := range []string{"t1", "t2", "t3", "t4", "t5", "t6"} {
		refs = append(refs, uploadTrack(t, e, adminToken, name))
	}
	t1, t2, t3, t4, t5, t6 := refs[0], refs[1], refs[2], refs[3], refs[4], refs[5]

	c := e.dialWS()
	c.send("auth", "a1", map[string]any{"name": "dj1"})
	c.waitFor("auth.ok")
	c.send("room.join", "j1", map[string]any{"room_id": "bq"})
	c.waitFor("room.joined")
	c.waitForQueueState("empty join snapshot", func(items []client.QueueEntry) bool { return len(items) == 0 })

	// 单条形态：ack 带 entry_ids（兼容字段），空闲自动开播
	c.send("queue.add", "s1", map[string]any{"room_id": "bq", "track_ref": t1})
	ack := c.waitFor("ack")
	if ack.Ref != "s1" {
		t.Fatalf("ack ref = %q, want s1", ack.Ref)
	}
	var ad ackData
	if err := json.Unmarshal(ack.Data, &ad); err != nil || len(ad.EntryIDs) != 1 || ad.EntryIDs[0] == "" {
		t.Fatalf("single add ack entry_ids = %+v (err=%v), want 1 id", ad, err)
	}

	// 批量形态：entry_ids 按追加顺序，与队列条目一一对应
	c.send("queue.add", "b1", map[string]any{"room_id": "bq", "track_refs": []string{t2, t3}})
	ack = c.waitFor("ack")
	ad.EntryIDs = nil
	if err := json.Unmarshal(ack.Data, &ad); err != nil || len(ad.EntryIDs) != 2 {
		t.Fatalf("batch ack entry_ids = %+v (err=%v), want 2 ids", ad, err)
	}
	q := c.waitForQueueState("batch t2,t3", func(items []client.QueueEntry) bool {
		return equalStrings(trackRefsOf(items), []string{t2, t3})
	})
	if got, want := trackRefsOf(q), []string{t2, t3}; !equalStrings(got, want) {
		t.Fatalf("queue refs = %v, want %v", got, want)
	}
	if q[0].EntryID != ad.EntryIDs[0] || q[1].EntryID != ad.EntryIDs[1] {
		t.Fatalf("entry_ids order %v does not match queue order [%s %s]", ad.EntryIDs, q[0].EntryID, q[1].EntryID)
	}

	// 双形态冲突 → bad_request
	c.send("queue.add", "x1", map[string]any{"room_id": "bq", "track_ref": t1, "track_refs": []string{t2}})
	errMsg := c.waitFor("error")
	assertErrCode(t, errMsg, "x1", "bad_request")

	// 两者都不给 → bad_request
	c.send("queue.add", "x2", map[string]any{"room_id": "bq"})
	errMsg = c.waitFor("error")
	assertErrCode(t, errMsg, "x2", "bad_request")

	// 超过 100 条 → bad_request
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = t1
	}
	c.send("queue.add", "x3", map[string]any{"room_id": "bq", "track_refs": tooMany})
	errMsg = c.waitFor("error")
	assertErrCode(t, errMsg, "x3", "bad_request")

	// 批内含坏 ref → bad_request 且零追加（随后单加 t4，队列应恰好 [t2,t3,t4]）
	c.send("queue.add", "x4", map[string]any{"room_id": "bq", "track_refs": []string{t4, "bogus"}})
	errMsg = c.waitFor("error")
	assertErrCode(t, errMsg, "x4", "bad_request")

	c.send("queue.add", "s2", map[string]any{"room_id": "bq", "track_ref": t4})
	c.waitFor("ack")
	if got, want := trackRefsOf(c.waitForQueueState("t4 appended after rejected batch", func(items []client.QueueEntry) bool {
		return equalStrings(trackRefsOf(items), []string{t2, t3, t4})
	})), []string{t2, t3, t4}; !equalStrings(got, want) {
		t.Fatalf("queue refs = %v, want %v（失败批次应零追加）", got, want)
	}

	// 协议客户端库：QueueAddMany 解析 entry_ids；QueueAdd 单条兼容
	ctx := context.Background()
	cli, err := client.Dial(ctx, e.srv.URL)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Auth(ctx, "dj2", ""); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	if err := cli.Join(ctx, "bq", ""); err != nil {
		t.Fatalf("client join: %v", err)
	}
	if _, err := cli.AwaitSnapshot(5 * time.Second); err != nil {
		t.Fatalf("client snapshot: %v", err)
	}
	ids, err := cli.QueueAddMany(ctx, "bq", []string{t5, t6})
	if err != nil {
		t.Fatalf("QueueAddMany: %v", err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("QueueAddMany ids = %v, want 2 distinct ids", ids)
	}
	q = c.waitForQueueState("client batch t5,t6 applied", func(items []client.QueueEntry) bool {
		return equalStrings(trackRefsOf(items), []string{t2, t3, t4, t5, t6})
	})
	if got, want := trackRefsOf(q), []string{t2, t3, t4, t5, t6}; !equalStrings(got, want) {
		t.Fatalf("queue refs = %v, want %v", got, want)
	}
	if q[3].EntryID != ids[0] || q[4].EntryID != ids[1] {
		t.Fatalf("client ids %v do not match queue tail [%s %s]", ids, q[3].EntryID, q[4].EntryID)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertErrCode(t *testing.T, m wsMsg, ref, code string) {
	t.Helper()
	if m.Ref != ref {
		t.Fatalf("error ref = %q, want %q", m.Ref, ref)
	}
	var d struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatalf("error unmarshal: %v", err)
	}
	if d.Code != code {
		t.Fatalf("error code = %q, want %q (msg: %s)", d.Code, code, m.Data)
	}
}

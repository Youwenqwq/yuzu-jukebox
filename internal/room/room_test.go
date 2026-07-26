package room

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// newTestRoom 启动一个带真实 store/cache 的房间 actor（无客户端连接）。
func newTestRoom(t *testing.T, policyRaw string) (*Room, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	reg := provider.NewRegistry()
	authm := auth.NewManager("pw", st)
	c := cache.New(filepath.Join(dir, "cache"), 1<<30, st, reg)
	r := New("r1", "room", "", policyRaw, st, authm, c, reg)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() {
		cancel()
		st.Close()
	})
	return r, st
}

var guest = auth.Identity{ID: "u1", Name: "u1", Kind: "guest", Roles: []string{"requester"}}

// mkEntry 构造长时长条目，避免测试期间自然播完触发切歌。
func mkEntry(ref, requestedBy string) QueueEntry {
	return EntryFromTrack(provider.Track{
		Ref: provider.TrackRef(ref), Title: ref, DurationMs: int64(10 * time.Minute / time.Millisecond),
	}, requestedBy)
}

// queueRefs 返回待播队列（不含正在播放的条目）的 track_ref，按队列顺序。
func queueRefs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.LoadQueue(context.Background(), "r1")
	if err != nil {
		t.Fatalf("LoadQueue: %v", err)
	}
	refs := []string{}
	for _, row := range rows {
		refs = append(refs, row.TrackRef)
	}
	return refs
}

// TestAddBatchAppendsInOrder 批量入队保持提交顺序；单条入队（AddFor）行为不变。
func TestAddBatchAppendsInOrder(t *testing.T) {
	r, st := newTestRoom(t, "")

	// 第一首直接开播（不占待播队列）
	if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if got := queueRefs(t, st); len(got) != 0 {
		t.Fatalf("after auto-play queue = %v, want empty", got)
	}

	batch := []QueueEntry{
		mkEntry("local:t1", guest.ID),
		mkEntry("local:t2", guest.ID),
		mkEntry("local:t3", guest.ID),
	}
	if err := r.AddBatchFor(guest, batch); err != nil {
		t.Fatalf("batch add: %v", err)
	}
	if got, want := queueRefs(t, st), []string{"local:t1", "local:t2", "local:t3"}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}

	// 单条仍走队尾追加
	if err := r.AddFor(guest, mkEntry("local:t4", guest.ID)); err != nil {
		t.Fatalf("single add: %v", err)
	}
	if got, want := queueRefs(t, st), []string{"local:t1", "local:t2", "local:t3", "local:t4"}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

// TestAddBatchAtomicQueueFull max_queue 按批量后投影值校验；超限一条不加。
func TestAddBatchAtomicQueueFull(t *testing.T) {
	r, st := newTestRoom(t, `{"max_queue": 2}`)

	if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	// 投影 0+2 ≤ 2：通过
	if err := r.AddBatchFor(guest, []QueueEntry{mkEntry("local:t1", guest.ID), mkEntry("local:t2", guest.ID)}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// 投影 2+2 > 2：整批拒绝
	if err := r.AddBatchFor(guest, []QueueEntry{mkEntry("local:t3", guest.ID), mkEntry("local:t4", guest.ID)}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second batch err = %v, want ErrQueueFull", err)
	}
	// 单条同样被拒
	if err := r.AddFor(guest, mkEntry("local:t5", guest.ID)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("single add err = %v, want ErrQueueFull", err)
	}
	if got, want := queueRefs(t, st), []string{"local:t1", "local:t2"}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v（失败批次应零追加）", got, want)
	}
}

// TestAddBatchAtomicQuota 按身份待播限额按批量后投影值校验；超限一条不加。
func TestAddBatchAtomicQuota(t *testing.T) {
	r, st := newTestRoom(t, `{"queue_limits": {"guest": 2}}`)

	if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	// 正在播放不计 pending：投影 0+2 ≤ 2，通过
	if err := r.AddBatchFor(guest, []QueueEntry{mkEntry("local:t1", guest.ID), mkEntry("local:t2", guest.ID)}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// 投影 2+1 > 2：整批拒绝
	if err := r.AddBatchFor(guest, []QueueEntry{mkEntry("local:t3", guest.ID)}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second batch err = %v, want ErrQuotaExceeded", err)
	}
	if err := r.AddFor(guest, mkEntry("local:t4", guest.ID)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("single add err = %v, want ErrQuotaExceeded", err)
	}
	if got, want := queueRefs(t, st), []string{"local:t1", "local:t2"}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v（超限批次应零追加）", got, want)
	}

	// 限额按身份计：另一用户不受 u1 占用影响
	other := auth.Identity{ID: "u2", Name: "u2", Kind: "guest", Roles: []string{"requester"}}
	if err := r.AddFor(other, mkEntry("local:t5", other.ID)); err != nil {
		t.Fatalf("other user add: %v", err)
	}
	if got, want := queueRefs(t, st), []string{"local:t1", "local:t2", "local:t5"}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

type recordingClient struct {
	id       auth.Identity
	messages chan any
}

func (c *recordingClient) ID() string              { return c.id.ID }
func (c *recordingClient) Identity() auth.Identity { return c.id }
func (c *recordingClient) Send(msg any)            { c.messages <- msg }

func TestAddSnapshotsRequesterName(t *testing.T) {
	r, st := newTestRoom(t, "")
	id := auth.Identity{
		ID: "u_alice", Name: "Alice", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	if err := r.AddFor(id, mkEntry("local:playing", id.ID)); err != nil {
		t.Fatal(err)
	}

	client := &recordingClient{id: id, messages: make(chan any, 8)}
	r.Join(client)
	var event struct {
		Type string   `json:"type"`
		Data Playback `json:"data"`
	}
	raw, err := json.Marshal(<-client.messages)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "playback.changed" || event.Data.Current == nil {
		t.Fatalf("first snapshot = %s, want playback.changed with current", raw)
	}
	if got := event.Data.Current.RequesterName; got != id.Name {
		t.Fatalf("playback requester_name = %q, want %q", got, id.Name)
	}

	if err := r.AddFor(id, mkEntry("local:queued", id.ID)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadQueue(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequesterName != id.Name {
		t.Fatalf("persisted queue = %#v, want requester_name %q", rows, id.Name)
	}
}

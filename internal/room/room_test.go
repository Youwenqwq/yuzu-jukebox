package room

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
// 当前曲目现在也保留在 room_queue 中，由 current_entry_id 标记，这里跳过它。
func queueRefs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, currentEntryID, err := st.LoadQueue(context.Background(), "r1")
	if err != nil {
		t.Fatalf("LoadQueue: %v", err)
	}
	refs := []string{}
	for _, row := range rows {
		if row.EntryID == currentEntryID {
			continue
		}
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
	id        auth.Identity
	messages  chan any
	interests RoomInterest // 0 = 默认 InterestAll
}

func (c *recordingClient) ID() string              { return c.id.ID }
func (c *recordingClient) Identity() auth.Identity { return c.id }
func (c *recordingClient) Send(msg any)            { c.messages <- msg }
func (c *recordingClient) Interests() RoomInterest {
	if c.interests == 0 {
		return InterestAll
	}
	return c.interests
}

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
	rows, currentEntryID, err := st.LoadQueue(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	// 当前曲目留在队列里（游标指向它），其后是刚点的待播条目。
	if len(rows) != 2 || rows[0].EntryID != currentEntryID ||
		rows[1].TrackRef != "local:queued" || rows[1].RequesterName != id.Name {
		t.Fatalf("persisted queue = %#v (current %q), want current + queued by %q",
			rows, currentEntryID, id.Name)
	}
}

// 当前曲目保留在 room_queue 中（游标标记），因此重启后应当从它本身续播，
// 而不是像以前那样把它当成已出队、直接跳到下一首。
func TestRestartResumesCurrentTrackInsteadOfSkippingIt(t *testing.T) {
	r, st := newTestRoom(t, "")
	if err := r.AddFor(guest, mkEntry("local:playing", guest.ID)); err != nil {
		t.Fatal(err)
	}
	if err := r.AddFor(guest, mkEntry("local:next", guest.ID)); err != nil {
		t.Fatal(err)
	}
	before, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if before.Playback.Current == nil || before.Playback.Current.TrackRef != "local:playing" {
		t.Fatalf("playback before restart = %#v, want local:playing", before.Playback)
	}
	if len(before.Queue) != 1 || before.Queue[0].TrackRef != "local:next" {
		t.Fatalf("queue before restart = %#v, want only local:next", before.Queue)
	}

	// 用同一个 store 重新起一个 actor，模拟进程重启。
	restarted := New("r1", "room", "", "", st, r.authm, r.cache, r.reg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go restarted.Run(ctx)

	after, err := restarted.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if after.Playback.Current == nil || after.Playback.Current.TrackRef != "local:playing" {
		t.Fatalf("playback after restart = %#v, want local:playing resumed", after.Playback)
	}
	if after.Playback.Current.EntryID != before.Playback.Current.EntryID {
		t.Fatalf("resumed entry id = %q, want %q",
			after.Playback.Current.EntryID, before.Playback.Current.EntryID)
	}
	if len(after.Queue) != 1 || after.Queue[0].TrackRef != "local:next" {
		t.Fatalf("queue after restart = %#v, want only local:next", after.Queue)
	}
}

// 正在播放的曲目必须对 SQL 可见：加速层要靠它钉住正在流式传输的对象。
func TestCurrentTrackStaysQueryableInQueue(t *testing.T) {
	r, st := newTestRoom(t, "")
	if err := r.AddFor(guest, mkEntry("local:playing", guest.ID)); err != nil {
		t.Fatal(err)
	}
	if err := r.AddFor(guest, mkEntry("local:next", guest.ID)); err != nil {
		t.Fatal(err)
	}
	rows, currentEntryID, err := st.LoadQueue(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].TrackRef != "local:playing" || rows[1].TrackRef != "local:next" {
		t.Fatalf("persisted queue = %#v, want current followed by upcoming", rows)
	}
	if currentEntryID != rows[0].EntryID {
		t.Fatalf("cursor = %q, want it to point at the playing entry %q", currentEntryID, rows[0].EntryID)
	}
	// 预取视界就是这条查询：游标位置起的前 N 条。
	if refs := queueRefs(t, st); len(refs) != 1 || refs[0] != "local:next" {
		t.Fatalf("upcoming refs = %#v, want only local:next", refs)
	}
}

func TestSnapshotIsReadOnlyAndDoesNotJoin(t *testing.T) {
	r, _ := newTestRoom(t, "")
	id := auth.Identity{
		ID: "u_snapshot", Name: "Snapshot Reader", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	playing := mkEntry("local:playing", id.ID)
	playing.Contributors = []provider.Contributor{{Role: "artist", Name: "Playing Artist"}}
	if err := r.AddFor(id, playing); err != nil {
		t.Fatal(err)
	}
	queued := mkEntry("local:queued", id.ID)
	queued.Contributors = []provider.Contributor{{Role: "artist", Name: "Queued Artist"}}
	if err := r.AddFor(id, queued); err != nil {
		t.Fatal(err)
	}

	snapshot, err := r.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Listeners) != 0 {
		t.Fatalf("snapshot listeners = %#v, want no implicit listener", snapshot.Listeners)
	}
	if snapshot.Playback.Current == nil || len(snapshot.Queue) != 1 {
		t.Fatalf("snapshot state = %#v, want current track and one queued track", snapshot)
	}

	snapshot.Playback.Current.Title = "mutated current"
	snapshot.Playback.Current.Contributors[0].Name = "mutated current contributor"
	snapshot.Queue[0].Title = "mutated queue"
	snapshot.Queue[0].Contributors[0].Name = "mutated queue contributor"

	again, err := r.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Playback.Current.Title; got != "local:playing" {
		t.Fatalf("current title after returned snapshot mutation = %q", got)
	}
	if got := again.Playback.Current.Contributors[0].Name; got != "Playing Artist" {
		t.Fatalf("current contributors after returned snapshot mutation = %q", got)
	}
	if got := again.Queue[0].Title; got != "local:queued" {
		t.Fatalf("queue title after returned snapshot mutation = %q", got)
	}
	if got := again.Queue[0].Contributors[0].Name; got != "Queued Artist" {
		t.Fatalf("queue contributors after returned snapshot mutation = %q", got)
	}
	if len(again.Listeners) != 0 {
		t.Fatalf("second snapshot listeners = %#v, want no implicit listener", again.Listeners)
	}
}

// TestPlayerPlaneReceivesOnlyPlayback ensures headless players only get
// playback.changed. Full queue snapshots are UI state and can exceed the
// default WebSocket frame limit when the queue is long.
func TestPlayerPlaneReceivesOnlyPlayback(t *testing.T) {
	r, _ := newTestRoom(t, "")
	dj := auth.Identity{
		ID: "u_dj", Name: "DJ", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	if err := r.AddFor(dj, mkEntry("local:playing", dj.ID)); err != nil {
		t.Fatal(err)
	}
	batch := make([]QueueEntry, 0, 120)
	for i := range 120 {
		batch = append(batch, mkEntry("local:queued-"+strconv.Itoa(i), dj.ID))
	}
	if err := r.AddBatchFor(dj, batch); err != nil {
		t.Fatal(err)
	}

	player := &recordingClient{
		id: auth.Identity{
			ID: "pl_speaker", Name: "Speaker", Kind: "player", PlayerID: "speaker-1",
		},
		messages:  make(chan any, 8),
		interests: InterestPlayback,
	}
	r.Join(player)

	msg := waitMsg(t, player.messages, time.Second)
	if typ := msgType(t, msg); typ != "playback.changed" {
		t.Fatalf("player first message = %s, want playback.changed", typ)
	}
	select {
	case extra := <-player.messages:
		t.Fatalf("player received unexpected extra message: %s", msgType(t, extra))
	case <-time.After(100 * time.Millisecond):
	}

	listener := &recordingClient{id: dj, messages: make(chan any, 16)}
	r.Join(listener)
	got := map[string]bool{}
	var queueItems []QueueEntry
	var queueRevision uint64
	nextQueuePart := 0
	queueDone := false
	deadline := time.Now().Add(time.Second)
	for !(got["playback.changed"] && queueDone && got["radio.changed"] && got["listeners.changed"]) &&
		time.Now().Before(deadline) {
		select {
		case message := <-listener.messages:
			typ := msgType(t, message)
			if typ != "queue.snapshot" {
				got[typ] = true
				continue
			}
			raw, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var envelope QueueSnapshotMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data.Part != nextQueuePart ||
				(nextQueuePart > 0 && envelope.Data.Revision != queueRevision) {
				t.Fatalf("queue snapshot sequence = revision %d part %d, want revision %d part %d",
					envelope.Data.Revision, envelope.Data.Part, queueRevision, nextQueuePart)
			}
			if nextQueuePart == 0 {
				queueRevision = envelope.Data.Revision
			}
			queueItems = append(queueItems, envelope.Data.Items...)
			nextQueuePart++
			queueDone = envelope.Data.Done
		case <-time.After(time.Until(deadline)):
		}
	}
	if !(got["playback.changed"] && queueDone && got["radio.changed"] && got["listeners.changed"]) {
		t.Fatalf("listener join snapshot incomplete: events=%v queueDone=%v", got, queueDone)
	}
	if len(queueItems) != len(batch) {
		t.Fatalf("reassembled queue has %d items, want %d", len(queueItems), len(batch))
	}
	for i := range batch {
		if queueItems[i].TrackRef != batch[i].TrackRef {
			t.Fatalf("reassembled queue item %d = %q, want %q", i, queueItems[i].TrackRef, batch[i].TrackRef)
		}
	}

	// listeners.changed must not list the headless player.
	second := &recordingClient{
		id: auth.Identity{
			ID: "u_second", Name: "Second", Kind: "guest",
			Roles: []string{auth.RoleListener},
		},
		messages: make(chan any, 8),
	}
	r.Join(second)
	var listenersMsg any
	for {
		m := waitMsg(t, second.messages, time.Second)
		if msgType(t, m) != "listeners.changed" {
			continue
		}
		listenersMsg = m
		break
	}
	raw, err := json.Marshal(listenersMsg)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Listeners []ListenerSnapshot `json:"listeners"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, l := range envelope.Data.Listeners {
		if l.ID == player.id.ID || l.Name == player.id.Name {
			t.Fatalf("listeners included player: %#v", envelope.Data.Listeners)
		}
	}

	if err := r.AddFor(dj, mkEntry("local:extra", dj.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case extra := <-player.messages:
		t.Fatalf("player received queue fanout: %s", msgType(t, extra))
	case <-time.After(100 * time.Millisecond):
	}
	var patchOps []QueuePatchOp
	nextPatchPart := 0
	patchDone := false
	deadline = time.Now().Add(time.Second)
	for !patchDone && time.Now().Before(deadline) {
		select {
		case message := <-listener.messages:
			if msgType(t, message) != "queue.patch" {
				continue
			}
			raw, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var envelope QueuePatchMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data.BaseRevision != queueRevision ||
				envelope.Data.Revision != queueRevision+1 ||
				envelope.Data.Part != nextPatchPart {
				t.Fatalf("queue patch sequence = base %d revision %d part %d",
					envelope.Data.BaseRevision, envelope.Data.Revision, envelope.Data.Part)
			}
			patchOps = append(patchOps, envelope.Data.Ops...)
			nextPatchPart++
			patchDone = envelope.Data.Done
		case <-time.After(time.Until(deadline)):
		}
	}
	if !patchDone || len(patchOps) != 1 || patchOps[0].Op != QueueOpAdd ||
		patchOps[0].Item == nil || patchOps[0].Item.TrackRef != "local:extra" {
		t.Fatalf("listener queue patch = %#v, done=%v", patchOps, patchDone)
	}
	snapshot, err := r.Snapshot(dj)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Queue) != len(batch)+1 || snapshot.Queue[len(batch)].TrackRef != "local:extra" {
		t.Fatalf("final queue state = %#v", snapshot.Queue)
	}
}

func TestQueueProtocolChunksCompleteEnvelopesAndReassemblesSnapshot(t *testing.T) {
	if QueueEnvelopeBudget != 24*1024 {
		t.Fatalf("queue envelope budget = %d, want %d", QueueEnvelopeBudget, 24*1024)
	}
	entries := make([]QueueEntry, 240)
	for i := range entries {
		suffix := strconv.Itoa(i)
		entries[i] = QueueEntry{
			EntryID:  "entry-" + suffix,
			TrackRef: "local:track-" + suffix,
			Title:    strings.Repeat("title-"+suffix+"-", 80),
			Artist:   strings.Repeat("artist-", 20),
		}
	}

	snapshots, err := QueueSnapshotMessages(37, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) < 2 {
		t.Fatalf("large snapshot used %d envelope, want chunking", len(snapshots))
	}
	var reassembled []QueueEntry
	for i, message := range snapshots {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > QueueEnvelopeBudget {
			t.Fatalf("snapshot part %d envelope = %d bytes, budget %d", i, len(encoded), QueueEnvelopeBudget)
		}
		if message.Type != "queue.snapshot" || message.Data.Revision != 37 || message.Data.Part != i {
			t.Fatalf("snapshot part %d metadata = %#v", i, message)
		}
		if message.Data.Done != (i == len(snapshots)-1) {
			t.Fatalf("snapshot part %d done = %v", i, message.Data.Done)
		}
		reassembled = append(reassembled, message.Data.Items...)
	}
	if len(reassembled) != len(entries) {
		t.Fatalf("reassembled snapshot items = %d, want %d", len(reassembled), len(entries))
	}
	for i := range entries {
		if reassembled[i].EntryID != entries[i].EntryID ||
			reassembled[i].TrackRef != entries[i].TrackRef ||
			reassembled[i].Title != entries[i].Title {
			t.Fatalf("reassembled snapshot item %d differs", i)
		}
	}

	ops := make([]QueuePatchOp, len(entries))
	for i := range entries {
		ops[i] = QueuePatchOp{Op: QueueOpAdd, Index: i, Item: &entries[i]}
	}
	patches, err := QueuePatchMessages(37, 38, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) < 2 {
		t.Fatalf("large patch used %d envelope, want chunking", len(patches))
	}
	reassembledOps := 0
	for i, message := range patches {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > QueueEnvelopeBudget {
			t.Fatalf("patch part %d envelope = %d bytes, budget %d", i, len(encoded), QueueEnvelopeBudget)
		}
		if message.Type != "queue.patch" || message.Data.BaseRevision != 37 ||
			message.Data.Revision != 38 || message.Data.Part != i {
			t.Fatalf("patch part %d metadata = %#v", i, message)
		}
		if message.Data.Done != (i == len(patches)-1) {
			t.Fatalf("patch part %d done = %v", i, message.Data.Done)
		}
		reassembledOps += len(message.Data.Ops)
	}
	if reassembledOps != len(ops) {
		t.Fatalf("reassembled patch operations = %d, want %d", reassembledOps, len(ops))
	}
}

func waitMsg(t *testing.T, ch <-chan any, timeout time.Duration) any {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for room message")
		return nil
	}
}

func msgType(t *testing.T, msg any) string {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Type
}

// TestAdvanceSchedulesStartLead 切歌把新曲目的 position 0 排在未来：position_ms
// 为负，客户端用这段窗口装载解码，到点同时开声——否则装载延迟会确定性地
// 吃掉曲目头部（客户端只能 seek 到房间已经走过的位置）。
func TestAdvanceSchedulesStartLead(t *testing.T) {
	r, _ := newTestRoom(t, "")
	before := time.Now().UnixMilli()
	if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	snapshot, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	pb := snapshot.Playback
	if pb.Current == nil || !pb.Playing {
		t.Fatalf("playback = %#v, want a playing current track", pb)
	}
	if pb.PositionMs != -DefaultStartLeadMs {
		t.Fatalf("position_ms = %d, want -%d (start lead)", pb.PositionMs, DefaultStartLeadMs)
	}
	if pb.UpdatedAt < before {
		t.Fatalf("updated_at = %d, want >= %d", pb.UpdatedAt, before)
	}
	// 预定开播时刻 = updated_at + |position_ms|，必须落在切歌之后
	if startAt := pb.UpdatedAt - pb.PositionMs; startAt <= before {
		t.Fatalf("scheduled start %d, want later than switch time %d", startAt, before)
	}
}

// TestStartLeadPolicyOverride 房间策略可覆盖提前量；0 关闭（切歌即 position 0）。
func TestStartLeadPolicyOverride(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   int64
	}{
		{`{"start_lead_ms":0}`, 0},
		{`{"start_lead_ms":1500}`, -1500},
	} {
		r, _ := newTestRoom(t, tc.policy)
		if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
			t.Fatalf("policy %s: seed add: %v", tc.policy, err)
		}
		snapshot, err := r.Snapshot(guest)
		if err != nil {
			t.Fatalf("policy %s: %v", tc.policy, err)
		}
		if got := snapshot.Playback.PositionMs; got != tc.want {
			t.Fatalf("policy %s: position_ms = %d, want %d", tc.policy, got, tc.want)
		}
	}
}

// TestPauseDuringStartLeadFreezesCountdown 提前量窗口内暂停冻结倒计时，
// resume 从剩余量续算——五元组是纯函数，负 position 走的是同一条路径。
func TestPauseDuringStartLeadFreezesCountdown(t *testing.T) {
	// 窗口取足够长，避免测试执行本身耗尽提前量。
	r, _ := newTestRoom(t, `{"start_lead_ms":5000}`)
	if err := r.AddFor(guest, mkEntry("local:t0", guest.ID)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if err := r.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}

	paused, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Playback.Playing {
		t.Fatal("playing = true after pause")
	}
	remaining := paused.Playback.PositionMs
	if remaining >= 0 || remaining < -5000 {
		t.Fatalf("paused position_ms = %d, want a pending negative countdown", remaining)
	}

	if err := r.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumed, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Playback.Playing {
		t.Fatal("playing = false after resume")
	}
	if got := resumed.Playback.PositionMs; got != remaining {
		t.Fatalf("resumed position_ms = %d, want unchanged %d", got, remaining)
	}
}

// TestParsePolicyStartLeadBounds 提前量越界拒绝；缺省回落到默认值。
func TestParsePolicyStartLeadBounds(t *testing.T) {
	for _, raw := range []string{`{"start_lead_ms":-1}`, `{"start_lead_ms":5001}`} {
		if _, err := ParsePolicy(raw); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("ParsePolicy(%s) error = %v, want ErrInvalidPolicy", raw, err)
		}
	}
	p, err := ParsePolicy(`{"max_queue":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.startLeadMs(); got != DefaultStartLeadMs {
		t.Fatalf("unset start_lead_ms = %d, want default %d", got, DefaultStartLeadMs)
	}
	p, err = ParsePolicy(`{"start_lead_ms":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.startLeadMs(); got != 0 {
		t.Fatalf("explicit 0 start_lead_ms = %d, want 0 (disabled)", got)
	}
}

type playReportCall struct {
	id                string
	playedMs, totalMs int64
}

type recordingPlayProvider struct {
	calls chan playReportCall
}

func (*recordingPlayProvider) ID() string { return "scrobble" }
func (*recordingPlayProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*recordingPlayProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref}, nil
}
func (*recordingPlayProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errors.New("测试不需要解析播放地址")
}
func (p *recordingPlayProvider) ReportPlay(_ context.Context, id string, playedMs, totalMs int64) error {
	p.calls <- playReportCall{id: id, playedMs: playedMs, totalMs: totalMs}
	return nil
}

type nonReportingProvider struct{}

func (*nonReportingProvider) ID() string { return "scrobble" }
func (*nonReportingProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*nonReportingProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref}, nil
}
func (*nonReportingProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errors.New("测试不需要解析播放地址")
}

// TestFinishCurrentScrobble 验证播放结束上报只对凭据所有者和达到阈值的曲目触发。
func TestFinishCurrentScrobble(t *testing.T) {
	const (
		ownerID = "owner"
		totalMs = int64(10 * time.Minute / time.Millisecond)
	)
	tests := []struct {
		name        string
		requestedBy string
		playedMs    int64
		reporter    bool
		want        *playReportCall
	}{
		{
			name:        "所有者达到阈值",
			requestedBy: ownerID,
			playedMs:    250_000,
			reporter:    true,
			want:        &playReportCall{id: "track-1", playedMs: 250_000, totalMs: totalMs},
		},
		{
			name:        "非所有者不上报",
			requestedBy: "other",
			playedMs:    250_000,
			reporter:    true,
		},
		{
			name:        "低于阈值不上报",
			requestedBy: ownerID,
			playedMs:    239_999,
			reporter:    true,
		},
		{
			name:        "电台曲目不上报",
			requestedBy: "radio",
			playedMs:    250_000,
			reporter:    true,
		},
		{
			name:        "无上报能力安全跳过",
			requestedBy: ownerID,
			playedMs:    250_000,
			reporter:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, st := newTestRoom(t, `{"start_lead_ms":0}`)
			calls := make(chan playReportCall, 2)
			if tc.reporter {
				r.reg.Register(&recordingPlayProvider{calls: calls})
			} else {
				r.reg.Register(&nonReportingProvider{})
			}

			ctx := context.Background()
			if err := st.UpsertCredential(ctx, "scrobble", "credential", "valid"); err != nil {
				t.Fatalf("UpsertCredential: %v", err)
			}
			if err := st.SetCredentialOwner(ctx, "scrobble", ownerID); err != nil {
				t.Fatalf("SetCredentialOwner: %v", err)
			}
			entry := mkEntry("scrobble:track-1", tc.requestedBy)
			if err := r.AddFor(guest, entry); err != nil {
				t.Fatalf("AddFor: %v", err)
			}
			// 先暂停再定位，使结束时的已播时长不受测试调度耗时影响。
			if err := r.Pause(); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			if err := r.SeekTo(tc.playedMs); err != nil {
				t.Fatalf("SeekTo: %v", err)
			}
			if err := r.Skip(); err != nil {
				t.Fatalf("Skip: %v", err)
			}

			if tc.want == nil {
				select {
				case got := <-calls:
					t.Fatalf("ReportPlay 调用 = %+v，期望不调用", got)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			select {
			case got := <-calls:
				if got != *tc.want {
					t.Fatalf("ReportPlay 调用 = %+v，期望 %+v", got, *tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("等待 ReportPlay 调用超时")
			}
			select {
			case got := <-calls:
				t.Fatalf("ReportPlay 额外调用 = %+v", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

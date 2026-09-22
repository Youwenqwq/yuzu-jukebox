package room

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type readyTestProvider struct {
	url string
	err error
}

func (*readyTestProvider) ID() string { return "ready" }
func (*readyTestProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*readyTestProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref, Title: ref.String()}, nil
}
func (p *readyTestProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{URL: p.url}, p.err
}

// blockedAudio 已完成 Resolve 和首字节，但下载前沿仍未达到就绪阈值。
func blockedAudio(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write([]byte("b"))
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(func() { unblock(); srv.Close() })
	return srv.URL, started, unblock
}

func newReadyRoom(t *testing.T, p provider.Provider, seed func(*store.Store)) (*Room, *store.Store) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	registry.Register(p)
	if seed != nil {
		seed(st)
	}
	trackCache := cache.New(filepath.Join(root, "cache"), 1<<20, 0, st, registry)
	r := New("r1", "room", "", "", st, auth.NewManager("", st), trackCache, registry)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.done; st.Close() })
	return r, st
}

func playbackClient(t *testing.T, r *Room) *recordingClient {
	t.Helper()
	c := &recordingClient{id: guest, messages: make(chan any, 64), interests: InterestPlayback}
	r.Join(c)
	if _, err := r.Snapshot(guest); err != nil {
		t.Fatal(err)
	}
	return c
}

func nextPlayback(t *testing.T, c *recordingClient) Playback {
	t.Helper()
	select {
	case message := <-c.messages:
		return message.(map[string]any)["data"].(Playback)
	case <-time.After(3 * time.Second):
		t.Fatal("等待 playback.changed 超时")
		return Playback{}
	}
}

func waitAudioRequest(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("等待音频下载超时")
	}
}

func cacheReadyEntry(t *testing.T, st *store.Store, ref string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio")
	if err := os.WriteFile(path, []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCacheRow(context.Background(), store.CacheRow{TrackRef: ref, FilePath: path, SizeBytes: 6}); err != nil {
		t.Fatal(err)
	}
}

func TestColdTrackBroadcastsOnlyAfterAudioReady(t *testing.T) {
	url, started, release := blockedAudio(t)
	r, st := newReadyRoom(t, &readyTestProvider{url: url}, nil)
	c := playbackClient(t, r)
	_ = nextPlayback(t, c)
	first := mkEntry("ready:cold", guest.ID)
	if err := r.AddFor(guest, first); err != nil {
		t.Fatal(err)
	}
	waitAudioRequest(t, started)
	for _, command := range []func() error{r.Pause, r.Resume, func() error { return r.SeekTo(20) }} {
		if err := command(); !errors.Is(err, ErrNoPlayback) {
			t.Fatalf("等待就绪时控制结果 = %v", err)
		}
	}
	// 队列修改必须保留等待就绪条目的持久游标，且不得触发额外开播。
	second, third := mkEntry("ready:second", guest.ID), mkEntry("ready:third", guest.ID)
	if err := r.AddBatchFor(guest, []QueueEntry{second, third}); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(third.EntryID, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(second.EntryID); err != nil {
		t.Fatal(err)
	}
	if err := r.ClearQueue(); err != nil {
		t.Fatal(err)
	}
	rows, currentID, err := st.LoadQueue(context.Background(), r.ID)
	if err != nil || currentID != first.EntryID || len(rows) != 1 {
		t.Fatalf("等待就绪期间持久游标丢失: rows=%v current=%s err=%v", rows, currentID, err)
	}
	snapshot, err := r.Snapshot(guest)
	if err != nil || snapshot.Playback.Current != nil || snapshot.Playback.Playing {
		t.Fatalf("音频就绪前已暴露 current: %+v, %v", snapshot.Playback, err)
	}
	select {
	case message := <-c.messages:
		t.Fatalf("音频就绪前广播: %#v", message)
	default:
	}
	readyAt := time.Now().UnixMilli()
	release()
	pb := nextPlayback(t, c)
	if pb.Current == nil || pb.Current.EntryID != first.EntryID || !pb.Playing || pb.PositionMs != -DefaultStartLeadMs || pb.UpdatedAt < readyAt {
		t.Fatalf("就绪后的播放时间线错误: %+v", pb)
	}
}

func TestReadyFailureDrainsWholeQueueWithoutPlaying(t *testing.T) {
	r, st := newReadyRoom(t, &readyTestProvider{err: errors.New("unplayable")}, nil)
	c := playbackClient(t, r)
	_ = nextPlayback(t, c)
	entries := make([]QueueEntry, 5)
	for i := range entries {
		entries[i] = mkEntry(fmt.Sprintf("ready:bad-%d", i), guest.ID)
	}
	if err := r.AddBatchFor(guest, entries); err != nil {
		t.Fatal(err)
	}
	pb := nextPlayback(t, c)
	if pb.Current != nil || pb.Playing {
		t.Fatalf("不可播曲目被广播: %+v", pb)
	}
	history, err := st.PlayHistory(context.Background(), r.ID, 0, 10)
	if err != nil || len(history) != len(entries) {
		t.Fatalf("整队未快速排空: history=%+v err=%v", history, err)
	}
	for _, row := range history {
		if row.EndReason != "unplayable" {
			t.Fatalf("失败条目的历史原因 = %q", row.EndReason)
		}
	}
	snapshot, err := r.Snapshot(guest)
	if err != nil || len(snapshot.Queue) != 0 || snapshot.Playback.Current != nil {
		t.Fatalf("失败后房间未空闲: %+v, %v", snapshot, err)
	}
}

func TestSkipPendingStartsCachedNextAndIgnoresStaleReady(t *testing.T) {
	url, started, release := blockedAudio(t)
	r, st := newReadyRoom(t, &readyTestProvider{url: url}, func(st *store.Store) {
		cacheReadyEntry(t, st, "ready:cached")
	})
	c := playbackClient(t, r)
	_ = nextPlayback(t, c)
	cold, cached := mkEntry("ready:cold", guest.ID), mkEntry("ready:cached", guest.ID)
	if err := r.AddBatchFor(guest, []QueueEntry{cold, cached}); err != nil {
		t.Fatal(err)
	}
	waitAudioRequest(t, started)
	if err := r.Skip(); err != nil {
		t.Fatal(err)
	}
	pb := nextPlayback(t, c)
	if pb.Current == nil || pb.Current.EntryID != cached.EntryID || !pb.Playing {
		t.Fatalf("skip 未立即切到已缓存曲目: %+v", pb)
	}
	release()
	// 延迟成功/失败结果都不能跳过新曲目；同 ref 的旧条目也不能误命中。
	r.inbound <- action{kind: actStartReady, generation: 0, entryID: cold.EntryID, ref: cold.TrackRef}
	r.inbound <- action{kind: actStartReady, generation: 0, entryID: cached.EntryID, ref: cached.TrackRef, err: errors.New("stale")}
	snapshot, err := r.Snapshot(guest)
	if err != nil || snapshot.Playback.Current == nil || snapshot.Playback.Current.EntryID != cached.EntryID {
		t.Fatalf("过期结果误伤新曲目: %+v, %v", snapshot, err)
	}
	history, err := st.PlayHistory(context.Background(), r.ID, 0, 10)
	if err != nil || len(history) != 1 || history[0].TrackRef != cold.TrackRef || history[0].EndReason != "skipped" {
		t.Fatalf("等待就绪时 skip 历史错误: %+v, %v", history, err)
	}
}

func TestRestartWaitsForCurrentAudioReady(t *testing.T) {
	url, started, release := blockedAudio(t)
	entry := mkEntry("ready:resume", guest.ID)
	r, _ := newReadyRoom(t, &readyTestProvider{url: url}, func(st *store.Store) {
		if err := st.ReplaceQueue(context.Background(), "r1", entry.EntryID, []store.QueueRow{{
			EntryID: entry.EntryID, TrackRef: entry.TrackRef, Title: entry.Title, DurationMs: entry.DurationMs,
		}}); err != nil {
			t.Fatal(err)
		}
	})
	waitAudioRequest(t, started)
	c := playbackClient(t, r)
	if pb := nextPlayback(t, c); pb.Current != nil || pb.Playing {
		t.Fatalf("重启时音频未就绪就开播: %+v", pb)
	}
	release()
	if pb := nextPlayback(t, c); pb.Current == nil || pb.Current.EntryID != entry.EntryID || !pb.Playing || pb.PositionMs != -DefaultStartLeadMs {
		t.Fatalf("重启就绪后未续播原曲目: %+v", pb)
	}
}

func TestReadyFailureAdvancesToCachedPlayableTrack(t *testing.T) {
	r, _ := newReadyRoom(t, &readyTestProvider{err: errors.New("unplayable")}, func(st *store.Store) {
		cacheReadyEntry(t, st, "ready:good")
	})
	c := playbackClient(t, r)
	_ = nextPlayback(t, c)
	if err := r.AddBatchFor(guest, []QueueEntry{
		mkEntry("ready:bad", guest.ID), mkEntry("ready:good", guest.ID),
	}); err != nil {
		t.Fatal(err)
	}
	pb := nextPlayback(t, c)
	if pb.Current == nil || pb.Current.TrackRef != "ready:good" || !pb.Playing {
		t.Fatalf("失败条目未静默跳到可播条目: %+v", pb)
	}
}

func TestStaleReadyCannotStartRepeatedPendingTrack(t *testing.T) {
	url, started, release := blockedAudio(t)
	r, _ := newReadyRoom(t, &readyTestProvider{url: url}, nil)
	c := playbackClient(t, r)
	_ = nextPlayback(t, c)
	first, second := mkEntry("ready:repeat", guest.ID), mkEntry("ready:repeat", guest.ID)
	if err := r.AddBatchFor(guest, []QueueEntry{first, second}); err != nil {
		t.Fatal(err)
	}
	waitAudioRequest(t, started)
	if err := r.Skip(); err != nil {
		t.Fatal(err)
	}
	// 即使 ref 和当前条目相同，旧代次的成功/失败也不能越过门控。
	r.inbound <- action{kind: actStartReady, generation: 0, entryID: second.EntryID, ref: second.TrackRef}
	r.inbound <- action{kind: actStartReady, generation: 0, entryID: second.EntryID, ref: second.TrackRef, err: errors.New("stale")}
	snapshot, err := r.Snapshot(guest)
	if err != nil || snapshot.Playback.Current != nil || snapshot.Playback.Playing {
		t.Fatalf("过期就绪结果越过门控: %+v, %v", snapshot.Playback, err)
	}
	r.ReportPlayerMetrics(c.ID(), PlayerMetrics{LoadMs: 1000})
	if _, err := r.Snapshot(guest); err != nil {
		t.Fatal(err)
	}
	release()
	pb := nextPlayback(t, c)
	if pb.Current == nil || pb.Current.EntryID != second.EntryID || !pb.Playing || pb.PositionMs != -1150 {
		t.Fatalf("重复条目就绪后未使用最新提前量: %+v", pb)
	}
}

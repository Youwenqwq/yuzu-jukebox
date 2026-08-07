package room

import (
	"context"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// brokenResolveProvider 模拟源无效：Resolve 恒失败（如 qq 无凭据的 104003）。
type brokenResolveProvider struct{}

func (*brokenResolveProvider) ID() string { return "broken" }
func (*brokenResolveProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*brokenResolveProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref, Title: ref.String()}, nil
}
func (*brokenResolveProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errUnplayable
}

var errUnplayable = context.DeadlineExceeded // 任意错误即可，测试只看"失败"

// TestAdvanceSkipsUnplayableCurrent 验证当前曲目 Resolve 失败时房间自动切到下一首，
// 且无效曲目以 end_reason="unplayable" 记入历史。
func TestAdvanceSkipsUnplayableCurrent(t *testing.T) {
	r, st := newTestRoom(t, "")
	r.reg.Register(&brokenResolveProvider{})

	if err := r.AddFor(guest, mkEntry("broken:t1", guest.ID)); err != nil {
		t.Fatalf("AddFor broken: %v", err)
	}
	if err := r.AddFor(guest, mkEntry("local:t2", guest.ID)); err != nil {
		t.Fatalf("AddFor playable: %v", err)
	}

	// 自动开播后：current 应为 broken:t1 → 预检失败 → 自动跳到 local:t2。
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := r.Snapshot(guest)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.Playback.Current != nil && snap.Playback.Current.TrackRef == "local:t2" {
			break
		}
		if time.Now().After(deadline) {
			cur := ""
			if snap.Playback.Current != nil {
				cur = snap.Playback.Current.TrackRef
			}
			t.Fatalf("current = %q, want auto-skip to local:t2", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx := context.Background()
	history, err := st.PlayHistory(ctx, r.ID, 0, 10)
	if err != nil {
		t.Fatalf("PlayHistory: %v", err)
	}
	found := false
	for _, row := range history {
		if row.TrackRef == "broken:t1" {
			found = true
			if row.EndReason != "unplayable" {
				t.Fatalf("broken:t1 end_reason = %q, want unplayable", row.EndReason)
			}
		}
	}
	if !found {
		t.Fatalf("broken:t1 missing from play history: %+v", history)
	}
}

// TestUnplayableReportStaleRefIgnored 预检回报到达时曲目已切走：不得误跳新曲目。
func TestUnplayableReportStaleRefIgnored(t *testing.T) {
	r, _ := newTestRoom(t, "")

	// 首曲目可播；预检完成前手动 skip 到下一首——预检回报（若有）必须被忽略。
	if err := r.AddFor(guest, mkEntry("local:t1", guest.ID)); err != nil {
		t.Fatalf("AddFor: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, err := r.Snapshot(guest)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Playback.Current != nil && snap.Playback.Current.TrackRef == "local:t1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("current never reached local:t1")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.Skip(); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	// 直接投递一个过期回报：ref 已不是当前曲目，必须被忽略（房间保持空闲而非死循环）。
	if err := r.call(action{kind: actUnplayable, ref: "local:t1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	snap, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Playback.Current != nil {
		t.Fatalf("stale unplayable report advanced playback: %#v", snap.Playback.Current)
	}
}

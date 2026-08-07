package room

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestParsePolicyDefaultMaxQueue(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"max_queue":0}`} {
		policy, err := ParsePolicy(raw)
		if err != nil {
			t.Fatalf("ParsePolicy(%q): %v", raw, err)
		}
		if policy.MaxQueue != DefaultMaxQueue {
			t.Fatalf("ParsePolicy(%q).MaxQueue = %d, want %d", raw, policy.MaxQueue, DefaultMaxQueue)
		}
	}

	policy, err := ParsePolicy(`{"max_queue":7}`)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxQueue != 7 {
		t.Fatalf("explicit MaxQueue = %d, want 7", policy.MaxQueue)
	}
}

func TestDefaultMaxQueueIsEnforced(t *testing.T) {
	r, _ := newTestRoom(t, "")
	entries := make([]QueueEntry, DefaultMaxQueue)
	for i := range entries {
		entries[i] = mkEntry(fmt.Sprintf("local:default-%d", i), guest.ID)
	}
	if err := r.AddBatchFor(guest, entries); err != nil {
		t.Fatalf("fill default queue: %v", err)
	}
	// Auto-play consumes one of the initial entries, leaving 49 pending. One more
	// reaches the effective cap; the following command must be rejected.
	if err := r.AddFor(guest, mkEntry("local:at-cap", guest.ID)); err != nil {
		t.Fatalf("add at default cap: %v", err)
	}
	if err := r.AddFor(guest, mkEntry("local:over-cap", guest.ID)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("add beyond default cap error = %v, want ErrQueueFull", err)
	}
}

type overproducingRadioSource struct {
	limits []int
	next   int
}

func (s *overproducingRadioSource) NextBatch(_ context.Context, limit int, _ provider.TrackRef) ([]provider.Track, bool, error) {
	s.limits = append(s.limits, limit)
	tracks := make([]provider.Track, 10)
	for i := range tracks {
		s.next++
		tracks[i] = provider.Track{
			Ref:   provider.TrackRef(fmt.Sprintf("radio:%d", s.next)),
			Title: "radio", DurationMs: int64(10 * time.Minute / time.Millisecond),
		}
	}
	return tracks, false, nil
}

func (*overproducingRadioSource) Description() string { return "overproducing test source" }
func (*overproducingRadioSource) Finite() bool        { return false }

type cappedRadioProvider struct {
	source *overproducingRadioSource
}

func (*cappedRadioProvider) ID() string { return "radio" }
func (*cappedRadioProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*cappedRadioProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref, Title: ref.String()}, nil
}
func (*cappedRadioProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{URL: "file:///nonexistent-in-test"}, nil
}
func (p *cappedRadioProvider) NewSource(context.Context, string) (provider.TrackSource, error) {
	return p.source, nil
}

func TestRadioRefillHonorsMaxQueue(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := &overproducingRadioSource{}
	registry := provider.NewRegistry()
	registry.Register(&cappedRadioProvider{source: source})
	authm := auth.NewManager("", st)
	trackCache := cache.New(filepath.Join(root, "cache"), 1<<20, 0, st, registry)
	r := New("r1", "room", "", `{"max_queue":2}`, st, authm, trackCache, registry)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() {
		cancel()
		st.Close()
	})

	if err := r.PlayRadio("radio:test", false, false); err != nil {
		t.Fatalf("PlayRadio: %v", err)
	}
	snapshot, err := r.Snapshot(auth.Identity{ID: "listener"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snapshot.Queue); got != 2 {
		t.Fatalf("radio pending queue length = %d, want max_queue 2", got)
	}
	if want := []int{2, 1}; !slices.Equal(source.limits, want) {
		t.Fatalf("radio source batch limits = %v, want %v", source.limits, want)
	}
}

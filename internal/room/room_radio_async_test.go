package room

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type asyncRadioBatch struct {
	tracks    []provider.Track
	exhausted bool
	err       error
}

type asyncRadioCall struct {
	limit int
	seed  provider.TrackRef
}

type blockingRadioSource struct {
	calls   chan asyncRadioCall
	results chan asyncRadioBatch
}

func newBlockingRadioSource() *blockingRadioSource {
	return &blockingRadioSource{
		calls:   make(chan asyncRadioCall, 8),
		results: make(chan asyncRadioBatch, 8),
	}
}

func (s *blockingRadioSource) NextBatch(ctx context.Context, limit int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	select {
	case s.calls <- asyncRadioCall{limit: limit, seed: seed}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	select {
	case result := <-s.results:
		return result.tracks, result.exhausted, result.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (*blockingRadioSource) Description() string { return "blocking radio test source" }
func (*blockingRadioSource) Finite() bool        { return false }

type asyncRadioProvider struct {
	source              provider.TrackSource
	constructionStarted chan struct{}
	constructionRelease <-chan struct{}
}

func (*asyncRadioProvider) ID() string { return "slowradio" }
func (*asyncRadioProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*asyncRadioProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref, Title: ref.String()}, nil
}
func (*asyncRadioProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{URL: "file:///nonexistent-in-test"}, nil
}
func (p *asyncRadioProvider) NewSource(ctx context.Context, _ string) (provider.TrackSource, error) {
	if p.constructionStarted != nil {
		select {
		case p.constructionStarted <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.constructionRelease != nil {
		select {
		case <-p.constructionRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.source, nil
}

func newAsyncRadioRoom(t *testing.T, radioProvider provider.Provider, policyRaw string) *Room {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	registry.Register(&playableTestProvider{})
	registry.Register(radioProvider)
	authm := auth.NewManager("", st)
	trackCache := cache.New(filepath.Join(root, "cache"), 1<<20, 0, st, registry)
	r := New("r1", "room", "", policyRaw, st, authm, trackCache, registry)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() {
		cancel()
		<-r.done
		st.Close()
	})
	return r
}

func waitRadioCall(t *testing.T, calls <-chan asyncRadioCall) asyncRadioCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for radio refill")
		return asyncRadioCall{}
	}
}

func requireCallCompletes(t *testing.T, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("room action blocked behind radio I/O")
		return nil
	}
}

func waitRadioSnapshot(t *testing.T, r *Room, accept func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err := r.Snapshot(guest)
		if err != nil {
			t.Fatal(err)
		}
		if accept(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for radio state: %#v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

func radioTracks(first, count int) []provider.Track {
	tracks := make([]provider.Track, count)
	for i := range tracks {
		ref := provider.TrackRef("slowradio:" + strconv.Itoa(first+i))
		tracks[i] = provider.Track{
			Ref: ref, Title: ref.String(), DurationMs: int64(10 * time.Minute / time.Millisecond),
		}
	}
	return tracks
}

func TestRadioSourceConstructionDoesNotBlockActorAndStoppedResultIsDiscarded(t *testing.T) {
	source := newBlockingRadioSource()
	started := make(chan struct{})
	release := make(chan struct{})
	r := newAsyncRadioRoom(t, &asyncRadioProvider{
		source: source, constructionStarted: started, constructionRelease: release,
	}, "")

	playDone := make(chan error, 1)
	go func() { playDone <- r.PlayRadio("slowradio:test", false, false) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for radio source construction")
	}

	if err := requireCallCompletes(t, func() error {
		return r.AddFor(guest, mkEntry("local:during-construction", guest.ID))
	}); err != nil {
		t.Fatalf("queue add during source construction: %v", err)
	}
	if err := requireCallCompletes(t, r.StopRadio); err != nil {
		t.Fatalf("stop radio during source construction: %v", err)
	}
	close(release)
	select {
	case <-playDone:
	case <-time.After(time.Second):
		t.Fatal("stale PlayRadio did not return")
	}

	snapshot, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Radio != nil {
		t.Fatalf("late source construction reactivated radio: %#v", snapshot.Radio)
	}
	if snapshot.Playback.Current == nil || snapshot.Playback.Current.TrackRef != "local:during-construction" {
		t.Fatalf("playback after responsive add = %#v", snapshot.Playback)
	}
	select {
	case call := <-source.calls:
		t.Fatalf("discarded source was refilled: %#v", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRadioSourceConstructionIsSingleFlightAndLatestRequestWins(t *testing.T) {
	source := newBlockingRadioSource()
	started := make(chan struct{})
	release := make(chan struct{})
	r := newAsyncRadioRoom(t, &asyncRadioProvider{
		source: source, constructionStarted: started, constructionRelease: release,
	}, "")

	firstDone := make(chan error, 1)
	go func() { firstDone <- r.PlayRadio("slowradio:first", false, false) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first source construction")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- r.PlayRadio("slowradio:second", false, false) }()
	select {
	case <-started:
		t.Fatal("second source construction ran concurrently with the first")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued source construction did not start")
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("superseded PlayRadio did not return")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("latest PlayRadio: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("latest PlayRadio did not return")
	}
	snapshot, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Radio == nil || snapshot.Radio.Source != "slowradio:second" {
		t.Fatalf("radio after superseded construction = %#v", snapshot.Radio)
	}
}

func TestQueueAddCompletesWhileRadioRefillIsBlocked(t *testing.T) {
	source := newBlockingRadioSource()
	r := newAsyncRadioRoom(t, &asyncRadioProvider{source: source}, "")
	if err := r.PlayRadio("slowradio:test", false, false); err != nil {
		t.Fatal(err)
	}
	waitRadioCall(t, source.calls)

	if err := requireCallCompletes(t, func() error {
		return r.AddFor(guest, mkEntry("local:during-refill", guest.ID))
	}); err != nil {
		t.Fatalf("queue add during refill: %v", err)
	}
	source.results <- asyncRadioBatch{tracks: radioTracks(1, 4)}
	snapshot := waitRadioSnapshot(t, r, func(snapshot Snapshot) bool {
		return snapshot.Playback.Current != nil &&
			snapshot.Playback.Current.TrackRef == "local:during-refill" && len(snapshot.Queue) == 4
	})
	if snapshot.Radio == nil {
		t.Fatal("radio stopped after successful background refill")
	}
}

func TestRadioRefillResultClampsToCurrentQueueCapacity(t *testing.T) {
	source := newBlockingRadioSource()
	r := newAsyncRadioRoom(t, &asyncRadioProvider{source: source}, `{"max_queue":2}`)
	if err := r.AddFor(guest, mkEntry("local:playing", guest.ID)); err != nil {
		t.Fatal(err)
	}
	if err := r.PlayRadio("slowradio:test", false, false); err != nil {
		t.Fatal(err)
	}
	call := waitRadioCall(t, source.calls)
	if call.limit != 2 {
		t.Fatalf("refill limit = %d, want 2", call.limit)
	}
	if err := requireCallCompletes(t, func() error {
		return r.AddBatchFor(guest, []QueueEntry{
			mkEntry("local:queued-1", guest.ID),
			mkEntry("local:queued-2", guest.ID),
		})
	}); err != nil {
		t.Fatalf("fill queue during refill: %v", err)
	}
	source.results <- asyncRadioBatch{tracks: radioTracks(1, 2), exhausted: true}
	snapshot := waitRadioSnapshot(t, r, func(snapshot Snapshot) bool { return snapshot.Radio == nil })
	if len(snapshot.Queue) != 2 ||
		snapshot.Queue[0].TrackRef != "local:queued-1" ||
		snapshot.Queue[1].TrackRef != "local:queued-2" {
		t.Fatalf("queue after capacity changed during refill = %#v", snapshot.Queue)
	}
}

func TestRadioPendingStartAndSingleInflightRefill(t *testing.T) {
	source := newBlockingRadioSource()
	r := newAsyncRadioRoom(t, &asyncRadioProvider{source: source}, "")
	if err := r.PlayRadio("slowradio:test", false, false); err != nil {
		t.Fatal(err)
	}
	first := waitRadioCall(t, source.calls)
	if first.limit != 10 || first.seed != "" {
		t.Fatalf("first refill = %#v, want limit 10 with empty seed", first)
	}

	if err := requireCallCompletes(t, r.Skip); err != nil {
		t.Fatalf("first skip during refill: %v", err)
	}
	if err := requireCallCompletes(t, r.Skip); err != nil {
		t.Fatalf("second skip during refill: %v", err)
	}
	select {
	case extra := <-source.calls:
		t.Fatalf("concurrent advance scheduled a second refill: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}

	source.results <- asyncRadioBatch{tracks: radioTracks(1, 4)}
	snapshot := waitRadioSnapshot(t, r, func(snapshot Snapshot) bool {
		return snapshot.Playback.Current != nil &&
			snapshot.Playback.Current.TrackRef == "slowradio:1" && len(snapshot.Queue) == 3
	})
	if snapshot.Radio == nil {
		t.Fatal("radio stopped after non-exhausted refill")
	}
	select {
	case extra := <-source.calls:
		t.Fatalf("full post-start queue triggered an extra refill: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRadioExhaustionStopsAfterPendingRefillResult(t *testing.T) {
	source := newBlockingRadioSource()
	r := newAsyncRadioRoom(t, &asyncRadioProvider{source: source}, "")
	if err := r.PlayRadio("slowradio:test", false, false); err != nil {
		t.Fatal(err)
	}
	waitRadioCall(t, source.calls)

	before, err := r.Snapshot(guest)
	if err != nil {
		t.Fatal(err)
	}
	if before.Radio == nil {
		t.Fatal("radio was not active while its first refill was pending")
	}
	source.results <- asyncRadioBatch{exhausted: true}
	after := waitRadioSnapshot(t, r, func(snapshot Snapshot) bool { return snapshot.Radio == nil })
	if after.Playback.Current != nil {
		t.Fatalf("exhausted empty radio started playback: %#v", after.Playback)
	}
}

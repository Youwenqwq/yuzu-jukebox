package plsync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/coverurl"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type fakeProvider struct {
	id       string
	calls    int
	remoteID string
	deadline time.Time
	name     string
	coverURL string
	tracks   []provider.Track
	err      error
}

func (p *fakeProvider) ID() string { return p.id }
func (p *fakeProvider) Search(context.Context, string) ([]provider.Track, error) {
	return nil, nil
}
func (p *fakeProvider) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, provider.ErrNotSupported
}
func (p *fakeProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, provider.ErrNotSupported
}
func (p *fakeProvider) ImportPlaylist(ctx context.Context, remoteID string) (string, string, []provider.Track, error) {
	p.calls++
	p.remoteID = remoteID
	p.deadline, _ = ctx.Deadline()
	return p.name, p.coverURL, p.tracks, p.err
}

type providerWithoutImporter struct{ id string }

func (p *providerWithoutImporter) ID() string { return p.id }
func (p *providerWithoutImporter) Search(context.Context, string) ([]provider.Track, error) {
	return nil, nil
}
func (p *providerWithoutImporter) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, provider.ErrNotSupported
}
func (p *providerWithoutImporter) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, provider.ErrNotSupported
}

func openSyncStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func createSyncPlaylist(t *testing.T, st *store.Store, id, providerID, remoteID string) {
	t.Helper()
	if err := st.CreatePlaylist(context.Background(), store.Playlist{
		ID: id, Name: "old name", CreatedAt: 1, UpdatedAt: 1,
		BoundProvider: providerID, BoundRemoteID: remoteID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncOneSuccessReplacesItems(t *testing.T) {
	st := openSyncStore(t)
	createSyncPlaylist(t, st, "bound", "fake", "remote-42")
	if err := st.ReplacePlaylistItems(context.Background(), "bound", []store.PlaylistItem{{TrackRef: "fake:old"}}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{
		id: "fake", name: "new name", coverURL: "https://img.example/playlist.jpg",
		tracks: []provider.Track{
			{Ref: provider.NewRef("fake", "1"), Title: "one", Artist: "artist one", DurationMs: 1000,
				Album: "album", CoverURL: "https://cover", SourceURL: "https://source",
				Contributors: []provider.Contributor{{Role: "artist", Name: "artist one"}}},
			{Ref: provider.NewRef("fake", "2"), Title: "two", Artist: "artist two", DurationMs: 2000},
		},
	}
	reg := provider.NewRegistry()
	reg.Register(fake)

	signer := coverurl.New([]byte("playlist-cover-test-key"))
	started := time.Now()
	count, err := SyncOne(context.Background(), st, reg, signer, "bound")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || fake.calls != 1 || fake.remoteID != "remote-42" {
		t.Fatalf("count=%d calls=%d remote=%q", count, fake.calls, fake.remoteID)
	}
	if fake.deadline.IsZero() || fake.deadline.Before(started.Add(119*time.Second)) || fake.deadline.After(started.Add(121*time.Second)) {
		t.Fatalf("import deadline = %v, want approximately 120s", fake.deadline)
	}
	items, err := st.PlaylistItems(context.Background(), "bound", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Ord != 0 || items[0].TrackRef != "fake:1" ||
		items[0].Album != "album" || items[0].CoverURL != "https://cover" ||
		items[0].SourceURL != "https://source" || !strings.Contains(items[0].ContributorsJSON, "artist one") ||
		items[1].Ord != 1 || items[1].TrackRef != "fake:2" {
		t.Fatalf("synced items = %+v", items)
	}
	playlist, err := st.GetPlaylist(context.Background(), "bound")
	if err != nil {
		t.Fatal(err)
	}
	if playlist.Name != "new name" || playlist.LastSyncAt == 0 || playlist.LastSyncError != "" ||
		!strings.HasPrefix(playlist.CoverURL, "/api/v1/cover/ext/") || playlist.CoverPath != "" {
		t.Fatalf("synced playlist = %+v", playlist)
	}
	token := strings.TrimPrefix(playlist.CoverURL, "/api/v1/cover/ext/")
	providerID, rawURL, ok := signer.Open(token)
	if !ok || providerID != "fake" || rawURL != fake.coverURL {
		t.Fatalf("minted cover = (%q, %q, %v), want fake/%q", providerID, rawURL, ok, fake.coverURL)
	}
}

func TestSyncOneEmptyCoverPreservesExistingCover(t *testing.T) {
	st := openSyncStore(t)
	createSyncPlaylist(t, st, "bound", "fake", "remote")
	const existingCover = "/api/v1/cover/ext/existing-token"
	if err := st.SetPlaylistCover(context.Background(), "bound", existingCover, "/tmp/old-cover"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{id: "fake", name: "new name"}
	reg := provider.NewRegistry()
	reg.Register(fake)

	if _, err := SyncOne(context.Background(), st, reg, coverurl.New([]byte("test-key")), "bound"); err != nil {
		t.Fatal(err)
	}
	playlist, err := st.GetPlaylist(context.Background(), "bound")
	if err != nil {
		t.Fatal(err)
	}
	if playlist.CoverURL != existingCover || playlist.CoverPath != "" {
		t.Fatalf("empty imported cover changed existing cover: %+v", playlist)
	}
}

func TestSyncOneFailurePreservesItemsAndRecordsError(t *testing.T) {
	st := openSyncStore(t)
	createSyncPlaylist(t, st, "bound", "fake", "remote")
	if err := st.ReplacePlaylistItems(context.Background(), "bound", []store.PlaylistItem{{TrackRef: "fake:old", Title: "old"}}); err != nil {
		t.Fatal(err)
	}
	providerErr := errors.New("upstream unavailable")
	fake := &fakeProvider{id: "fake", err: providerErr}
	reg := provider.NewRegistry()
	reg.Register(fake)

	if _, err := SyncOne(context.Background(), st, reg, coverurl.New([]byte("test-key")), "bound"); !errors.Is(err, providerErr) {
		t.Fatalf("SyncOne error = %v, want %v", err, providerErr)
	}
	items, err := st.PlaylistItems(context.Background(), "bound", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TrackRef != "fake:old" {
		t.Fatalf("failed sync changed items: %+v", items)
	}
	playlist, err := st.GetPlaylist(context.Background(), "bound")
	if err != nil {
		t.Fatal(err)
	}
	if playlist.Name != "old name" || playlist.LastSyncAt == 0 || playlist.LastSyncError != providerErr.Error() {
		t.Fatalf("failed sync result = %+v", playlist)
	}
}

func TestSyncOneRejectsUnboundPlaylist(t *testing.T) {
	st := openSyncStore(t)
	createSyncPlaylist(t, st, "unbound", "", "")
	if _, err := SyncOne(context.Background(), st, provider.NewRegistry(), coverurl.New([]byte("test-key")), "unbound"); err == nil || !strings.Contains(err.Error(), "not provider-bound") {
		t.Fatalf("SyncOne error = %v", err)
	}
}

func TestSyncOneRejectsProviderWithoutPlaylistImporter(t *testing.T) {
	st := openSyncStore(t)
	createSyncPlaylist(t, st, "bound", "plain", "remote")
	reg := provider.NewRegistry()
	reg.Register(&providerWithoutImporter{id: "plain"})

	if _, err := SyncOne(context.Background(), st, reg, coverurl.New([]byte("test-key")), "bound"); err == nil || !strings.Contains(err.Error(), "does not support playlist import") {
		t.Fatalf("SyncOne error = %v", err)
	}
	playlist, err := st.GetPlaylist(context.Background(), "bound")
	if err != nil {
		t.Fatal(err)
	}
	if playlist.LastSyncAt == 0 || !strings.Contains(playlist.LastSyncError, "does not support playlist import") {
		t.Fatalf("capability failure not recorded: %+v", playlist)
	}
}

func TestRunDisabledReturnsImmediately(t *testing.T) {
	syncer := New(provider.NewRegistry(), openSyncStore(t), coverurl.New([]byte("test-key")))
	done := make(chan struct{})
	go func() {
		syncer.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return with periodic sync disabled")
	}
}

func TestIsDue(t *testing.T) {
	now := time.UnixMilli(100_000)
	interval := 10 * time.Second
	for _, tt := range []struct {
		name       string
		lastSyncAt int64
		want       bool
	}{
		{name: "never synced", lastSyncAt: 0, want: true},
		{name: "exact boundary", lastSyncAt: 90_000, want: true},
		{name: "not due", lastSyncAt: 90_001, want: false},
		{name: "future attempt", lastSyncAt: 110_000, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDue(store.Playlist{LastSyncAt: tt.lastSyncAt}, now, interval); got != tt.want {
				t.Fatalf("isDue() = %v, want %v", got, tt.want)
			}
		})
	}
}

var _ provider.PlaylistImporter = (*fakeProvider)(nil)
var _ provider.Provider = (*providerWithoutImporter)(nil)

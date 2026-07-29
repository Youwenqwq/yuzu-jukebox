package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type directoryClient struct{ identity auth.Identity }

func (c *directoryClient) ID() string              { return c.identity.ID }
func (c *directoryClient) Identity() auth.Identity { return c.identity }
func (c *directoryClient) Send(any)                {}
func (c *directoryClient) Interests() room.RoomInterest {
	return room.InterestAll
}

type roomsResponse struct {
	Rooms []struct {
		ID            string          `json:"id"`
		ListenerCount int             `json:"listener_count"`
		NowPlaying    *roomNowPlaying `json:"now_playing"`
	} `json:"rooms"`
}

type roomNowPlaying struct {
	Title      string  `json:"title"`
	Artist     string  `json:"artist"`
	DurationMs int64   `json:"duration_ms"`
	CoverURL   string  `json:"cover_url"`
	PositionMs int64   `json:"position_ms"`
	UpdatedAt  int64   `json:"updated_at"`
	Playing    bool    `json:"playing"`
	Rate       float64 `json:"rate"`
}

func TestListRoomsIncludesLiveDirectorySummary(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		st.Close()
	})

	authm := auth.NewManager("test", st)
	reg := provider.NewRegistry()
	roomCache := cache.New(filepath.Join(dir, "cache"), 1<<30, st, reg)
	rooms := room.NewManager(ctx, st, authm, roomCache, reg, nil)
	for i, row := range []store.Room{
		{ID: "active", Name: "Active", CreatedAt: 1},
		{ID: "idle", Name: "Idle", CreatedAt: 2},
	} {
		if err := st.CreateRoom(ctx, row); err != nil {
			t.Fatalf("create room %d: %v", i, err)
		}
	}
	active := rooms.Spawn(store.Room{ID: "active", Name: "Active", CreatedAt: 1})
	rooms.Spawn(store.Room{ID: "idle", Name: "Idle", CreatedAt: 2})

	identity := auth.Identity{
		ID: "u1", Name: "Alice", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	active.Join(&directoryClient{identity: identity})
	entry := room.EntryFromTrack(provider.Track{
		Ref: provider.TrackRef("local:t1"), Title: "Now", Artist: "Artist",
		DurationMs: int64(5 * time.Minute / time.Millisecond), CoverURL: "https://origin.invalid/cover.jpg",
	}, identity.ID)
	if err := active.AddFor(identity, entry); err != nil {
		t.Fatal(err)
	}

	token := authm.IssueSession(identity)
	s := &Server{st: st, authm: authm, rooms: rooms}
	request := func() (roomsResponse, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.listRooms(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "stream_url") {
			t.Fatalf("rooms response leaked stream_url: %s", body)
		}
		var response roomsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response, body
	}

	response, body := request()
	byID := make(map[string]struct {
		listeners int
		now       *roomNowPlaying
	})
	for _, info := range response.Rooms {
		byID[info.ID] = struct {
			listeners int
			now       *roomNowPlaying
		}{info.ListenerCount, info.NowPlaying}
	}
	if got := byID["idle"]; got.listeners != 0 || got.now != nil {
		t.Fatalf("idle summary = %#v, response = %s", got, body)
	}
	got := byID["active"]
	if got.listeners != 1 || got.now == nil {
		t.Fatalf("active summary = %#v, response = %s", got, body)
	}
	if got.now.Title != "Now" || got.now.Artist != "Artist" || got.now.DurationMs != entry.DurationMs ||
		!got.now.Playing || got.now.Rate != 1 || got.now.UpdatedAt == 0 ||
		!strings.HasPrefix(got.now.CoverURL, "/api/v1/cover/") {
		t.Fatalf("now_playing = %#v", got.now)
	}

	if err := active.Pause(); err != nil {
		t.Fatal(err)
	}
	response, _ = request()
	for _, info := range response.Rooms {
		if info.ID == "active" && (info.NowPlaying == nil || info.NowPlaying.Playing) {
			t.Fatalf("paused now_playing = %#v", info.NowPlaying)
		}
	}
}

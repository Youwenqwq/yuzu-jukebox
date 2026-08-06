package ncm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestLikeCheck(t *testing.T) {
	tests := []struct {
		name string
		ids  []int64
		want bool
	}{
		{name: "liked", ids: []int64{123}, want: true},
		{name: "not liked", ids: []int64{456}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/song/like/check" {
					t.Errorf("path = %q, want /song/like/check", r.URL.Path)
				}
				if got := r.URL.Query().Get("ids"); got != "[123]" {
					t.Errorf("ids = %q, want [123]", got)
				}
				if got := r.URL.Query().Get("cookie"); got != "MUSIC_U=test" {
					t.Errorf("cookie = %q, want MUSIC_U=test", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "ids": tt.ids})
			}))
			defer server.Close()

			p := &Provider{base: server.URL, writeClient: server.Client()}
			p.cookie.Store("MUSIC_U=test")
			got, err := p.LikeCheck(context.Background(), "123")
			if err != nil {
				t.Fatalf("LikeCheck() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("LikeCheck() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLikeCheckEmptyCookieDoesNotRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	p := &Provider{base: server.URL, writeClient: server.Client()}
	p.cookie.Store("")
	if _, err := p.LikeCheck(context.Background(), "123"); err == nil {
		t.Fatal("LikeCheck() error = nil, want missing credential error")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestAccountPlaylistsMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/playlist" {
			t.Errorf("path = %q, want /user/playlist", r.URL.Path)
		}
		if got := r.URL.Query().Get("uid"); got != "9988" {
			t.Errorf("uid = %q, want 9988", got)
		}
		if got := r.URL.Query().Get("cookie"); got != "MUSIC_U=test" {
			t.Errorf("cookie = %q, want MUSIC_U=test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"playlist":[{"id":11,"name":"我喜欢的音乐","coverImgUrl":"https://cover/liked","trackCount":7},{"id":22,"name":"通勤","coverImgUrl":"https://cover/commute","trackCount":19}]}`))
	}))
	defer server.Close()

	p := accountTestProvider(t, server, "9988")
	got, err := p.AccountPlaylists(context.Background())
	if err != nil {
		t.Fatalf("AccountPlaylists() error = %v", err)
	}
	want := []provider.AccountPlaylist{
		{ID: "11", Name: "我喜欢的音乐", CoverURL: "https://cover/liked", TrackCount: 7},
		{ID: "22", Name: "通勤", CoverURL: "https://cover/commute", TrackCount: 19},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountPlaylists() = %#v, want %#v", got, want)
	}
}

func TestAccountPlaylistsMissingAccountUIDDoesNotRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	p := accountTestProvider(t, server, "")
	if _, err := p.AccountPlaylists(context.Background()); err == nil {
		t.Fatal("AccountPlaylists() error = nil, want missing account uid error")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func accountTestProvider(t *testing.T, server *httptest.Server, uid string) *Provider {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ncm.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=test", "ok"); err != nil {
		t.Fatal(err)
	}
	if uid != "" {
		if err := st.SetCredentialAccount(ctx, "ncm", store.AccountProfile{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	p := &Provider{base: server.URL, st: st, client: server.Client(), writeClient: server.Client()}
	p.cookie.Store("MUSIC_U=test")
	return p
}

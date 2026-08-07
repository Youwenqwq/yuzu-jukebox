package qq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// qqCred 构造已登录凭据（跳过校验）。
func qqCred() *qqCredential {
	c, err := parseCredential(testCredJSON)
	if err != nil {
		panic(err)
	}
	return c
}

func TestLikeUsesLikedDirAndCookie(t *testing.T) {
	var addCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/song/" + testMid + "/detail":
			_, _ = w.Write([]byte(envelope(map[string]any{"track": songFixture(testMid, testName, 101, 267, "周杰伦")})))
		case "/songlist/add_songs":
			addCalls++
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			q := r.URL.Query()
			if got := q.Get("dirid"); got != "201" {
				t.Errorf("dirid = %q, want 201", got)
			}
			if got := q.Get("tid"); got != "0" {
				t.Errorf("tid = %q, want 0", got)
			}
			if got := q.Get("song_id"); got != "101" {
				t.Errorf("song_id = %q, want 101 (numeric id from detail)", got)
			}
			if got := q.Get("song_type"); got != "1" {
				t.Errorf("song_type = %q, want 1", got)
			}
			if cookie := r.Header.Get("Cookie"); !strings.Contains(cookie, "musickey=MK_TEST") {
				t.Errorf("Cookie = %q, want musickey", cookie)
			}
			_, _ = w.Write([]byte(envelope(nil)))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)
	p.cred.Store(qqCred())

	if err := p.Like(context.Background(), testMid); err != nil {
		t.Fatalf("Like() error = %v", err)
	}
	if addCalls != 1 {
		t.Fatalf("add_songs calls = %d, want 1", addCalls)
	}
}

func TestLikeWithoutCredentialFails(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()
	p := testProvider(t, server)

	if err := p.Like(context.Background(), testMid); err == nil {
		t.Fatal("Like() error = nil, want credential error")
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestAddToPlaylistCompositeRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/song/" + testMid + "/detail":
			_, _ = w.Write([]byte(envelope(map[string]any{"track": songFixture(testMid, testName, 101, 267, "周杰伦")})))
		case "/songlist/add_songs":
			q := r.URL.Query()
			if got := q.Get("tid"); got != "88" {
				t.Errorf("tid = %q, want 88", got)
			}
			if got := q.Get("dirid"); got != "44" {
				t.Errorf("dirid = %q, want 44", got)
			}
			_, _ = w.Write([]byte(envelope(nil)))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)
	p.cred.Store(qqCred())

	if err := p.AddToPlaylist(context.Background(), "88:44", testMid); err != nil {
		t.Fatalf("AddToPlaylist() error = %v", err)
	}
}

func TestAddToPlaylistBadRef(t *testing.T) {
	p := &Provider{}
	for _, ref := range []string{"", "88", "abc:def"} {
		if err := p.AddToPlaylist(context.Background(), ref, testMid); err == nil {
			t.Errorf("AddToPlaylist(%q) unexpectedly succeeded", ref)
		}
	}
}

func TestLikeCheckPagingAndCompare(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/EAA%3D/fav/songs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if cookie := r.Header.Get("Cookie"); !strings.Contains(cookie, "musickey=MK_TEST") {
			t.Errorf("Cookie = %q, want musickey", cookie)
		}
		page := r.URL.Query().Get("page")
		if page == "1" {
			_, _ = w.Write([]byte(envelope(map[string]any{"songs": []any{songFixture("other", "x", 1, 1, "a")}, "total": 2, "hasmore": 1})))
			return
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"songs": []any{songFixture(testMid, testName, 101, 267, "周杰伦")}, "total": 2, "hasmore": 0})))
	}))
	defer server.Close()
	p := testProvider(t, server)
	p.cred.Store(qqCred())

	got, err := p.LikeCheck(context.Background(), testMid)
	if err != nil {
		t.Fatalf("LikeCheck() error = %v", err)
	}
	if !got {
		t.Fatal("LikeCheck() = false, want true (found on page 2)")
	}
}

func TestLikeCheckMissingEncryptUin(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()
	p := testProvider(t, server)
	cred := qqCred()
	cred.EncryptUin = ""
	p.cred.Store(cred)

	if _, err := p.LikeCheck(context.Background(), testMid); err == nil {
		t.Fatal("LikeCheck() error = nil, want account error")
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestAccountPlaylistsMergeCreatedAndFav(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/12345/created_songlists":
			_, _ = w.Write([]byte(envelope(map[string]any{"total": 1, "playlists": []any{
				map[string]any{"id": 11, "dirid": 1, "title": "我的创建", "picurl": "https://cover/c1", "songnum": 7},
			}, "deleted_ids": []any{}, "finished": true})))
		case "/user/EAA%3D/fav/songlists":
			_, _ = w.Write([]byte(envelope(map[string]any{"number": 2, "total": 2, "hasmore": 0, "hide": false, "playlists": []any{
				map[string]any{"id": 11, "dirid": 1, "title": "我的创建", "picurl": "https://cover/c1", "songnum": 7},
				map[string]any{"id": 22, "dirid": 2, "title": "收藏的歌单", "picurl": "https://cover/c2", "songnum": 19},
			}, "deleted_ids": []any{}, "failed_ids": []any{}})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)
	p.cred.Store(qqCred())

	got, err := p.AccountPlaylists(context.Background())
	if err != nil {
		t.Fatalf("AccountPlaylists() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("AccountPlaylists() = %d, want 2 (deduped)", len(got))
	}
	if got[0].ID != "11:1" || got[0].Name != "我的创建" || got[0].TrackCount != 7 {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].ID != "22:2" || got[1].CoverURL != "https://cover/c2" {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestPlaylistRefRoundTrip(t *testing.T) {
	tid, dirid, err := parsePlaylistRef("88:44")
	if err != nil || tid != 88 || dirid != 44 {
		t.Fatalf("parsePlaylistRef = %d:%d err=%v", tid, dirid, err)
	}
	if got := playlistRef(88, 44); got != "88:44" {
		t.Fatalf("playlistRef = %q, want 88:44", got)
	}
}

// 接口断言：账号写操作整组实现。
func TestAccountWriterInterface(t *testing.T) {
	p := &Provider{}
	var _ provider.AccountWriter = p
}

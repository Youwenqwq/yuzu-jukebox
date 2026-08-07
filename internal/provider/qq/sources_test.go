package qq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func TestImportPlaylistPagingAndDedup(t *testing.T) {
	var pageCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/songlist/777/detail" {
			t.Errorf("path = %q", r.URL.Path)
		}
		pageCalls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		info := map[string]any{"id": 777, "title": "我的歌单", "picurl": "https://cover/pl", "songnum": 3}
		var songs []any
		switch page {
		case 1:
			// 重复 mid 只算一次
			songs = []any{songFixture("mid-1", "一", 1, 100, "a"), songFixture("mid-1", "一", 1, 100, "a"), songFixture("mid-2", "二", 2, 200, "b")}
		case 2:
			songs = []any{songFixture("mid-3", "三", 3, 300, "c")}
		default:
			t.Errorf("unexpected page %d", page)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"code": 0, "subcode": 0, "msg": "", "info": info, "size": len(songs), "songs": songs, "total": 3, "hasmore": page})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	name, cover, tracks, err := p.ImportPlaylist(context.Background(), "https://y.qq.com/n/ryqq/playlist/777")
	if err != nil {
		t.Fatalf("ImportPlaylist() error = %v", err)
	}
	if name != "我的歌单" || cover != "https://cover/pl" {
		t.Fatalf("name=%q cover=%q", name, cover)
	}
	if len(tracks) != 3 {
		t.Fatalf("tracks = %d, want 3 (deduped)", len(tracks))
	}
	if pageCalls != 2 {
		t.Fatalf("page calls = %d, want 2", pageCalls)
	}
	if tracks[0].Ref.String() != "qq:mid-1" || tracks[2].Ref.String() != "qq:mid-3" {
		t.Fatalf("track refs = %v, %v", tracks[0].Ref, tracks[2].Ref)
	}
}

func TestImportPlaylistBadID(t *testing.T) {
	p := &Provider{}
	if _, _, _, err := p.ImportPlaylist(context.Background(), "no-digits"); err == nil {
		t.Fatal("ImportPlaylist() error = nil, want id parse error")
	}
}

func TestRadioSources(t *testing.T) {
	p := &Provider{}
	want := []provider.RadioSource{
		{Spec: "newsong", Name: "QQ 新歌推荐", Finite: true},
		{Spec: "top", Arg: "top_id", Name: "QQ 排行榜", Finite: true},
	}
	if got := p.RadioSources(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RadioSources() = %+v, want %+v", got, want)
	}
}

func TestNewSourceValidation(t *testing.T) {
	p := &Provider{}
	for _, spec := range []string{"", "unknown", "top:", "playlist:123"} {
		if _, err := p.NewSource(context.Background(), spec); err == nil {
			t.Errorf("NewSource(%q) unexpectedly succeeded", spec)
		}
	}
	if _, err := p.NewSource(context.Background(), "newsong"); err != nil {
		t.Errorf("NewSource(newsong) error = %v", err)
	}
	if _, err := p.NewSource(context.Background(), "top:62"); err != nil {
		t.Errorf("NewSource(top:62) error = %v", err)
	}
}

func TestTopSourceBatchAndExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/top/62/detail" {
			t.Errorf("path = %q", r.URL.Path)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var songs []any
		switch page {
		case 1:
			songs = []any{songFixture("t-1", "一", 1, 100, "a"), songFixture("t-2", "二", 2, 100, "a"), songFixture("t-3", "三", 3, 100, "a")}
		case 2:
			songs = []any{songFixture("t-4", "四", 4, 100, "a")}
		default:
			t.Errorf("unexpected page %d", page)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{
			"info": map[string]any{"id": 62, "name": "热歌榜", "total_num": 4}, "songs": songs, "song_tags": []any{},
		})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	src, err := p.NewSource(context.Background(), "top:62")
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if !src.Finite() || src.Description() == "" {
		t.Fatalf("source = %+v", src)
	}
	batch1, exhausted, err := src.NextBatch(context.Background(), 3, "")
	if err != nil {
		t.Fatalf("NextBatch 1 error = %v", err)
	}
	if len(batch1) != 3 || exhausted {
		t.Fatalf("batch1 = %d exhausted=%v, want 3/false", len(batch1), exhausted)
	}
	batch2, exhausted2, err := src.NextBatch(context.Background(), 3, "")
	if err != nil {
		t.Fatalf("NextBatch 2 error = %v", err)
	}
	if len(batch2) != 1 || !exhausted2 {
		t.Fatalf("batch2 = %d exhausted=%v, want 1/true", len(batch2), exhausted2)
	}
	if batch2[0].Ref.String() != "qq:t-4" {
		t.Fatalf("batch2 ref = %s", batch2[0].Ref)
	}
}

func TestNewsongSourceMaterialization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recommend/get_recommend_newsong" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"songs": []any{
			songFixture("n-1", "新一", 1, 100, "a"), songFixture("n-2", "新二", 2, 100, "a"),
		}})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	src, err := p.NewSource(context.Background(), "newsong")
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	batch, exhausted, err := src.NextBatch(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("NextBatch 1 error = %v", err)
	}
	if len(batch) != 1 || exhausted {
		t.Fatalf("batch1 = %d exhausted=%v, want 1/false", len(batch), exhausted)
	}
	if batch[0].Title != "新一" {
		t.Fatalf("batch1[0].Title = %q, want 新一", batch[0].Title)
	}
	batch2, exhausted2, err := src.NextBatch(context.Background(), 5, "")
	if err != nil {
		t.Fatalf("NextBatch 2 error = %v", err)
	}
	if len(batch2) != 1 || !exhausted2 {
		t.Fatalf("batch2 = %d exhausted=%v, want 1/true", len(batch2), exhausted2)
	}
	// 耗尽后再取返回空
	batch3, exhausted3, _ := src.NextBatch(context.Background(), 5, "")
	if len(batch3) != 0 || !exhausted3 {
		t.Fatalf("batch3 = %d exhausted=%v, want 0/true", len(batch3), exhausted3)
	}
}

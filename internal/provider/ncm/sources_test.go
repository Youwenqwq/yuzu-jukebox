package ncm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func TestRadioSources(t *testing.T) {
	p := &Provider{}
	got := p.RadioSources()
	tests := []struct {
		name   string
		want   provider.RadioSource
		source provider.TrackSource
	}{
		{
			name:   "daily",
			want:   provider.RadioSource{Spec: "daily", Name: "每日推荐", Finite: true, RequiresCredential: true},
			source: &dailySource{p: p},
		},
		{
			name:   "newsong",
			want:   provider.RadioSource{Spec: "newsong", Name: "推荐新歌", Finite: true},
			source: &listSource{},
		},
		{
			name:   "fm",
			want:   provider.RadioSource{Spec: "fm", Name: "私人 FM", Finite: false, RequiresCredential: true},
			source: &fmSource{p: p},
		},
		{
			name:   "simi",
			want:   provider.RadioSource{Spec: "simi", Arg: "track_id", Name: "相似歌曲", Finite: false, RequiresCredential: true},
			source: &chainedSource{p: p, kind: "simi"},
		},
		{
			name:   "heart",
			want:   provider.RadioSource{Spec: "heart", Arg: "track_id", Name: "心动模式", Finite: false, RequiresCredential: true},
			source: &chainedSource{p: p, kind: "heart"},
		},
		{
			name:   "playlist",
			want:   provider.RadioSource{Spec: "playlist", Arg: "playlist_id", Name: "歌单电台", Finite: true},
			source: &listSource{},
		},
	}
	if len(got) != len(tests) {
		t.Fatalf("RadioSources() returned %d entries, want %d: %#v", len(got), len(tests), got)
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(got[i], tt.want) {
				t.Fatalf("RadioSources()[%d] = %#v, want %#v", i, got[i], tt.want)
			}
			if got[i].Finite != tt.source.Finite() {
				t.Fatalf("catalog Finite = %v, source Finite() = %v", got[i].Finite, tt.source.Finite())
			}
		})
	}
}

func TestSimilarQueriesOnceAndLimits(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/simi/song" {
			t.Errorf("path = %q, want /simi/song", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "347230" {
			t.Errorf("id = %q, want 347230", got)
		}
		if got := r.URL.Query().Get("cookie"); got != "MUSIC_U=test" {
			t.Errorf("cookie = %q, want MUSIC_U=test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"songs":[
			{"id":11,"name":"相似一","duration":111000,"album":{"name":"专辑一","picUrl":"https://cover/11"},"artists":[{"name":"甲"},{"name":"乙"}]},
			{"id":12,"name":"相似二","duration":222000,"album":{"name":"专辑二","picUrl":"https://cover/12"},"artists":[{"name":"丙"}]}
		]}`))
	}))
	defer server.Close()

	p := &Provider{base: server.URL, client: server.Client()}
	p.cookie.Store("MUSIC_U=test")
	tracks, err := p.Similar(context.Background(), "347230", 1)
	if err != nil {
		t.Fatalf("Similar() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if len(tracks) != 1 {
		t.Fatalf("len(tracks) = %d, want 1", len(tracks))
	}
	want := provider.Track{
		Ref:          "ncm:11",
		Title:        "相似一",
		Artist:       "甲/乙",
		DurationMs:   111000,
		Album:        "专辑一",
		CoverURL:     "https://cover/11",
		SourceURL:    "https://music.163.com/song?id=11",
		Contributors: []provider.Contributor{{Role: "artist", Name: "甲"}, {Role: "artist", Name: "乙"}},
	}
	if !reflect.DeepEqual(tracks[0], want) {
		t.Fatalf("track = %#v, want %#v", tracks[0], want)
	}

	p.cookie.Store("")
	if _, err := p.Similar(context.Background(), "347230", 1); err == nil || !strings.Contains(err.Error(), "requires login") {
		t.Fatalf("Similar() without credential error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("missing credential called upstream; requests = %d", requests)
	}
}

func TestPlaylistSourceMaterializesAndDrains(t *testing.T) {
	var detailRequests, trackRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/playlist/detail":
			detailRequests++
			if got := r.URL.Query().Get("id"); got != "123" {
				t.Errorf("detail id = %q, want 123", got)
			}
			_, _ = w.Write([]byte(`{"playlist":{"name":"顺序歌单"}}`))
		case "/playlist/track/all":
			trackRequests++
			if got := r.URL.Query().Get("id"); got != "123" {
				t.Errorf("track id = %q, want 123", got)
			}
			if got := r.URL.Query().Get("limit"); got != "1000" {
				t.Errorf("limit = %q, want 1000", got)
			}
			if got := r.URL.Query().Get("offset"); got != "0" {
				t.Errorf("offset = %q, want 0", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"songs": []map[string]any{
					{"id": 1, "name": "一", "dt": 1000, "al": map[string]any{"name": "专辑一", "picUrl": "https://cover/1"}, "ar": []map[string]any{{"name": "甲"}}},
					{"id": 2, "name": "二", "dt": 2000, "al": map[string]any{"name": "专辑二", "picUrl": "https://cover/2"}, "ar": []map[string]any{{"name": "乙"}}},
					{"id": 3, "name": "三", "dt": 3000, "al": map[string]any{"name": "专辑三", "picUrl": "https://cover/3"}, "ar": []map[string]any{{"name": "丙"}}},
				},
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p := &Provider{base: server.URL, client: server.Client()}
	p.cookie.Store("")
	src, err := p.NewSource(context.Background(), "playlist:123")
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if !src.Finite() {
		t.Fatal("Finite() = false, want true")
	}
	if got := src.Description(); got != "网易云歌单《顺序歌单》" {
		t.Fatalf("Description() = %q", got)
	}
	if detailRequests != 1 || trackRequests != 1 {
		t.Fatalf("materialization requests = detail:%d tracks:%d, want 1 each", detailRequests, trackRequests)
	}

	first, exhausted, err := src.NextBatch(context.Background(), 2, "")
	if err != nil {
		t.Fatalf("first NextBatch() error = %v", err)
	}
	if exhausted {
		t.Fatal("first NextBatch() exhausted = true, want false")
	}
	if got, want := trackRefs(first), []provider.TrackRef{"ncm:1", "ncm:2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first refs = %v, want %v", got, want)
	}

	second, exhausted, err := src.NextBatch(context.Background(), 2, "")
	if err != nil {
		t.Fatalf("second NextBatch() error = %v", err)
	}
	if !exhausted {
		t.Fatal("second NextBatch() exhausted = false, want true")
	}
	if got, want := trackRefs(second), []provider.TrackRef{"ncm:3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second refs = %v, want %v", got, want)
	}

	last, exhausted, err := src.NextBatch(context.Background(), 2, "")
	if err != nil {
		t.Fatalf("last NextBatch() error = %v", err)
	}
	if len(last) != 0 || !exhausted {
		t.Fatalf("last NextBatch() = (%v, %v), want empty/true", last, exhausted)
	}
	if detailRequests != 1 || trackRequests != 1 {
		t.Fatalf("source refetched: detail:%d tracks:%d", detailRequests, trackRequests)
	}
}

// TestNewsongSourceMaterializesAndDrains 推荐新歌源匿名物化 /personalized/newsong
// （song 内联完整曲目），游标式耗尽。
func TestNewsongSourceMaterializesAndDrains(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/personalized/newsong" {
			t.Errorf("path = %q, want /personalized/newsong", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "30" {
			t.Errorf("limit = %q, want 30", got)
		}
		if got := r.URL.Query().Get("cookie"); got != "" {
			t.Errorf("anonymous newsong sent cookie %q", got)
		}
		_, _ = w.Write([]byte(`{"code":200,"result":[
			{"song":{"id":1,"name":"一","duration":1000,"album":{"name":"专辑一","picUrl":"https://cover/1"},"artists":[{"name":"甲"}]}},
			{"song":{"id":2,"name":"二","dt":2000,"al":{"name":"专辑二","picUrl":"https://cover/2"},"ar":[{"name":"乙"}]}}
		]}`))
	}))
	defer server.Close()

	p := &Provider{base: server.URL, client: server.Client()}
	p.cookie.Store("")
	src, err := p.NewSource(context.Background(), "newsong")
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if !src.Finite() {
		t.Fatal("Finite() = false, want true")
	}
	if got := src.Description(); got != "网易云推荐新歌" {
		t.Fatalf("Description() = %q", got)
	}

	first, exhausted, err := src.NextBatch(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("first NextBatch() error = %v", err)
	}
	if exhausted || len(first) != 1 || first[0].Ref != "ncm:1" ||
		first[0].Title != "一" || first[0].Artist != "甲" || first[0].Album != "专辑一" {
		t.Fatalf("first batch = %#v, want ncm:1 with rich fields", first)
	}

	second, exhausted, err := src.NextBatch(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("second NextBatch() error = %v", err)
	}
	if !exhausted || len(second) != 1 || second[0].Ref != "ncm:2" {
		t.Fatalf("second batch = %#v, want ncm:2 and exhausted", second)
	}

	last, exhausted, err := src.NextBatch(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("last NextBatch() error = %v", err)
	}
	if len(last) != 0 || !exhausted {
		t.Fatalf("last NextBatch() = (%v, %v), want empty/true", last, exhausted)
	}
	if requests != 1 {
		t.Fatalf("materialization requests = %d, want 1", requests)
	}
}

func TestNewSourceUnknownMentionsPlaylist(t *testing.T) {
	p := &Provider{}
	_, err := p.NewSource(context.Background(), "unknown")
	if err == nil {
		t.Fatal("NewSource() error = nil")
	}
	if !strings.Contains(err.Error(), "playlist:<id>") {
		t.Fatalf("NewSource() error = %q, want playlist:<id>", err)
	}
	if _, err := p.NewSource(context.Background(), "playlist:"); err == nil {
		t.Fatal("NewSource(playlist:) error = nil")
	}
}

func trackRefs(tracks []provider.Track) []provider.TrackRef {
	refs := make([]provider.TrackRef, len(tracks))
	for i := range tracks {
		refs[i] = tracks[i].Ref
	}
	return refs
}

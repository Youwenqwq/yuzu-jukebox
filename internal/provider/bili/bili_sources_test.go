package bili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func TestNewSourceFavorite(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/fav/resource/list" || r.URL.Query().Get("media_id") != "123" || r.URL.Query().Get("ps") != "20" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get(cookieHeader); got != "SESSDATA=radio" {
			t.Errorf("cookie header = %q", got)
		}
		pn, err := strconv.Atoi(r.URL.Query().Get("pn"))
		if err != nil || (pn != 1 && pn != 2) {
			t.Errorf("page = %q", r.URL.Query().Get("pn"))
		}
		_ = json.NewEncoder(w).Encode(videoListResponse{
			Results: makeVideos(fmt.Sprintf("p%d-", pn), 20, -1),
			Total:   25,
		})
	}))
	defer server.Close()

	source, err := testProvider(server, "SESSDATA=radio").NewSource(context.Background(), "fav:123")
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if !source.Finite() {
		t.Fatal("Finite() = false, want true")
	}
	if got := source.Description(); !strings.Contains(got, "123") {
		t.Errorf("Description() = %q, want media_id", got)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}

	first, exhausted, err := source.NextBatch(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("first NextBatch: %v", err)
	}
	if exhausted || len(first) != 10 {
		t.Fatalf("first batch len = %d, exhausted = %v; want 10, false", len(first), exhausted)
	}
	assertVideoTrack(t, first[0], "p1-0")
	assertVideoTrack(t, first[9], "p1-9")

	last, exhausted, err := source.NextBatch(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("last NextBatch: %v", err)
	}
	if !exhausted || len(last) != 15 {
		t.Fatalf("last batch len = %d, exhausted = %v; want 15, true", len(last), exhausted)
	}
	assertVideoTrack(t, last[0], "p1-10")
	assertVideoTrack(t, last[len(last)-1], "p2-4")

	drained, exhausted, err := source.NextBatch(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("drained NextBatch: %v", err)
	}
	if !exhausted || len(drained) != 0 {
		t.Fatalf("drained batch len = %d, exhausted = %v; want 0, true", len(drained), exhausted)
	}
}

func TestNewSourceFavoriteValidation(t *testing.T) {
	t.Run("bad media id", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		p := testProvider(server, "cookie")
		for _, spec := range []string{"fav:", "fav:abc", "fav:1:2", "fav:18446744073709551616"} {
			if _, err := p.NewSource(context.Background(), spec); err == nil {
				t.Errorf("NewSource(%q) unexpectedly succeeded", spec)
			}
		}
	})

	t.Run("empty cookie fails before request", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := testProvider(server, "").NewSource(context.Background(), "fav:123")
		if err == nil || !strings.Contains(err.Error(), "cookie") {
			t.Fatalf("NewSource error = %v, want cookie error", err)
		}
		if requests != 0 {
			t.Fatalf("requests = %d, want 0", requests)
		}
	})

	t.Run("unknown spec", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		_, err := testProvider(server, "cookie").NewSource(context.Background(), "daily")
		if err == nil || !strings.Contains(err.Error(), "fav:<media_id>") {
			t.Fatalf("NewSource error = %v, want fav:<media_id> hint", err)
		}
	})
}

func TestRadioSources(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	got := testProvider(server, "").RadioSources()
	want := []provider.RadioSource{
		{Spec: "ranking", Arg: "music", Name: "音乐区排行榜", Finite: true},
		{Spec: "ranking", Arg: "kichiku", Name: "鬼畜区排行榜", Finite: true},
		{Spec: "ranking", Arg: "all", Name: "全站排行榜", Finite: true},
		{Spec: "fav", Arg: "media_id", Name: "收藏夹电台", Finite: true, RequiresCredential: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RadioSources() = %+v, want %+v", got, want)
	}
}

// TestNewSourceRanking 分区排行榜物化：key 走 partition 参数、数字走 rid 参数。
func TestNewSourceRanking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/region-ranking" {
			t.Errorf("path = %q, want /region-ranking", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("type"); got != "all" {
			t.Errorf("type = %q, want all", got)
		}
		if got := r.Header.Get(cookieHeader); got != "" {
			t.Errorf("anonymous ranking sent cookie %q", got)
		}
		var partition string
		if got := q.Get("partition"); got != "" {
			partition = "partition=" + got
		}
		if got := q.Get("rid"); got != "" {
			partition = "rid=" + got
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": makeVideos(partition+"-", 3, -1),
			"total":   3,
		})
	}))
	defer server.Close()
	p := testProvider(server, "")

	byKey, err := p.NewSource(context.Background(), "ranking:music")
	if err != nil {
		t.Fatalf("NewSource(ranking:music): %v", err)
	}
	if !byKey.Finite() {
		t.Fatal("Finite() = false, want true")
	}
	if got := byKey.Description(); !strings.Contains(got, "music") {
		t.Errorf("Description() = %q, want partition key", got)
	}
	batch, exhausted, err := byKey.NextBatch(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("NextBatch: %v", err)
	}
	if !exhausted || len(batch) != 3 {
		t.Fatalf("batch len = %d, exhausted = %v; want 3, true (3 条全返回即耗尽)", len(batch), exhausted)
	}
	assertVideoTrack(t, batch[0], "partition=music-0")

	byRid, err := p.NewSource(context.Background(), "ranking:1003")
	if err != nil {
		t.Fatalf("NewSource(ranking:1003): %v", err)
	}
	batch, _, err = byRid.NextBatch(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("NextBatch rid: %v", err)
	}
	assertVideoTrack(t, batch[0], "rid=1003-0")

	if _, err := p.NewSource(context.Background(), "ranking:"); err == nil {
		t.Fatal("NewSource(ranking:) unexpectedly succeeded")
	}
}

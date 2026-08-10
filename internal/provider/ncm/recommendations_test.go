package ncm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestRecommendations 榜单 feed：/toplist 前 3 榜 → /playlist/track/all，
// 每榜曲目封顶 10；单个榜单曲目拉取失败时跳过该 shelf 保留其余。
func TestRecommendations(t *testing.T) {
	var trackCalls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cookie"); got != "" {
			t.Errorf("anonymous toplist cookie = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/toplist":
			_, _ = w.Write([]byte(`{"code":200,"list":[
				{"id":11,"name":"飙升榜"},
				{"id":12,"name":"新歌榜"},
				{"id":13,"name":"热歌榜"},
				{"id":14,"name":"第四榜"}
			]}`))
		case "/playlist/track/all":
			id := r.URL.Query().Get("id")
			trackCalls = append(trackCalls, id)
			if got := r.URL.Query().Get("limit"); got != "10" {
				t.Errorf("limit = %q, want 10", got)
			}
			switch id {
			case "11":
				// 12 首 → 截断为 10
				var sb strings.Builder
				sb.WriteString(`{"code":200,"songs":[`)
				for i := 1; i <= 12; i++ {
					if i > 1 {
						sb.WriteString(",")
					}
					sb.WriteString(`{"id":` + itoa(i) + `,"name":"Song ` + itoa(i) + `","dt":1000,"al":{"name":"Album","picUrl":"https://cover/` + itoa(i) + `"},"ar":[{"name":"Artist"}]}`)
				}
				sb.WriteString(`]}`)
				_, _ = w.Write([]byte(sb.String()))
			case "12":
				_, _ = w.Write([]byte(`{"code":200,"songs":[{"id":101,"name":"New Song","duration":2000,"al":{"name":"New Album","picUrl":"https://cover/101"},"ar":[{"name":"New Artist"}]}]}`))
			case "13":
				// 曲目拉取失败 → 该 shelf 被跳过
				w.WriteHeader(http.StatusInternalServerError)
			case "14":
				t.Errorf("fourth toplist must not be fetched (capped at 3)")
				w.WriteHeader(http.StatusInternalServerError)
			default:
				t.Errorf("unexpected toplist id %q", id)
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	shelves, err := p.Recommendations(context.Background())
	if err != nil {
		t.Fatalf("Recommendations error = %v", err)
	}
	if len(shelves) != 2 {
		t.Fatalf("shelves = %#v, want 2 (13 曲目拉取失败被跳过)", shelves)
	}
	if shelves[0].ID != "toplist:11" || shelves[0].Title != "飙升榜" || len(shelves[0].Tracks) != 10 {
		t.Fatalf("shelf 0 = %#v, want toplist:11 with 10 capped tracks", shelves[0])
	}
	if shelves[1].ID != "toplist:12" || shelves[1].Title != "新歌榜" || len(shelves[1].Tracks) != 1 {
		t.Fatalf("shelf 1 = %#v, want toplist:12 with 1 track", shelves[1])
	}
	first := shelves[0].Tracks[0]
	if first.Ref.String() != "ncm:1" || first.Artist != "Artist" ||
		first.Album != "Album" || first.CoverURL != "https://cover/1" {
		t.Fatalf("shelf track = %#v, want rich ncm:1", first)
	}
	if strings.Join(trackCalls, ",") != "11,12,13" {
		t.Fatalf("track calls = %v, want 11,12,13", trackCalls)
	}
}

// TestRecommendationsEmptyToplist 榜单列表为空 → 空 shelves（不报错）。
func TestRecommendationsEmptyToplist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"list":[]}`))
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	shelves, err := p.Recommendations(context.Background())
	if err != nil {
		t.Fatalf("Recommendations error = %v", err)
	}
	if len(shelves) != 0 {
		t.Fatalf("shelves = %#v, want empty", shelves)
	}
}

// TestRecommendationsAllTracksFail 全部曲目拉取失败 → 整体报错。
func TestRecommendationsAllTracksFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/toplist":
			_, _ = w.Write([]byte(`{"code":200,"list":[{"id":11,"name":"飙升榜"}]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	if _, err := p.Recommendations(context.Background()); err == nil {
		t.Fatal("Recommendations = nil error, want error when all shelves fail")
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/ncm"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestNCMToplistCatalogMaterializesThroughRadioAPI(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/toplist":
			_, _ = w.Write([]byte(`{"code":200,"list":[{"id":123,"name":"飙升榜","coverImgUrl":"https://cover/top.jpg","updateFrequency":"每天更新"}]}`))
		case "/playlist/detail":
			if got := r.URL.Query().Get("id"); got != "123" {
				t.Errorf("playlist detail id = %q, want 123", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"playlist":{"name":"飙升榜","coverImgUrl":"https://cover/top.jpg"}}`))
		case "/playlist/track/all":
			if got := r.URL.Query().Get("id"); got != "123" {
				t.Errorf("playlist tracks id = %q, want 123", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"songs":[{"id":9,"name":"Top Song","dt":180000,"al":{"name":"Top Album","picUrl":"https://cover/song.jpg"},"ar":[{"name":"Artist"}]}]}`))
		default:
			t.Errorf("unexpected sidecar path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer sidecar.Close()

	providerStore, err := store.Open(filepath.Join(t.TempDir(), "ncm.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer providerStore.Close()
	f := setupRadioCatalogEndpoint(t, ncm.New(sidecar.URL, "", providerStore))

	catalog := radioCatalogEndpointRequest(t, f, f.token)
	if catalog.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body = %s", catalog.Code, catalog.Body.String())
	}
	var catalogBody struct {
		Entries []radioSourceCatalogEntry `json:"entries"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &catalogBody); err != nil {
		t.Fatal(err)
	}
	var chart *radioSourceCatalogEntry
	for i := range catalogBody.Entries {
		if catalogBody.Entries[i].Spec == "ncm:toplist:123" {
			chart = &catalogBody.Entries[i]
			break
		}
	}
	if chart == nil {
		t.Fatalf("catalog entries = %#v, want ncm:toplist:123", catalogBody.Entries)
	}
	if chart.Name != "飙升榜" || chart.Detail != "每天更新" || !chart.Finite || !strings.HasPrefix(chart.CoverURL, "/api/v1/cover/ext/") {
		t.Fatalf("chart entry = %#v", *chart)
	}

	path := "/api/v1/radio/tracks?source=" + url.QueryEscape(chart.Spec) + "&limit=10"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	tracks := httptest.NewRecorder()
	f.handler.ServeHTTP(tracks, req)
	if tracks.Code != http.StatusOK {
		t.Fatalf("tracks status = %d, body = %s", tracks.Code, tracks.Body.String())
	}
	var tracksBody struct {
		Tracks []provider.Track `json:"tracks"`
	}
	if err := json.Unmarshal(tracks.Body.Bytes(), &tracksBody); err != nil {
		t.Fatal(err)
	}
	if len(tracksBody.Tracks) != 1 || tracksBody.Tracks[0].Ref != "ncm:9" || tracksBody.Tracks[0].CoverURL != "/api/v1/cover/ncm:9" {
		t.Fatalf("materialized tracks = %#v", tracksBody.Tracks)
	}
}

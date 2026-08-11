package ncm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToplistCatalogAndSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p := &Provider{base: server.URL, client: server.Client()}
	p.cookie.Store("")
	entries, err := p.RadioSourceCatalog(context.Background())
	if err != nil {
		t.Fatalf("RadioSourceCatalog() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("RadioSourceCatalog() = %#v, want one entry", entries)
	}
	entry := entries[0]
	if entry.Spec != "toplist:123" || entry.Name != "飙升榜" || entry.CoverURL != "https://cover/top.jpg" || entry.Detail != "每天更新" {
		t.Fatalf("catalog entry = %#v", entry)
	}

	source, err := p.NewSource(context.Background(), entry.Spec)
	if err != nil {
		t.Fatalf("NewSource(%q) error = %v", entry.Spec, err)
	}
	tracks, exhausted, err := source.NextBatch(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("NextBatch() error = %v", err)
	}
	if !exhausted || len(tracks) != 1 {
		t.Fatalf("NextBatch() = (%#v, %v), want one exhausted track", tracks, exhausted)
	}
	if tracks[0].Ref != "ncm:9" || tracks[0].Title != "Top Song" || tracks[0].CoverURL != "https://cover/song.jpg" {
		t.Fatalf("materialized track = %#v", tracks[0])
	}
}

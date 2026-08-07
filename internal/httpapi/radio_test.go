package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestRadioTracksEndpoint(t *testing.T) {
	f := setupProviderEndpoints(t)

	t.Run("materializes with offset and known total", func(t *testing.T) {
		rec := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=radio%3Afinite&limit=2&offset=2", f.ownerToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tracks []provider.Track `json:"tracks"`
			Total  *int             `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Total == nil || *body.Total != 5 {
			t.Fatalf("total = %#v, want 5", body.Total)
		}
		if len(body.Tracks) != 2 {
			t.Fatalf("len(tracks) = %d, want 2", len(body.Tracks))
		}
		refs := []provider.TrackRef{body.Tracks[0].Ref, body.Tracks[1].Ref}
		if !reflect.DeepEqual(refs, []provider.TrackRef{"radio:3", "radio:4"}) {
			t.Fatalf("refs = %v", refs)
		}
		for _, track := range body.Tracks {
			if !strings.HasPrefix(track.CoverURL, "/api/v1/cover/") {
				t.Fatalf("cover_url = %q, want proxy path", track.CoverURL)
			}
		}
		if f.radio.lastSource == nil || f.radio.lastSource.calls != 2 {
			t.Fatalf("NextBatch calls = %#v, want 2", f.radio.lastSource)
		}
	})

	t.Run("unknown total is null before exhaustion", func(t *testing.T) {
		rec := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=radio%3Apartial&limit=2", f.ownerToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tracks []provider.Track `json:"tracks"`
			Total  *int             `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Total != nil || len(body.Tracks) != 2 {
			t.Fatalf("response = %#v, want two tracks and null total", body)
		}
	})

	t.Run("materializes generic playlist with total", func(t *testing.T) {
		ctx := context.Background()
		if err := f.st.CreatePlaylist(ctx, store.Playlist{
			ID: "radio-list", Name: "列表", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.AppendPlaylistItems(ctx, "radio-list", []store.PlaylistItem{
			{TrackRef: "radio:10", Title: "十", Artist: "甲", DurationMs: 10000},
			{TrackRef: "radio:11", Title: "十一", Artist: "乙", DurationMs: 11000},
		}); err != nil {
			t.Fatal(err)
		}
		rec := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=playlist%3Aradio-list&limit=1&offset=1", f.ownerToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tracks []provider.Track `json:"tracks"`
			Total  *int             `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Total == nil || *body.Total != 2 || len(body.Tracks) != 1 || body.Tracks[0].Ref != "radio:11" {
			t.Fatalf("response = %#v", body)
		}
	})

	t.Run("rejects infinite and unknown sources before construction", func(t *testing.T) {
		before := len(f.radio.sourceCalls)
		for _, path := range []string{
			"/api/v1/radio/tracks?source=radio%3Aendless",
			"/api/v1/radio/tracks?source=radio%3Aunknown",
			"/api/v1/radio/tracks?source=missing%3Afinite",
			"/api/v1/radio/tracks",
		} {
			rec := providerEndpointRequest(t, f, http.MethodGet, path, f.ownerToken, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
			}
		}
		if got := len(f.radio.sourceCalls); got != before {
			t.Fatalf("invalid sources invoked factory %d times", got-before)
		}
	})

	t.Run("maps source construction failure to provider error", func(t *testing.T) {
		rec := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=radio%3Abroken", f.ownerToken, nil)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "provider_error") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("uses search pagination and authorization", func(t *testing.T) {
		tooLarge := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=radio%3Afinite&limit=101", f.ownerToken, nil)
		if tooLarge.Code != http.StatusBadRequest {
			t.Fatalf("large limit status = %d, body = %s", tooLarge.Code, tooLarge.Body.String())
		}
		unauthorized := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/radio/tracks?source=radio%3Afinite", "", nil)
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status = %d, body = %s", unauthorized.Code, unauthorized.Body.String())
		}
	})
}

func TestProviderSimilarEndpoint(t *testing.T) {
	f := setupProviderEndpoints(t)

	t.Run("returns proxied tracks and forwards limit", func(t *testing.T) {
		rec := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/providers/radio/similar?track=seed-7&limit=1", f.ownerToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tracks []provider.Track `json:"tracks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tracks) != 1 || body.Tracks[0].Ref != "radio:similar-1" ||
			!strings.HasPrefix(body.Tracks[0].CoverURL, "/api/v1/cover/") {
			t.Fatalf("tracks = %#v", body.Tracks)
		}
		if got := f.radio.similarCalls; !reflect.DeepEqual(got, [][2]any{{"seed-7", 1}}) {
			t.Fatalf("similar calls = %#v", got)
		}
	})

	t.Run("validates provider track and capability", func(t *testing.T) {
		tests := []struct {
			path string
			want int
		}{
			{"/api/v1/providers/missing/similar?track=1", http.StatusNotFound},
			{"/api/v1/providers/basic/similar?track=1", http.StatusNotImplemented},
			{"/api/v1/providers/radio/similar", http.StatusBadRequest},
			{"/api/v1/providers/radio/similar?track=1&limit=101", http.StatusBadRequest},
		}
		for _, tt := range tests {
			rec := providerEndpointRequest(t, f, http.MethodGet, tt.path, f.ownerToken, nil)
			if rec.Code != tt.want {
				t.Errorf("%s status = %d, want %d, body = %s", tt.path, rec.Code, tt.want, rec.Body.String())
			}
		}
	})

	t.Run("maps optional and provider errors", func(t *testing.T) {
		f.radio.similarErr = provider.ErrNotSupported
		notSupported := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/providers/radio/similar?track=1", f.ownerToken, nil)
		if notSupported.Code != http.StatusNotImplemented || !strings.Contains(notSupported.Body.String(), "not_supported") {
			t.Fatalf("not supported status = %d, body = %s", notSupported.Code, notSupported.Body.String())
		}

		f.radio.similarErr = errors.New("similar upstream failed")
		failed := providerEndpointRequest(t, f, http.MethodGet,
			"/api/v1/providers/radio/similar?track=1", f.ownerToken, nil)
		if failed.Code != http.StatusBadGateway || !strings.Contains(failed.Body.String(), "provider_error") {
			t.Fatalf("provider error status = %d, body = %s", failed.Code, failed.Body.String())
		}
	})
}

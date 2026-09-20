package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/distribution"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/local"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestDistributionInternalAPI(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mediaDir := filepath.Join(dir, "media")
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(mediaDir, "song.mp3"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMediaFile(context.Background(), store.MediaFile{
		ID: "song", Filename: "song.mp3", Title: "Song",
		DurationMs: 1000, SizeBytes: int64(len(content)), CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	reg := provider.NewRegistry()
	reg.Register(local.New(mediaDir, st))
	authm := auth.NewManager("", st)
	roomCache := cache.New(cacheDir, 1<<30, 0, st, reg)
	dist := distribution.New(st)
	_, err = st.CreateAcceleration(context.Background(), store.Acceleration{
		ID: "edgeone-main", Name: "EdgeOne", Kind: "edgeone",
		CacheMode: store.CacheModePrefetchAndHeat, PrefetchHorizon: store.DefaultPrefetchHorizon,
		PrefetchSharePercent: store.DefaultPrefetchSharePercent, ControlBaseURL: "https://control.test/yuzu-edge",
		BackendBaseURL: "https://control.test/yuzu-blob", LeaseTTLSeconds: 600,
		UploadRateBytesPerSecond: 187500, MaxObjectBytes: 23 << 20,
	}, distribution.HashCredential("publisher-secret"), distribution.HashCredential("delivery-secret"), "backend-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateAcceleration(context.Background(), "edgeone-main", store.AccelerationUpdate{
		Name: "EdgeOne", Enabled: true, CacheMode: store.CacheModePrefetchAndHeat,
		PrefetchHorizon:      store.DefaultPrefetchHorizon,
		PrefetchSharePercent: store.DefaultPrefetchSharePercent,
		ControlBaseURL:       "https://control.test/yuzu-edge",
		BackendBaseURL:       "https://control.test/yuzu-blob", LeaseTTLSeconds: 600,
		UploadRateBytesPerSecond: 187500, MaxObjectBytes: 23 << 20,
		StorageBudgetBytes: 850 << 20, StorageHighWatermarkPercent: 95,
		StorageLowWatermarkPercent: 85,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{st: st, authm: authm, reg: reg, cache: roomCache}
	server.ConfigureDistribution(dist, distribution.NewRegistry(st))
	handler := server.Handler()

	ref := "local:song"
	ticket := authm.IssueTicket("listener-1", ref)
	introspectBody := map[string]any{"track_ref": ref, "ticket": ticket}
	unauthorized := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/introspect", "wrong", introspectBody)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized introspect = %d", unauthorized.Code)
	}
	introspect := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/introspect", "delivery-secret", introspectBody)
	if introspect.Code != http.StatusOK {
		t.Fatalf("introspect = %d: %s", introspect.Code, introspect.Body.String())
	}
	var initial struct {
		Ready bool `json:"ready"`
	}
	decodeRecorder(t, introspect, &initial)
	if initial.Ready {
		t.Fatal("new distribution request unexpectedly ready")
	}

	claim := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/leases", "publisher-secret",
		map[string]any{"owner": "publisher-1", "lease_seconds": 600})
	if claim.Code != http.StatusCreated {
		t.Fatalf("claim = %d: %s", claim.Code, claim.Body.String())
	}
	var claimed struct {
		Lease struct {
			ID        string `json:"id"`
			TrackRef  string `json:"track_ref"`
			Owner     string `json:"owner"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"lease"`
		SourceURL string `json:"source_url"`
	}
	decodeRecorder(t, claim, &claimed)
	if claimed.Lease.TrackRef != ref || claimed.SourceURL == "" {
		t.Fatalf("claimed = %#v", claimed)
	}
	progress := distributionRequest(t, handler, http.MethodPatch,
		"/internal/v1/accelerations/leases/"+claimed.Lease.ID+"/progress",
		"publisher-secret", map[string]any{
			"owner": "publisher-1", "phase": "uploading",
			"source_bytes": len(content), "upload_bytes": 4,
			"total_bytes": len(content),
		})
	if progress.Code != http.StatusOK {
		t.Fatalf("progress = %d: %s", progress.Code, progress.Body.String())
	}
	var progressed struct {
		Lease struct {
			ExpiresAt int64 `json:"expires_at"`
		} `json:"lease"`
	}
	decodeRecorder(t, progress, &progressed)
	if progressed.Lease.ExpiresAt < claimed.Lease.ExpiresAt {
		t.Fatalf("renewed expiry = %d, original %d", progressed.Lease.ExpiresAt, claimed.Lease.ExpiresAt)
	}

	sourceReq := httptest.NewRequest(http.MethodGet, claimed.SourceURL, nil)
	sourceReq.Header.Set("Authorization", "Bearer publisher-secret")
	sourceReq.Header.Set("Range", "bytes=2-5")
	source := httptest.NewRecorder()
	handler.ServeHTTP(source, sourceReq)
	if source.Code != http.StatusPartialContent || source.Body.String() != "2345" {
		t.Fatalf("source = %d %q", source.Code, source.Body.String())
	}

	reserve := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/leases/"+claimed.Lease.ID+"/reserve",
		"publisher-secret", map[string]any{
			"owner": "publisher-1", "locator": "opaque/blob/object",
			"size_bytes": len(content),
		})
	if reserve.Code != http.StatusOK {
		t.Fatalf("reserve = %d: %s", reserve.Code, reserve.Body.String())
	}

	complete := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/leases/"+claimed.Lease.ID+"/complete",
		"publisher-secret", map[string]any{
			"owner": "publisher-1", "content_version": "sha256-value",
			"locator": "opaque/blob/object", "layout": "object",
			"size_bytes": len(content), "content_type": "audio/mpeg", "etag": "etag-1",
		})
	if complete.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", complete.Code, complete.Body.String())
	}
	ready := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/introspect", "delivery-secret", introspectBody)
	var resolved struct {
		Ready     bool `json:"ready"`
		Candidate struct {
			Locator string `json:"locator"`
		} `json:"candidate"`
	}
	decodeRecorder(t, ready, &resolved)
	if !resolved.Ready || resolved.Candidate.Locator != "opaque/blob/object" {
		t.Fatalf("resolved = %#v", resolved)
	}

	t.Run("oversized existing candidate falls back", func(t *testing.T) {
		if _, err := st.DB().Exec(`UPDATE accelerations SET max_object_bytes = 8 WHERE id = 'edgeone-main'`); err != nil {
			t.Fatal(err)
		}
		response := distributionRequest(t, handler, http.MethodPost,
			"/internal/v1/accelerations/introspect", "delivery-secret", introspectBody)
		var result struct {
			Ready bool `json:"ready"`
		}
		decodeRecorder(t, response, &result)
		if response.Code != http.StatusOK || result.Ready {
			t.Fatalf("oversized candidate remained ready: %s", response.Body.String())
		}
		claim := distributionRequest(t, handler, http.MethodPost, "/internal/v1/accelerations/leases",
			"publisher-secret", map[string]any{"owner": "publisher-1"})
		if claim.Code != http.StatusNoContent {
			t.Fatalf("known oversized source was re-leased: %s", claim.Body.String())
		}
	})
	t.Run("structured failure policy survives introspection", func(t *testing.T) {
		for _, tc := range []struct{ ref, code, state string }{
			{"ncm:policy", "object_too_large", "skipped"},
			{"ncm:ordinary", "publish_failed", "retry_wait"},
		} {
			if err := dist.Request(context.Background(), "edgeone-main", provider.TrackRef(tc.ref)); err != nil {
				t.Fatal(err)
			}
			claim := distributionRequest(t, handler, http.MethodPost, "/internal/v1/accelerations/leases",
				"publisher-secret", map[string]any{"owner": "publisher-1"})
			if claim.Code != http.StatusCreated {
				t.Fatalf("claim: %s", claim.Body.String())
			}
			var work struct {
				Lease distribution.Lease `json:"lease"`
			}
			decodeRecorder(t, claim, &work)
			failed := distributionRequest(t, handler, http.MethodPost,
				"/internal/v1/accelerations/leases/"+work.Lease.ID+"/fail", "publisher-secret",
				map[string]any{"owner": "publisher-1", "error": "object is too large", "error_code": tc.code})
			if failed.Code != http.StatusOK {
				t.Fatalf("failure protocol: %s", failed.Body.String())
			}
			demand := distributionRequest(t, handler, http.MethodPost, "/internal/v1/accelerations/introspect",
				"delivery-secret", map[string]any{"track_ref": tc.ref, "ticket": authm.IssueTicket("listener-1", tc.ref)})
			if demand.Code != http.StatusOK {
				t.Fatalf("repeat introspection: %s", demand.Body.String())
			}
			view, err := st.GetDistributionRequest(context.Background(), "edgeone-main", tc.ref, time.Now().UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			if view.State != tc.state || view.ErrorCode != tc.code {
				t.Fatalf("failure classified from message rather than code: %#v", view)
			}
			claim = distributionRequest(t, handler, http.MethodPost, "/internal/v1/accelerations/leases",
				"publisher-secret", map[string]any{"owner": "publisher-1"})
			if claim.Code != http.StatusNoContent {
				t.Fatalf("repeat demand bypassed failure policy: %s", claim.Body.String())
			}
		}
	})
}

func distributionRequest(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeRecorder(t *testing.T, recorder *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
}

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
	roomCache := cache.New(cacheDir, 1<<30, st, reg)
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

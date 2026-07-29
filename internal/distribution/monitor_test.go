package distribution

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestHealthMonitorRecordsControlAndBackendHealth(t *testing.T) {
	const backendToken = "backend-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/health":
		case "/backend/health":
			if r.Header.Get("Authorization") != "Bearer "+backendToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.CreateAcceleration(context.Background(), store.Acceleration{
		ID: "edgeone-main", Name: "EdgeOne", Kind: "edgeone",
		ControlBaseURL:  server.URL + "/control",
		BackendBaseURL:  server.URL + "/backend",
		LeaseTTLSeconds: 600, UploadRateBytesPerSecond: 187500,
		MaxObjectBytes: 23 << 20,
	}, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), backendToken)
	if err != nil {
		t.Fatal(err)
	}

	monitor := NewHealthMonitor(st)
	monitor.client = server.Client()
	monitor.checkAll(context.Background())

	acceleration, err := st.GetAcceleration(context.Background(), "edgeone-main")
	if err != nil {
		t.Fatal(err)
	}
	if acceleration.ControlHealthy == nil || !*acceleration.ControlHealthy {
		t.Fatalf("control health = %v", acceleration.ControlHealthy)
	}
	if acceleration.BackendHealthy == nil || !*acceleration.BackendHealthy {
		t.Fatalf("backend health = %v", acceleration.BackendHealthy)
	}
	if acceleration.LastHealthAt == nil || *acceleration.LastHealthAt == 0 {
		t.Fatalf("last health at = %v", acceleration.LastHealthAt)
	}
}

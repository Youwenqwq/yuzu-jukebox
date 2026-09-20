package distribution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestHealthMonitorSkipsDisabledAndBacksOffFailures(t *testing.T) {
	var requests atomic.Int64
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"enabled", "disabled"} {
		if _, err := st.CreateAcceleration(ctx, store.Acceleration{ID: id, Name: id, Kind: "edgeone", Enabled: id == "enabled", ControlBaseURL: server.URL, BackendBaseURL: server.URL, LeaseTTLSeconds: 600, MaxObjectBytes: 100}, HashCredential("publisher:"+id), HashCredential("delivery:"+id), "test-token"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE accelerations SET enabled = 1 WHERE id = 'enabled'`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	monitor := NewHealthMonitor(st)
	monitor.now = func() time.Time { return now }
	monitor.client = server.Client()
	monitor.checkAll(ctx)
	if requests.Load() != 2 {
		t.Fatalf("initial probes = %d, disabled acceleration must not be probed", requests.Load())
	}
	for _, delay := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		before := requests.Load()
		now = now.Add(delay - time.Second)
		monitor.checkAll(ctx)
		if requests.Load() != before {
			t.Fatal("failed backend probed before backoff")
		}
		now = now.Add(time.Second)
		monitor.checkAll(ctx)
		if requests.Load() != before+2 {
			t.Fatal("health probe did not resume at bounded deadline")
		}
	}
	healthy.Store(true)
	now = now.Add(time.Hour)
	monitor.checkAll(ctx)
	before := requests.Load()
	now = now.Add(5*time.Minute - time.Second)
	monitor.checkAll(ctx)
	if requests.Load() != before {
		t.Fatal("healthy probe ran faster than five minutes")
	}
	now = now.Add(time.Second)
	monitor.checkAll(ctx)
	if requests.Load() != before+2 {
		t.Fatal("healthy probe did not reset cadence")
	}
}

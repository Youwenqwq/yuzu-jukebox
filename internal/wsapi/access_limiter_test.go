package wsapi

import (
	"fmt"
	"testing"
	"time"
)

func TestAccessProbeLimiterThresholdResetAndCleanup(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	limiter := newAccessProbeLimiter()
	limiter.now = func() time.Time { return now }
	limiter.lastCleanup = now

	for i := range accessProbeLimit {
		if !limiter.allow("room-a", "192.0.2.1:1000", false) {
			t.Fatalf("probe %d rejected before threshold", i+1)
		}
	}
	if limiter.allow("room-a", "192.0.2.1:2000", false) {
		t.Fatal("probe after threshold was accepted")
	}
	if !limiter.allow("room-a", "192.0.2.1:3000", true) ||
		!limiter.allow("room-a", "192.0.2.1:4000", false) {
		t.Fatal("correct credential did not reset the failure bucket")
	}

	for i := range 20 {
		limiter.allow("room-b", fmt.Sprintf("198.51.100.%d:1000", i), false)
	}
	now = now.Add(accessProbeWindow)
	limiter.allow("room-c", "203.0.113.1:1000", false)
	if len(limiter.buckets) != 1 {
		t.Fatalf("expired buckets were retained: %d", len(limiter.buckets))
	}
}

package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestPasswordProbeLimiterThresholds(t *testing.T) {
	limiter := newPasswordProbeLimiter()
	for i := range guestPasswordProbeLimit {
		remote := fmt.Sprintf("203.0.113.10:%d", 2000+i)
		if !limiter.allow(remote, true, false) {
			t.Fatalf("probe %d was rejected before the limit", i+1)
		}
	}
	if limiter.allow("203.0.113.10:3000", true, false) {
		t.Fatal("password probe after the limit was allowed")
	}

	for i := range passwordlessGuestLimit {
		if !limiter.allow("203.0.113.20:4000", false, false) {
			t.Fatalf("passwordless guest authentication %d was limited early", i+1)
		}
	}
	if limiter.allow("203.0.113.20:5000", false, false) {
		t.Fatal("passwordless guest authentication after the limit was allowed")
	}
}

func TestPasswordProbeLimiterGlobalThresholdFailClosed(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	limiter := newPasswordProbeLimiter()
	limiter.now = func() time.Time { return now }
	limiter.lastCleanup = now

	for i := range globalPasswordProbeLimit {
		remote := fmt.Sprintf("203.0.113.%d:%d", i/guestPasswordProbeLimit, 2000+i)
		if !limiter.allow(remote, true, false) {
			t.Fatalf("global probe %d was rejected before the limit", i+1)
		}
	}
	if limiter.allow("198.51.100.1:1234", true, true) {
		t.Fatal("correct password was allowed while the global bucket was full")
	}

	now = now.Add(guestPasswordProbeWindow)
	if !limiter.allow("198.51.100.1:1234", true, true) {
		t.Fatal("expired global probe window did not decay")
	}
}

func TestPasswordProbeLimiterWindowExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	limiter := newPasswordProbeLimiter()
	limiter.now = func() time.Time { return now }
	limiter.lastCleanup = now

	for i := range guestPasswordProbeLimit {
		if !limiter.allow("198.51.100.8:1234", true, false) {
			t.Fatalf("probe %d was rejected before the limit", i+1)
		}
	}
	if limiter.allow("198.51.100.8:1234", true, false) {
		t.Fatal("password probe after the limit was allowed")
	}

	now = now.Add(guestPasswordProbeWindow)
	if !limiter.allow("198.51.100.8:4321", true, false) {
		t.Fatal("expired probe window did not decay")
	}
}

func TestPasswordProbeLimiterCorrectPasswordResets(t *testing.T) {
	limiter := newPasswordProbeLimiter()
	for i := range guestPasswordProbeLimit - 1 {
		if !limiter.allow("192.0.2.44:1234", true, false) {
			t.Fatalf("probe %d was rejected before the limit", i+1)
		}
	}
	if !limiter.allow("192.0.2.44:2345", true, true) {
		t.Fatal("correct password was rejected before the limit")
	}

	for i := range guestPasswordProbeLimit {
		if !limiter.allow("192.0.2.44:3456", true, false) {
			t.Fatalf("probe %d after reset was rejected", i+1)
		}
	}
	if limiter.allow("192.0.2.44:4567", true, false) {
		t.Fatal("reset bucket did not enforce a fresh limit")
	}
}

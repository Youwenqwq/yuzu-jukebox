package wsapi

import (
	"testing"
	"time"
)

func TestCommandTokenBucketRateAndBurst(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	bucket := newCommandTokenBucket(now)

	for i := range commandBurst {
		if !bucket.allow(now) {
			t.Fatalf("burst command %d was rejected", i+1)
		}
	}
	if bucket.allow(now) {
		t.Fatal("command beyond burst was accepted without refill")
	}

	now = now.Add(time.Second/commandRatePerSecond + time.Nanosecond)
	if !bucket.allow(now) {
		t.Fatal("one token was not restored at 30 commands/second")
	}
	if bucket.allow(now) {
		t.Fatal("fractional refill admitted a second command")
	}

	now = now.Add(10 * time.Second)
	for i := range commandBurst {
		if !bucket.allow(now) {
			t.Fatalf("refilled burst command %d was rejected", i+1)
		}
	}
	if bucket.allow(now) {
		t.Fatal("long idle period refilled beyond burst capacity")
	}
}

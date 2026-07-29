package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestDistributionLeaseLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "distribution.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	const now = int64(1_000_000)

	if err := st.RequestDistribution(ctx, "edgeone", "local:song", now); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestDistribution(ctx, "edgeone", "local:song", now+1); err != nil {
		t.Fatal(err)
	}
	metrics, err := st.DistributionMetrics(ctx, "edgeone")
	if err != nil {
		t.Fatal(err)
	}
	if metrics["requests"] != 1 {
		t.Fatalf("requests metric = %d, want 1", metrics["requests"])
	}

	lease, err := st.ClaimDistribution(ctx, "edgeone", "publisher-a", "lease-a", now+2, now+602_000)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TrackRef != "local:song" || lease.Owner != "publisher-a" {
		t.Fatalf("lease = %#v", lease)
	}
	if _, err := st.ClaimDistribution(ctx, "edgeone", "publisher-b", "lease-b", now+3, now+603_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second claim error = %v, want sql.ErrNoRows", err)
	}

	candidate := DistributionCandidate{
		Backend: "edgeone", TrackRef: "local:song",
		ContentVersion: "abc", Locator: "opaque/blob/key", Layout: "object",
		SizeBytes: 1234, ContentType: "audio/mpeg", ETag: "etag-a",
	}
	if err := st.CompleteDistribution(ctx, lease.ID, "wrong-owner", candidate, now+4); !errors.Is(err, ErrDistributionLeaseInvalid) {
		t.Fatalf("wrong owner error = %v", err)
	}
	if err := st.CompleteDistribution(ctx, lease.ID, lease.Owner, candidate, now+5); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDistributionCandidate(ctx, "edgeone", "local:song")
	if err != nil {
		t.Fatal(err)
	}
	if got.Locator != candidate.Locator || got.ContentVersion != candidate.ContentVersion || got.SizeBytes != candidate.SizeBytes {
		t.Fatalf("candidate = %#v, want %#v", got, candidate)
	}
	status, err := st.DistributionStatus(ctx, "edgeone", now+6)
	if err != nil {
		t.Fatal(err)
	}
	if status.Requested != 1 || status.Ready != 1 || status.Pending != 0 || status.Leased != 0 {
		t.Fatalf("status = %#v", status)
	}
	metrics, err = st.DistributionMetrics(ctx, "edgeone")
	if err != nil {
		t.Fatal(err)
	}
	if metrics["publish_success"] != 1 || metrics["uploaded_bytes"] != 1234 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestDistributionExpiredLeaseCanBeReclaimedAndFailed(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "distribution.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	if err := st.RequestDistribution(ctx, "edgeone", "ncm:1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDistribution(ctx, "edgeone", "old", "old-lease", 110, 120); err != nil {
		t.Fatal(err)
	}
	lease, err := st.ClaimDistribution(ctx, "edgeone", "new", "new-lease", 121, 500)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != "new-lease" {
		t.Fatalf("reclaimed lease = %#v", lease)
	}
	if err := st.FailDistribution(ctx, lease.ID, lease.Owner, "temporary", 130, 230); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDistribution(ctx, "edgeone", "early", "early-lease", 200, 500); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim before retry = %v", err)
	}
	if _, err := st.ClaimDistribution(ctx, "edgeone", "retry", "retry-lease", 230, 600); err != nil {
		t.Fatalf("claim at retry time: %v", err)
	}
}

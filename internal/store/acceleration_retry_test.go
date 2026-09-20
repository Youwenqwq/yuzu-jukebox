package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestDistributionRetryLimitSurvivesDemandAndRestart(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retry.db")
			st, err := Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if st != nil {
					st.Close()
				}
			}()
			createDistributionTestAcceleration(t, st)
			ctx := context.Background()
			now := int64(1_000_000)
			if err := st.RequestDistribution(ctx, "edgeone", "ncm:retry", now); err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 5; attempt++ {
				lease, err := st.ClaimDistribution(ctx, "edgeone", "publisher", fmt.Sprintf("lease-%d", attempt), now, now+1000)
				if err != nil {
					t.Fatal(err)
				}
				if expired {
					now += 1000
					if _, err := st.ClaimDistribution(ctx, "edgeone", "other", "expired-poll", now, now+1000); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("expiry immediately reclaimed: %v", err)
					}
				} else {
					now++
					if err := st.FailDistribution(ctx, lease.ID, lease.Owner, "temporary backend outage", "publish_failed", now); err != nil {
						t.Fatal(err)
					}
				}
				view, err := st.GetDistributionRequest(ctx, "edgeone", "ncm:retry", now)
				if err != nil {
					t.Fatal(err)
				}
				if view.Attempts != int64(attempt) || view.ConsecutiveAttempts != int64(attempt) {
					t.Fatalf("attempt accounting: %#v", view)
				}
				if attempt < 5 {
					wantNext := now + int64(60_000<<(attempt-1))
					if view.State != "retry_wait" || view.NextAttemptAt != wantNext {
						t.Fatalf("backoff: %#v, want %d", view, wantNext)
					}
					if err := st.RequestDistribution(ctx, "edgeone", "ncm:retry", now+1); err != nil {
						t.Fatal(err)
					}
					if _, err := st.ClaimDistribution(ctx, "edgeone", "publisher", "early", wantNext-1, wantNext+1000); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("claim before deadline: %v", err)
					}
					now = wantNext
				} else if view.State != "failed" || view.NextAttemptAt != 0 {
					t.Fatalf("terminal state: %#v", view)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			now += int64((30 * 24 * time.Hour) / time.Millisecond)
			if err := st.RequestDistribution(ctx, "edgeone", "ncm:retry", now); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ClaimDistribution(ctx, "edgeone", "publisher", "revived", now, now+1000); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("terminal revived: %v", err)
			}
			status, err := st.DistributionStatus(ctx, "edgeone", now)
			if err != nil {
				t.Fatal(err)
			}
			if status.Failed != 1 || status.Queued != 0 || status.RetryWait != 0 {
				t.Fatalf("status: %#v", status)
			}
			requests, err := st.ListDistributionRequests(ctx, "edgeone", "failed", now, 10)
			if err != nil || len(requests) != 1 || requests[0].TrackRef != "ncm:retry" {
				t.Fatalf("failed query: %#v, %v", requests, err)
			}
		})
	}
}

func TestDistributionSizePolicySkipsKnownAndReportedObjects(t *testing.T) {
	st := openManagedAccelerationStore(t, 100, 95, 80)
	ctx := context.Background()
	if _, err := st.db.Exec(`INSERT INTO media_cache(track_ref, file_path, size_bytes, last_accessed_at, created_at) VALUES ('ncm:large', '/not-read', 101, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestDistribution(ctx, "managed", "ncm:large", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDistribution(ctx, "managed", "publisher", "known", 1001, 2000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("known oversized object was leased: %v", err)
	}
	if err := st.RequestDistribution(ctx, "managed", "ncm:unknown", 1002); err != nil {
		t.Fatal(err)
	}
	lease, err := st.ClaimDistribution(ctx, "managed", "publisher", "unknown", 1003, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailDistribution(ctx, lease.ID, lease.Owner, "size policy", "object_too_large", 1004); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"ncm:large", "ncm:unknown"} {
		if err := st.RequestDistribution(ctx, "managed", ref, 900_000_000); err != nil {
			t.Fatal(err)
		}
		view, err := st.GetDistributionRequest(ctx, "managed", ref, 900_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if view.State != "skipped" || view.ErrorCode != "object_too_large" || view.NextAttemptAt != 0 || view.CanceledAt != 0 || view.EvictedAt != 0 {
			t.Fatalf("policy state: %#v", view)
		}
	}
	if _, err := st.ClaimDistribution(ctx, "managed", "publisher", "weekly-retry", 900_000_000, 900_001_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("oversized object retried: %v", err)
	}
}

func TestExistingCandidateStopsServingAfterSizeLimitReduction(t *testing.T) {
	st := openManagedAccelerationStore(t, 1000, 95, 80)
	ctx := context.Background()
	publishStorageTestCandidate(t, st, "local:large", "large-lease", "large-object", 101, 1000)
	if _, err := st.db.Exec(`UPDATE accelerations SET max_object_bytes = 100 WHERE id = 'managed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDistributionCandidate(ctx, "managed", "local:large"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("oversized candidate still served: %v", err)
	}
	view, err := st.GetDistributionRequest(ctx, "managed", "local:large", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "skipped" || view.Candidate != nil {
		t.Fatalf("candidate policy state: %#v", view)
	}
	status, err := st.DistributionStatus(ctx, "managed", 2000)
	if err != nil || status.Ready != 0 || status.Skipped != 1 {
		t.Fatalf("candidate policy count: %#v, %v", status, err)
	}
	if err := st.PinAccelerationDemand(ctx, "managed", []string{"local:large"}, 900_000, 2001); err != nil {
		t.Fatal(err)
	}
	if pinned := accelerationObjectPin(t, st, "large-object"); pinned != 0 {
		t.Fatalf("skipped candidate was protected from GC until %d", pinned)
	}
}

func TestInventoryFailuresStopAutomaticCloudWork(t *testing.T) {
	st := openManagedAccelerationStore(t, 1000, 95, 80)
	ctx := context.Background()
	if _, err := st.db.Exec(`UPDATE accelerations SET enabled = 1 WHERE id = 'managed'`); err != nil {
		t.Fatal(err)
	}
	now := int64(1_000_000)
	scan, err := st.RequestAccelerationInventoryScan(ctx, "managed", now)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		claimed, err := st.ClaimAccelerationInventoryScan(ctx, "managed", "publisher", time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.ID != scan.ID {
			t.Fatal("retry replaced scan identity")
		}
		if err := st.FailAccelerationInventoryScan(ctx, "managed", scan.ID, "publisher", "backend unavailable", now+1); err != nil {
			t.Fatal(err)
		}
		scan, err = st.GetAccelerationInventoryScan(ctx, "managed", scan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 5 {
			if _, err := st.ClaimAccelerationInventoryScan(ctx, "managed", "publisher", time.Minute, scan.NextAttemptAt-1); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("inventory bypassed backoff: %v", err)
			}
			now = scan.NextAttemptAt
		}
	}
	if scan.State != "failed" || scan.Attempts != 5 {
		t.Fatalf("inventory terminal: %#v", scan)
	}
	now += int64((30 * 24 * time.Hour) / time.Millisecond)
	if _, err := st.ClaimAccelerationInventoryScan(ctx, "managed", "publisher", time.Minute, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("inventory terminal reclaimed: %v", err)
	}
	scheduled, err := st.ScheduleDueAccelerationInventoryScans(ctx, now)
	if err != nil || len(scheduled) != 0 {
		t.Fatalf("scheduler revived failed inventory: %#v, %v", scheduled, err)
	}
	manual, err := st.RequestAccelerationInventoryScan(ctx, "managed", now+1)
	if err != nil || manual.ID == scan.ID || manual.State != "queued" {
		t.Fatalf("explicit inventory refresh: %#v, %v", manual, err)
	}
}

func TestDeletionFailuresRemainAccountedButStopRetrying(t *testing.T) {
	st := openManagedAccelerationStore(t, 1000, 95, 80)
	ctx := context.Background()
	if _, err := st.db.Exec(`INSERT INTO acceleration_objects(acceleration_id,locator,size_bytes,state,last_accessed_at,created_at,updated_at) VALUES ('managed','object',100,'deleting',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO acceleration_deletion_jobs(id,acceleration_id,locator,state,created_at,updated_at) VALUES ('deletion','managed','object','pending',1,1)`); err != nil {
		t.Fatal(err)
	}
	now := int64(1_000_000)
	for attempt := 1; attempt <= 5; attempt++ {
		deletion, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FailAccelerationDeletion(ctx, "managed", deletion.ID, deletion.Owner, "backend unavailable", now+1); err != nil {
			t.Fatal(err)
		}
		next := now + 1 + int64(60_000<<(attempt-1))
		if _, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, next-1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("deletion bypassed backoff: %v", err)
		}
		now = next
	}
	if _, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, now+900_000_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deletion terminal reclaimed: %v", err)
	}
	status, err := st.AccelerationStorageStatus(ctx, "managed", now)
	if err != nil || status.AccountedBytes != 100 {
		t.Fatalf("failed deletion disappeared from accounting: %#v, %v", status, err)
	}
}

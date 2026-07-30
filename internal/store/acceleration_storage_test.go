package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAccelerationStorageReservationGCAndInventory(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "storage.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	publisherHash := make([]byte, 32)
	deliveryHash := make([]byte, 32)
	publisherHash[0], deliveryHash[0] = 1, 2
	_, err = st.CreateAcceleration(ctx, Acceleration{
		ID: "managed", Name: "Managed", Kind: "edgeone",
		ControlBaseURL: "https://control.test", BackendBaseURL: "https://backend.test",
		LeaseTTLSeconds: 600, MaxObjectBytes: 100,
		StorageBudgetBytes: 100, StorageHighWatermarkPercent: 95,
		StorageLowWatermarkPercent: 80,
	}, publisherHash, deliveryHash, "backend-token")
	if err != nil {
		t.Fatal(err)
	}

	first := publishStorageTestCandidate(t, st, "local:first", "lease-first", "object-first", 60, 100)
	if first.Locator != "object-first" {
		t.Fatalf("first candidate = %#v", first)
	}
	if err := st.RequestDistribution(ctx, "managed", "local:second", 200); err != nil {
		t.Fatal(err)
	}
	secondLease, err := st.ClaimDistribution(ctx, "managed", "publisher", "lease-second", 201, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ReserveAccelerationStorage(ctx, secondLease.ID, secondLease.Owner, "object-second", 40, 202)
	if !errors.Is(err, ErrAccelerationStorageFull) {
		t.Fatalf("reserve over high watermark = %v, want storage full", err)
	}
	if _, err := st.GetDistributionCandidate(ctx, "managed", "local:first"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("evicted candidate lookup = %v, want sql.ErrNoRows", err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, secondLease.ID, secondLease.Owner,
		"object-first", 60, 203); !errors.Is(err, ErrStorageReservationInProgress) {
		t.Fatalf("reservation racing deletion = %v, want in progress", err)
	}
	deletion, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, 203)
	if err != nil {
		t.Fatal(err)
	}
	if deletion.Locator != "object-first" {
		t.Fatalf("deletion = %#v", deletion)
	}
	if err := st.CompleteAccelerationDeletion(ctx, "managed", deletion.ID, deletion.Owner, 204); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, secondLease.ID, secondLease.Owner, "object-second", 40, 205); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestDistribution(ctx, "managed", "local:duplicate", 205); err != nil {
		t.Fatal(err)
	}
	duplicateLease, err := st.ClaimDistribution(ctx, "managed", "publisher-2", "lease-duplicate", 206, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, duplicateLease.ID, duplicateLease.Owner,
		"object-second", 40, 207); !errors.Is(err, ErrStorageReservationInProgress) {
		t.Fatalf("competing reservation = %v, want in progress", err)
	}
	second := DistributionCandidate{
		AccelerationID: "managed", TrackRef: "local:second", ContentVersion: "version-second",
		Locator: "object-second", Layout: "object", SizeBytes: 40, ContentType: "audio/mpeg",
	}
	if err := st.CompleteDistribution(ctx, secondLease.ID, secondLease.Owner, second, 206); err != nil {
		t.Fatal(err)
	}

	if err := st.AppendAccelerationInventory(ctx, "managed", "publisher", "inventory-1", 300,
		[]StorageInventoryObject{
			{Locator: "object-second", SizeBytes: 40, ExternalVersion: "etag-second"},
			{Locator: "object-orphan", SizeBytes: 20, ExternalVersion: "etag-orphan"},
		}, true, 301); err != nil {
		t.Fatal(err)
	}
	status, err := st.AccelerationStorageStatus(ctx, "managed", 302)
	if err != nil {
		t.Fatal(err)
	}
	if status.AccountedBytes != 60 || status.ObservedBytes != 60 || status.OrphanCount != 1 || status.MissingCount != 0 {
		t.Fatalf("reconciled status = %#v", status)
	}
	if err := st.AppendAccelerationInventory(ctx, "managed", "publisher", "inventory-2", 400,
		[]StorageInventoryObject{{Locator: "object-second", SizeBytes: 40, ExternalVersion: "etag-second"}},
		true, 401); err != nil {
		t.Fatal(err)
	}
	status, err = st.AccelerationStorageStatus(ctx, "managed", 402)
	if err != nil {
		t.Fatal(err)
	}
	if status.AccountedBytes != 40 || status.ObservedBytes != 40 || status.OrphanCount != 0 || status.MissingCount != 1 {
		t.Fatalf("missing-object status = %#v", status)
	}
}

func TestAccelerationInventoryScanCommitsAtomically(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "inventory.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	publisherHash := make([]byte, 32)
	deliveryHash := make([]byte, 32)
	publisherHash[0], deliveryHash[0] = 1, 2
	_, err = st.CreateAcceleration(ctx, Acceleration{
		ID: "inventory", Name: "Inventory", Kind: "edgeone",
		ControlBaseURL: "https://control.test", BackendBaseURL: "https://backend.test",
		LeaseTTLSeconds: 600, MaxObjectBytes: 100, StorageBudgetBytes: 1000,
		StorageHighWatermarkPercent: 95, StorageLowWatermarkPercent: 80,
		InventoryIntervalSeconds: 60, InventoryStaleAfterSeconds: 120,
	}, publisherHash, deliveryHash, "backend-token")
	if err != nil {
		t.Fatal(err)
	}

	scan, err := st.RequestAccelerationInventoryScan(ctx, "inventory", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	scan, err = st.ClaimAccelerationInventoryScan(ctx, "inventory", "publisher",
		time.Minute, 1_000_001)
	if err != nil {
		t.Fatal(err)
	}
	firstPage := []StorageInventoryObject{{Locator: "object-a", SizeBytes: 10}}
	if err := st.AppendClaimedAccelerationInventory(ctx, "inventory", scan.ID,
		scan.Owner, 1_000_002, firstPage, false, 1_000_003); err != nil {
		t.Fatal(err)
	}
	status, err := st.AccelerationStorageStatus(ctx, "inventory", 1_000_004)
	if err != nil {
		t.Fatal(err)
	}
	if status.ObservedAt != 0 || status.ObservedObjectCount != 0 || !status.Stale {
		t.Fatalf("partial scan changed active snapshot: %#v", status)
	}
	secondPage := []StorageInventoryObject{{Locator: "object-b", SizeBytes: 20}}
	if err := st.AppendClaimedAccelerationInventory(ctx, "inventory", scan.ID,
		scan.Owner, 1_000_002, secondPage, true, 1_000_005); err != nil {
		t.Fatal(err)
	}
	status, err = st.AccelerationStorageStatus(ctx, "inventory", 1_000_006)
	if err != nil {
		t.Fatal(err)
	}
	if status.ObservedObjectCount != 2 || status.ObservedBytes != 30 ||
		status.ObservedAt != 1_000_002 || status.Stale {
		t.Fatalf("completed snapshot = %#v", status)
	}
	completed, err := st.GetAccelerationInventoryScan(ctx, "inventory", scan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != "completed" {
		t.Fatalf("scan = %#v", completed)
	}

	failed, err := st.RequestAccelerationInventoryScan(ctx, "inventory", 1_000_010)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = st.ClaimAccelerationInventoryScan(ctx, "inventory", "publisher",
		time.Minute, 1_000_011)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendClaimedAccelerationInventory(ctx, "inventory", failed.ID,
		failed.Owner, 1_000_012, []StorageInventoryObject{{Locator: "partial", SizeBytes: 99}},
		false, 1_000_013); err != nil {
		t.Fatal(err)
	}
	if err := st.FailAccelerationInventoryScan(ctx, "inventory", failed.ID,
		failed.Owner, "backend failed", 1_000_014); err != nil {
		t.Fatal(err)
	}
	status, err = st.AccelerationStorageStatus(ctx, "inventory", 1_000_015)
	if err != nil {
		t.Fatal(err)
	}
	if status.ObservedObjectCount != 2 || status.ObservedBytes != 30 ||
		status.ObservedAt != 1_000_002 || status.ReconciliationError != "backend failed" {
		t.Fatalf("failed scan replaced complete snapshot: %#v", status)
	}
}

func publishStorageTestCandidate(
	t *testing.T,
	st *Store,
	trackRef, leaseID, locator string,
	size, now int64,
) DistributionCandidate {
	t.Helper()
	ctx := context.Background()
	if err := st.RequestDistribution(ctx, "managed", trackRef, now); err != nil {
		t.Fatal(err)
	}
	lease, err := st.ClaimDistribution(ctx, "managed", "publisher", leaseID, now+1, now+10_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, lease.ID, lease.Owner, locator, size, now+2); err != nil {
		t.Fatal(err)
	}
	candidate := DistributionCandidate{
		AccelerationID: "managed", TrackRef: trackRef, ContentVersion: "version-" + locator,
		Locator: locator, Layout: "object", SizeBytes: size, ContentType: "audio/mpeg",
	}
	if err := st.CompleteDistribution(ctx, lease.ID, lease.Owner, candidate, now+3); err != nil {
		t.Fatal(err)
	}
	return candidate
}

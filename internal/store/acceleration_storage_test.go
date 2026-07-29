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

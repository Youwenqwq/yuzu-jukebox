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

func TestAccelerationInventorySparesObjectsCreatedAfterScanStart(t *testing.T) {
	st := openManagedAccelerationStore(t, 1000, 95, 80)
	ctx := context.Background()

	// 分页扫描要花好几秒：observedAt 之前完成的上传该由本次扫描判决，
	// observedAt 之后完成的上传不可能出现在快照里，必须放过。
	publishStorageTestCandidate(t, st, "local:before", "lease-before", "object-before", 40, 1_000)
	observedAt := int64(2_000)
	publishStorageTestCandidate(t, st, "local:during", "lease-during", "object-during", 50, 3_000)

	if err := st.AppendAccelerationInventory(ctx, "managed", "publisher", "inventory-1",
		observedAt, nil, true, 4_000); err != nil {
		t.Fatal(err)
	}
	status, err := st.AccelerationStorageStatus(ctx, "managed", 4_001)
	if err != nil {
		t.Fatal(err)
	}
	if status.MissingCount != 1 || status.AccountedBytes != 50 {
		t.Fatalf("scan-window status = %#v, want only object-before missing", status)
	}
	var state string
	if err := st.db.QueryRowContext(ctx, `SELECT state FROM acceleration_objects
		WHERE acceleration_id = ? AND locator = ?`, "managed", "object-during").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("object created during scan = %q, want ready", state)
	}

	// 下一次扫描的 observedAt 已经晚于该对象的 created_at，此时才应判决它。
	if err := st.AppendAccelerationInventory(ctx, "managed", "publisher", "inventory-2",
		5_000, nil, true, 5_001); err != nil {
		t.Fatal(err)
	}
	status, err = st.AccelerationStorageStatus(ctx, "managed", 5_002)
	if err != nil {
		t.Fatal(err)
	}
	if status.MissingCount != 2 || status.AccountedBytes != 0 {
		t.Fatalf("later scan status = %#v, want both objects missing", status)
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

// 驱逐是缓存的正常工作，不是待重试的失败。GC 回收对象后请求必须退出可认领集合，
// 否则它会立刻从 ready 翻回 queued，形成"删了又传"的永动循环。
func TestEvictedRequestLeavesQueueUntilDemandReturns(t *testing.T) {
	st := openManagedAccelerationStore(t, 100, 95, 80)
	ctx := context.Background()
	publishStorageTestCandidate(t, st, "local:first", "lease-first", "object-first", 60, 100)

	if err := st.RequestDistribution(ctx, "managed", "local:second", 200); err != nil {
		t.Fatal(err)
	}
	lease, err := st.ClaimDistribution(ctx, "managed", "publisher", "lease-second", 201, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, lease.ID, lease.Owner,
		"object-second", 40, 202); !errors.Is(err, ErrAccelerationStorageFull) {
		t.Fatalf("reserve over high watermark = %v, want storage full", err)
	}

	evicted, err := st.GetDistributionRequest(ctx, "managed", "local:first", 203)
	if err != nil {
		t.Fatal(err)
	}
	if evicted.State != "evicted" || evicted.EvictedAt == 0 {
		t.Fatalf("request after eviction = %#v, want evicted state", evicted)
	}

	// 排空删除队列，排除回收背压的干扰，确认驱逐本身就让请求退出了可认领集合。
	deletion, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, 204)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteAccelerationDeletion(ctx, "managed", deletion.ID, deletion.Owner, 205); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDistribution(ctx, "managed", "publisher",
		"lease-recycled", 206, 20_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim of evicted request = %v, want sql.ErrNoRows", err)
	}
	status, err := st.DistributionStatus(ctx, "managed", 207)
	if err != nil {
		t.Fatal(err)
	}
	if status.Evicted != 1 || status.Queued != 0 || status.RetryWait != 0 {
		t.Fatalf("status after eviction = %#v", status)
	}

	// 真实需求（播放或缓存就绪）是唯一的复活路径。
	if err := st.RequestDistribution(ctx, "managed", "local:first", 208); err != nil {
		t.Fatal(err)
	}
	revived, err := st.GetDistributionRequest(ctx, "managed", "local:first", 209)
	if err != nil {
		t.Fatal(err)
	}
	if revived.State != "queued" || revived.EvictedAt != 0 {
		t.Fatalf("revived request = %#v, want queued", revived)
	}
	claimed, err := st.ClaimDistribution(ctx, "managed", "publisher", "lease-revived", 210, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TrackRef != "local:first" {
		t.Fatalf("claimed lease = %#v, want local:first", claimed)
	}
}

// 回收在途时不派发新工作：否则 publisher 会先下载完整源再撞 507，每个失败周期
// 浪费一次整源下载。
func TestClaimBlockedWhileReclaimInFlight(t *testing.T) {
	st := openManagedAccelerationStore(t, 1000, 95, 70)
	ctx := context.Background()
	publishStorageTestCandidate(t, st, "local:cold", "lease-cold", "object-cold", 400, 100)
	publishStorageTestCandidate(t, st, "local:warm", "lease-warm", "object-warm", 400, 200)

	if err := st.RequestDistribution(ctx, "managed", "local:incoming", 300); err != nil {
		t.Fatal(err)
	}
	lease, err := st.ClaimDistribution(ctx, "managed", "publisher", "lease-incoming", 301, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveAccelerationStorage(ctx, lease.ID, lease.Owner,
		"object-incoming", 200, 302); !errors.Is(err, ErrAccelerationStorageFull) {
		t.Fatalf("reserve over high watermark = %v, want storage full", err)
	}

	// local:pending 是一条干净的排队请求，它被拒绝只可能是因为回收背压。
	if err := st.RequestDistribution(ctx, "managed", "local:pending", 303); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDistribution(ctx, "managed", "publisher",
		"lease-blocked", 304, 20_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim while reclaim in flight = %v, want sql.ErrNoRows", err)
	}

	deletion, err := st.ClaimAccelerationDeletion(ctx, "managed", "publisher", time.Minute, 305)
	if err != nil {
		t.Fatal(err)
	}
	if deletion.Locator != "object-cold" {
		t.Fatalf("deletion = %#v, want least recently used object", deletion)
	}
	if err := st.CompleteAccelerationDeletion(ctx, "managed", deletion.ID, deletion.Owner, 306); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimDistribution(ctx, "managed", "publisher", "lease-unblocked", 307, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TrackRef != "local:pending" {
		t.Fatalf("claimed lease after reclaim = %#v, want local:pending", claimed)
	}
}

func openManagedAccelerationStore(t *testing.T, budget int64, high, low int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "storage.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	publisherHash := make([]byte, 32)
	deliveryHash := make([]byte, 32)
	publisherHash[0], deliveryHash[0] = 1, 2
	if _, err := st.CreateAcceleration(context.Background(), Acceleration{
		ID: "managed", Name: "Managed", Kind: "edgeone",
		ControlBaseURL: "https://control.test", BackendBaseURL: "https://backend.test",
		LeaseTTLSeconds: 600, MaxObjectBytes: budget,
		StorageBudgetBytes: budget, StorageHighWatermarkPercent: high,
		StorageLowWatermarkPercent: low,
	}, publisherHash, deliveryHash, "backend-token"); err != nil {
		t.Fatal(err)
	}
	return st
}

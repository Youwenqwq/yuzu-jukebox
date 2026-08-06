package credmon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// fakeProvider 可编程的 CredentialAware 假实现。
type fakeProvider struct {
	id     string
	status string // CredentialStatus 返回值
	calls  int
}

func (f *fakeProvider) ID() string { return f.id }
func (f *fakeProvider) Search(ctx context.Context, q string, limit, offset int) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakeProvider) GetTrack(ctx context.Context, r provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, nil
}
func (f *fakeProvider) Resolve(ctx context.Context, r provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, nil
}
func (f *fakeProvider) SetCredential(ctx context.Context, payload string) error { return nil }
func (f *fakeProvider) CredentialStatus(ctx context.Context) string {
	f.calls++
	return f.status
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMonitorUpdatesStatusAndAuditsTransitions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	fp := &fakeProvider{id: "fake", status: "ok"}
	reg := provider.NewRegistry()
	reg.Register(fp)

	// 种入一条凭据
	if err := st.UpsertCredential(ctx, "fake", "SECRET", "ok"); err != nil {
		t.Fatal(err)
	}

	m := New(reg, st)
	m.checkAll(ctx)

	// 状态已写回（仍为 ok），无翻转审计
	if got, _ := st.GetCredentialStatus(ctx, "fake"); got != "ok" {
		t.Fatalf("status = %q, want ok", got)
	}
	if fp.calls != 1 {
		t.Fatalf("CredentialStatus called %d times, want 1", fp.calls)
	}

	// 凭据失效 → 状态翻转，应记审计
	fp.status = "invalid"
	m.checkAll(ctx)
	if got, _ := st.GetCredentialStatus(ctx, "fake"); got != "invalid" {
		t.Fatalf("status = %q, want invalid", got)
	}
	var auditCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'credential.status_change' AND target = 'fake'`).
		Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount)
	}

	// 再次检查（状态未变）→ 不应重复记审计
	m.checkAll(ctx)
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'credential.status_change'`).
		Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows after no-change check = %d, want still 1", auditCount)
	}
}

func TestMonitorSkipsUnsetProviders(t *testing.T) {
	st := newTestStore(t)
	fp := &fakeProvider{id: "fake", status: "ok"}
	reg := provider.NewRegistry()
	reg.Register(fp)

	m := New(reg, st)
	m.checkAll(context.Background())

	if fp.calls != 0 {
		t.Fatalf("unset provider was probed %d times, want 0", fp.calls)
	}
}

func TestMonitorFirstCheckEstablishesBaselineWithoutAudit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	fp := &fakeProvider{id: "fake", status: "invalid"} // 启动时凭据就已失效
	reg := provider.NewRegistry()
	reg.Register(fp)
	if err := st.UpsertCredential(ctx, "fake", "SECRET", "ok"); err != nil {
		t.Fatal(err)
	}

	m := New(reg, st)
	m.checkAll(ctx)

	var auditCount int
	st.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'credential.status_change'`).
		Scan(&auditCount)
	if auditCount != 0 {
		t.Fatalf("baseline check produced %d audit rows, want 0 (no transition from memory)", auditCount)
	}
	if got, _ := st.GetCredentialStatus(ctx, "fake"); got != "invalid" {
		t.Fatalf("status = %q, want invalid", got)
	}
}

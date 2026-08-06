package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCredentialOwnerBindAndRead(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 无凭据行：ok=false
	if _, ok, err := st.GetCredentialOwner(ctx, "ncm"); err != nil || ok {
		t.Fatalf("no credential row: ok=%v err=%v, want false/nil", ok, err)
	}

	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=x", "ok"); err != nil {
		t.Fatal(err)
	}
	// 新凭据默认未绑定
	owner, ok, err := st.GetCredentialOwner(ctx, "ncm")
	if err != nil || !ok {
		t.Fatalf("GetCredentialOwner: ok=%v err=%v", ok, err)
	}
	if owner.PrincipalID != "" {
		t.Fatalf("fresh credential should be unbound, got %q", owner.PrincipalID)
	}

	if err := st.SetCredentialOwner(ctx, "ncm", "principal-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCredentialAccount(ctx, "ncm", AccountProfile{UID: "12345", Name: "小明", Avatar: "https://pic"}); err != nil {
		t.Fatal(err)
	}
	owner, _, err = st.GetCredentialOwner(ctx, "ncm")
	if err != nil {
		t.Fatal(err)
	}
	if owner.PrincipalID != "principal-1" || owner.Account.UID != "12345" ||
		owner.Account.Name != "小明" || owner.Account.Avatar != "https://pic" {
		t.Fatalf("owner/account round trip: %+v", owner)
	}
}

func TestCredentialOwnerInheritedAcrossRotation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=old", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCredentialOwner(ctx, "ncm", "principal-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCredentialAccount(ctx, "ncm", AccountProfile{UID: "12345"}); err != nil {
		t.Fatal(err)
	}

	// cookie 轮换（无身份写入路径）：绑定与账号资料必须跟随到新行
	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=new", "ok"); err != nil {
		t.Fatal(err)
	}
	owner, ok, err := st.GetCredentialOwner(ctx, "ncm")
	if err != nil || !ok {
		t.Fatalf("GetCredentialOwner: ok=%v err=%v", ok, err)
	}
	if owner.PrincipalID != "principal-1" || owner.Account.UID != "12345" {
		t.Fatalf("owner not inherited across rotation: %+v", owner)
	}

	// 重新委托：人设凭据显式重绑
	if err := st.SetCredentialOwner(ctx, "ncm", "principal-2"); err != nil {
		t.Fatal(err)
	}
	owner, _, _ = st.GetCredentialOwner(ctx, "ncm")
	if owner.PrincipalID != "principal-2" {
		t.Fatalf("re-bind failed: %+v", owner)
	}
}

func TestCredentialOwnerIsolationAcrossProviders(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=x", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCredentialOwner(ctx, "ncm", "principal-1"); err != nil {
		t.Fatal(err)
	}
	// 另一 provider 的首行不得继承 ncm 的绑定
	if err := st.UpsertCredential(ctx, "bili", "SESSDATA=y", "ok"); err != nil {
		t.Fatal(err)
	}
	owner, ok, err := st.GetCredentialOwner(ctx, "bili")
	if err != nil || !ok {
		t.Fatalf("GetCredentialOwner: ok=%v err=%v", ok, err)
	}
	if owner.PrincipalID != "" {
		t.Fatalf("owner leaked across providers: %+v", owner)
	}
}

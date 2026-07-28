package auth

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestBindingServiceRedeemsOnceAndReplaysExactTarget(t *testing.T) {
	st, identity := newBindingTestStore(t)
	const integrationToken = "binding-integration-token"
	if _, err := st.CreateIntegration(context.Background(), "bridge", "Bridge", HashIntegrationToken(integrationToken)); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	service := NewBindingService(st)
	service.now = func() time.Time { return now }
	service.rand = bytes.NewReader(make([]byte, 8))
	issued, err := service.Issue(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Code != "0000-0000-0000" || issued.ExpiresAt != now.Add(bindingCodeTTL).UnixMilli() {
		t.Fatalf("issued code = %#v", issued)
	}

	target := ExternalBindingTarget{
		IntegrationID: "bridge", AdapterID: "astrbot", ScopeType: "group", ScopeID: "42", SubjectID: "7",
	}
	first, err := service.Redeem(context.Background(), integrationToken, strings.ToLower(issued.Code), target)
	if err != nil || first.Identity.ID != identity.ID || first.Replayed {
		t.Fatalf("first redemption = %#v, %v", first, err)
	}
	replayed, err := service.Redeem(context.Background(), integrationToken, issued.Code, target)
	if err != nil || replayed.Identity.ID != identity.ID || !replayed.Replayed {
		t.Fatalf("replayed redemption = %#v, %v", replayed, err)
	}

	otherTarget := target
	otherTarget.SubjectID = "8"
	if _, err := service.Redeem(context.Background(), integrationToken, issued.Code, otherTarget); !errors.Is(err, ErrBindingCodeInvalid) {
		t.Fatalf("different target error = %v", err)
	}
}

func TestBindingServiceExpiresAndRefusesConflictingLink(t *testing.T) {
	st, identity := newBindingTestStore(t)
	const integrationToken = "binding-integration-token"
	if _, err := st.CreateIntegration(context.Background(), "bridge", "Bridge", HashIntegrationToken(integrationToken)); err != nil {
		t.Fatal(err)
	}
	other := store.Principal{
		ID: "o_other", Name: "Other", Kind: "oidc", OIDCSubject: "other-sub",
		Roles: []string{RoleListener}, Active: true,
	}
	if err := st.UpsertPrincipal(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	target := ExternalBindingTarget{
		IntegrationID: "bridge", AdapterID: "astrbot", ScopeType: "group", ScopeID: "42", SubjectID: "7",
	}
	if err := st.UpsertExternalIdentityLink(
		context.Background(), target.IntegrationID, target.AdapterID,
		target.ScopeType, target.ScopeID, target.SubjectID, other.ID,
	); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	service := NewBindingService(st)
	service.now = func() time.Time { return now }
	service.rand = bytes.NewReader(bytes.Repeat([]byte{1}, 16))
	issued, err := service.Issue(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Redeem(context.Background(), integrationToken, issued.Code, target); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := service.Redeem(context.Background(), integrationToken, issued.Code, target); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("conflict replay error = %v", err)
	}

	second, err := service.Issue(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(bindingCodeTTL) }
	if _, err := service.Redeem(context.Background(), integrationToken, second.Code, ExternalBindingTarget{
		IntegrationID: "bridge", AdapterID: "astrbot", ScopeType: "group", ScopeID: "43", SubjectID: "7",
	}); !errors.Is(err, ErrBindingCodeInvalid) {
		t.Fatalf("expired code error = %v", err)
	}
}

func TestBindingServiceRequiresDirectOIDCIdentity(t *testing.T) {
	st, identity := newBindingTestStore(t)
	service := NewBindingService(st)

	guest := identity
	guest.OIDCSubject = ""
	if _, err := service.Issue(context.Background(), guest); !errors.Is(err, ErrBindingRequiresOIDC) {
		t.Fatalf("guest issue error = %v", err)
	}
	actor := identity
	actor.IntegrationID = "bridge"
	if _, err := service.Issue(context.Background(), actor); !errors.Is(err, ErrBindingRequiresOIDC) {
		t.Fatalf("integration actor issue error = %v", err)
	}
}

func newBindingTestStore(t *testing.T) (*store.Store, Identity) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "binding.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	identity := OIDCIdentity(OIDCClaims{Sub: "binding-sub", Username: "Binding User"}, []string{RoleListener, RoleRequester})
	if err := st.UpsertPrincipal(context.Background(), store.Principal{
		ID: identity.ID, Name: identity.Name, Kind: identity.Kind,
		OIDCSubject: identity.OIDCSubject, Roles: identity.Roles, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	return st, identity
}

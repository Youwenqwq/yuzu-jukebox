package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestPrincipalAndExternalMappings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "principal.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	alice := Principal{
		ID: "principal-alice", Name: "Alice", Avatar: "https://id.example/assets/v1/org1/avatar-key",
		Kind: "oidc", OIDCSubject: "oidc-alice",
		Roles: []string{"listener", "requester"}, Active: true,
	}
	bob := Principal{
		ID: "principal-bob", Name: "Bob", Kind: "guest",
		Roles: []string{"listener", "requester"}, Active: true,
	}
	if err := st.UpsertPrincipal(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPrincipal(ctx, bob); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetPrincipalByOIDCSubject(ctx, alice.OIDCSubject)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != alice.ID || got.Name != alice.Name || got.Kind != alice.Kind || !got.Active {
		t.Fatalf("OIDC principal = %#v, want %#v", got, alice)
	}
	if got.Avatar != alice.Avatar {
		t.Fatalf("avatar = %q, want %q", got.Avatar, alice.Avatar)
	}
	if len(got.Roles) != 2 || got.Roles[1] != "requester" {
		t.Fatalf("principal roles = %#v", got.Roles)
	}

	if err := st.UpsertExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1", alice.ID,
	); err != nil {
		t.Fatal(err)
	}
	principalID, err := st.ResolveExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1",
	)
	if err != nil || principalID != alice.ID {
		t.Fatalf("resolved identity = %q, %v", principalID, err)
	}
	if err := st.UpsertExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1", bob.ID,
	); err != nil {
		t.Fatal(err)
	}
	principalID, err = st.ResolveExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1",
	)
	if err != nil || principalID != bob.ID {
		t.Fatalf("updated identity = %q, %v", principalID, err)
	}
	if err := st.RemoveExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1",
	); err != nil {
		t.Fatal(err)
	}
	_, err = st.ResolveExternalIdentityLink(
		ctx, "integration", "adapter", "channel", "scope-1", "subject-1",
	)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed identity resolve error = %v, want sql.ErrNoRows", err)
	}
}

func TestExternalScopeRoomAndRoomGrants(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "grants.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for _, room := range []Room{
		{ID: "room-1", Name: "Room 1", CreatedAt: 1},
		{ID: "room-2", Name: "Room 2", CreatedAt: 2},
	} {
		if err := st.CreateRoom(ctx, room); err != nil {
			t.Fatal(err)
		}
	}
	principal := Principal{
		ID: "principal-1", Name: "Controller", Kind: "oidc",
		Roles: []string{"listener"}, Active: true,
	}
	const capability = "controller"
	if err := st.UpsertPrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}

	if err := st.BindExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1", "room-1"); err != nil {
		t.Fatal(err)
	}
	roomID, err := st.ResolveExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1")
	if err != nil || roomID != "room-1" {
		t.Fatalf("resolved room = %q, %v", roomID, err)
	}
	if err := st.BindExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1", "room-2"); err != nil {
		t.Fatal(err)
	}
	roomID, err = st.ResolveExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1")
	if err != nil || roomID != "room-2" {
		t.Fatalf("updated room = %q, %v", roomID, err)
	}
	if err := st.RemoveExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1"); err != nil {
		t.Fatal(err)
	}
	_, err = st.ResolveExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed scope resolve error = %v, want sql.ErrNoRows", err)
	}
	if err := st.BindExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1", "room-2"); err != nil {
		t.Fatal(err)
	}

	granted, err := st.HasRoomGrant(ctx, "room-2", principal.ID, capability)
	if err != nil || granted {
		t.Fatalf("grant before insert = %v, %v", granted, err)
	}
	if err := st.GrantRoomGrant(ctx, "room-2", principal.ID, capability); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantRoomGrant(ctx, "room-2", principal.ID, capability); err != nil {
		t.Fatal(err)
	}
	granted, err = st.HasRoomGrant(ctx, "room-2", principal.ID, capability)
	if err != nil || !granted {
		t.Fatalf("grant after insert = %v, %v", granted, err)
	}
	grants, err := st.ListRoomGrants(ctx, "room-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].PrincipalID != principal.ID || grants[0].Capability != capability {
		t.Fatalf("room grants = %#v", grants)
	}
	if err := st.RevokeRoomGrant(ctx, "room-2", principal.ID, capability); err != nil {
		t.Fatal(err)
	}
	granted, err = st.HasRoomGrant(ctx, "room-2", principal.ID, capability)
	if err != nil || granted {
		t.Fatalf("grant after revoke = %v, %v", granted, err)
	}

	if err := st.GrantRoomGrant(ctx, "room-2", principal.ID, capability); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRoom(ctx, "room-2"); err != nil {
		t.Fatal(err)
	}
	_, err = st.ResolveExternalScopeRoom(ctx, "integration", "adapter", "guild", "scope-1")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("room cascade resolve error = %v, want sql.ErrNoRows", err)
	}
	granted, err = st.HasRoomGrant(ctx, "room-2", principal.ID, capability)
	if err != nil || granted {
		t.Fatalf("room cascade grant = %v, %v", granted, err)
	}
}

package control

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"

	"golang.org/x/crypto/bcrypt"
)

type grantKey struct {
	roomID      string
	principalID string
	capability  string
}

type fakeGrantStore struct {
	grants map[grantKey]bool
	err    error
	calls  int
}

func (s *fakeGrantStore) HasRoomGrant(_ context.Context, roomID, principalID, capability string) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.grants[grantKey{roomID: roomID, principalID: principalID, capability: capability}], nil
}

func TestAuthorizerControllerContract(t *testing.T) {
	storeFailure := errors.New("grant lookup failed")
	globalStore := &fakeGrantStore{err: storeFailure}
	global := auth.Identity{ID: "global", Roles: []string{auth.RoleRoomAdmin}}
	allowed, err := NewAuthorizer(globalStore).IsController(context.Background(), "room-a", global)
	if err != nil || !allowed {
		t.Fatalf("global room_admin = (%v, %v), want (true, nil)", allowed, err)
	}
	if globalStore.calls != 0 {
		t.Fatalf("global room_admin performed %d grant lookups, want 0", globalStore.calls)
	}

	grants := &fakeGrantStore{grants: map[grantKey]bool{
		{roomID: "room-a", principalID: "principal", capability: CapabilityController}: true,
	}}
	principal := auth.Identity{ID: "principal"}
	authorizer := NewAuthorizer(grants)
	allowed, err = authorizer.IsController(context.Background(), "room-a", principal)
	if err != nil || !allowed {
		t.Fatalf("room-a controller grant = (%v, %v), want (true, nil)", allowed, err)
	}
	allowed, err = authorizer.IsController(context.Background(), "room-b", principal)
	if err != nil || allowed {
		t.Fatalf("room-b controller grant = (%v, %v), want (false, nil)", allowed, err)
	}
}

type testRoomSource map[string]*room.Room

func (rooms testRoomSource) Get(id string) (*room.Room, error) {
	r, ok := rooms[id]
	if !ok {
		return nil, room.ErrRoomNotFound
	}
	return r, nil
}

type staticProvider struct{}

func (*staticProvider) ID() string { return "test" }
func (*staticProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*staticProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{
		Ref: ref, Title: ref.String(), DurationMs: int64(10 * time.Minute / time.Millisecond),
	}, nil
}
func (*staticProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	// 预检要求当前曲目可播；control 测试不真正拉流。
	return provider.StreamLocator{URL: "file:///nonexistent-in-test"}, nil
}

type staticTrackSource struct{}

func (staticTrackSource) NextBatch(context.Context, int, provider.TrackRef) ([]provider.Track, bool, error) {
	return nil, false, nil
}
func (staticTrackSource) Description() string { return "control test radio" }
func (staticTrackSource) Finite() bool        { return false }

func (*staticProvider) NewSource(context.Context, string) (provider.TrackSource, error) {
	return staticTrackSource{}, nil
}

func newServiceFixture(t *testing.T) (*Service, *fakeGrantStore) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	reg.Register(&staticProvider{})
	authm := auth.NewManager("", st)
	trackCache := cache.New(filepath.Join(root, "cache"), 1<<20, 0, st, reg)
	r := room.New("room-a", "Room A", "", "", st, authm, trackCache, reg)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() {
		cancel()
		st.Close()
	})
	grants := &fakeGrantStore{grants: map[grantKey]bool{}}
	return NewService(testRoomSource{"room-a": r}, reg, NewAuthorizer(grants)), grants
}

func TestServiceRequesterOwnershipAndControllers(t *testing.T) {
	service, grants := newServiceFixture(t)
	ctx := context.Background()
	owner := auth.Identity{
		ID: "owner", Name: "Owner", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	other := auth.Identity{
		ID: "other", Name: "Other", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}

	ids, err := service.QueueAdd(ctx, "room-a", owner, []string{"test:a", "test:b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("entry IDs = %v, want two", ids)
	}
	if err := service.QueueRemove(ctx, "room-a", other, ids[1]); !errors.Is(err, room.ErrForbidden) {
		t.Fatalf("non-owner remove error = %v, want forbidden", err)
	}
	if err := service.QueueRemove(ctx, "room-a", owner, ids[1]); err != nil {
		t.Fatalf("owner remove: %v", err)
	}

	if _, err := service.QueueAdd(ctx, "room-a", auth.Identity{ID: "listener"}, []string{"test:x"}); !errors.Is(err, room.ErrForbidden) {
		t.Fatalf("listener queue add error = %v, want forbidden", err)
	}

	moreIDs, err := service.QueueAdd(ctx, "room-a", owner, []string{"test:c", "test:d"})
	if err != nil {
		t.Fatal(err)
	}
	controller := auth.Identity{ID: "controller", Name: "Controller"}
	grants.grants[grantKey{
		roomID: "room-a", principalID: controller.ID, capability: CapabilityController,
	}] = true
	if err := service.QueueMove(ctx, "room-a", controller, moreIDs[1], 0); err != nil {
		t.Fatalf("room controller move: %v", err)
	}
	snapshot, err := service.RoomSnapshot(ctx, "room-a", controller)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Queue) != 2 || snapshot.Queue[0].EntryID != moreIDs[1] {
		t.Fatalf("queue after controller move = %#v", snapshot.Queue)
	}
	if err := service.QueueRemove(ctx, "room-a", controller, moreIDs[0]); err != nil {
		t.Fatalf("room controller remove any: %v", err)
	}

	global := auth.Identity{ID: "global", Roles: []string{auth.RoleRoomAdmin}}
	if err := service.Pause(ctx, "room-a", global); err != nil {
		t.Fatalf("global room_admin pause: %v", err)
	}
	if err := service.Resume(ctx, "room-a", controller); err != nil {
		t.Fatalf("room controller resume: %v", err)
	}
	if err := service.Pause(ctx, "room-b", controller); !errors.Is(err, room.ErrForbidden) {
		t.Fatalf("cross-room controller error = %v, want forbidden", err)
	}
}

func TestServiceRoomSnapshotRedactsProtectedRoomStreamURL(t *testing.T) {
	service, grants := newServiceFixture(t)
	ctx := context.Background()
	guest := auth.Identity{
		ID: "guest", Name: "Guest", Kind: "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	if _, err := service.QueueAdd(ctx, "room-a", guest, []string{"test:current"}); err != nil {
		t.Fatalf("seed playback: %v", err)
	}
	r, err := service.GetRoom("room-a")
	if err != nil {
		t.Fatal(err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("room-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyAccessConfig(room.AccessConfig{
		Mode: room.AccessModeStaticPassword, PasswordHash: string(passwordHash),
		TrustedRoles: []string{"staff"},
	}); err != nil {
		t.Fatalf("protect room: %v", err)
	}

	snapshot, err := service.RoomSnapshot(ctx, "room-a", guest)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Playback.Current == nil {
		t.Fatal("protected-room snapshot has no current track")
	}
	if got := snapshot.Playback.Current.StreamURL; got != "" {
		t.Fatalf("ordinary guest stream_url = %q, want redacted", got)
	}

	controller := auth.Identity{ID: "controller", Kind: "oidc"}
	grants.grants[grantKey{
		roomID: "room-a", principalID: controller.ID, capability: CapabilityController,
	}] = true
	snapshot, err = service.RoomSnapshot(ctx, "room-a", controller)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Playback.Current.StreamURL; got == "" {
		t.Fatal("room controller stream_url was redacted")
	}

	trustedOIDC := auth.Identity{ID: "staff-user", Kind: "oidc", Roles: []string{"staff"}}
	snapshot, err = service.RoomSnapshot(ctx, "room-a", trustedOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Playback.Current.StreamURL; got == "" {
		t.Fatal("trusted OIDC role stream_url was redacted")
	}

	if err := r.ApplyAccessConfig(room.AccessConfig{Mode: room.AccessModeOpen}); err != nil {
		t.Fatalf("open room: %v", err)
	}
	snapshot, err = service.RoomSnapshot(ctx, "room-a", guest)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Playback.Current.StreamURL; got == "" {
		t.Fatal("open-room guest stream_url was redacted")
	}
}

func TestServiceRadioControlPolicy(t *testing.T) {
	ctx := context.Background()
	requester := auth.Identity{
		ID: "requester", Name: "Requester",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}

	for _, policyRaw := range []string{"", `{"radio_control":"controller"}`} {
		t.Run("controller-only "+policyRaw, func(t *testing.T) {
			service, _ := newServiceFixture(t)
			if policyRaw != "" {
				r, err := service.GetRoom("room-a")
				if err != nil {
					t.Fatal(err)
				}
				if err := r.SetPolicy(policyRaw); err != nil {
					t.Fatalf("set policy: %v", err)
				}
			}
			err := service.RadioPlay(ctx, "room-a", requester, "test:radio", false, false)
			if !errors.Is(err, room.ErrForbidden) || err.Error() != "controller capability required" {
				t.Fatalf("requester radio play error = %v, want controller forbidden", err)
			}
		})
	}

	service, grants := newServiceFixture(t)
	r, err := service.GetRoom("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPolicy(`{"radio_control":"requester"}`); err != nil {
		t.Fatalf("set requester radio policy: %v", err)
	}
	if err := service.RadioPlay(ctx, "room-a", requester, "test:radio", false, false); err != nil {
		t.Fatalf("requester radio play: %v", err)
	}

	listener := auth.Identity{ID: "listener", Name: "Listener", Roles: []string{auth.RoleListener}}
	if err := service.RadioStop(ctx, "room-a", listener); !errors.Is(err, room.ErrForbidden) {
		t.Fatalf("listener radio stop error = %v, want forbidden", err)
	}
	if err := service.RadioPlay(ctx, "room-a", listener, "test:radio", false, false); !errors.Is(err, room.ErrForbidden) {
		t.Fatalf("listener radio play error = %v, want forbidden", err)
	}
	if err := service.RadioStop(ctx, "room-a", requester); err != nil {
		t.Fatalf("requester radio stop: %v", err)
	}

	controller := auth.Identity{ID: "controller", Name: "Controller"}
	grants.grants[grantKey{
		roomID: "room-a", principalID: controller.ID, capability: CapabilityController,
	}] = true
	if err := service.RadioPlay(ctx, "room-a", controller, "test:radio", false, false); err != nil {
		t.Fatalf("controller radio play under requester policy: %v", err)
	}
	if err := service.RadioStop(ctx, "room-a", controller); err != nil {
		t.Fatalf("controller radio stop under requester policy: %v", err)
	}
}

func TestServiceRoomCapabilitiesRadio(t *testing.T) {
	service, grants := newServiceFixture(t)
	ctx := context.Background()
	requester := auth.Identity{ID: "requester", Roles: []string{auth.RoleRequester}}
	listener := auth.Identity{ID: "listener", Roles: []string{auth.RoleListener}}
	controller := auth.Identity{ID: "controller"}

	capabilities, err := service.RoomCapabilities(ctx, "room-a", requester)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Controller || capabilities.Radio {
		t.Fatalf("default requester capabilities = %+v, want both false", capabilities)
	}

	r, err := service.GetRoom("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPolicy(`{"radio_control":"requester"}`); err != nil {
		t.Fatalf("set requester radio policy: %v", err)
	}

	capabilities, err = service.RoomCapabilities(ctx, "room-a", requester)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Controller || !capabilities.Radio {
		t.Fatalf("requester-policy requester capabilities = %+v, want controller=false radio=true", capabilities)
	}

	capabilities, err = service.RoomCapabilities(ctx, "room-a", listener)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Controller || capabilities.Radio {
		t.Fatalf("requester-policy listener capabilities = %+v, want both false", capabilities)
	}

	grants.grants[grantKey{
		roomID: "room-a", principalID: controller.ID, capability: CapabilityController,
	}] = true
	capabilities, err = service.RoomCapabilities(ctx, "room-a", controller)
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Controller || !capabilities.Radio {
		t.Fatalf("requester-policy controller capabilities = %+v, want both true", capabilities)
	}
}

func TestParsePolicyRejectsInvalidRadioControl(t *testing.T) {
	_, err := room.ParsePolicy(`{"radio_control":"everyone"}`)
	if !errors.Is(err, room.ErrInvalidPolicy) {
		t.Fatalf("invalid radio_control error = %v, want ErrInvalidPolicy", err)
	}
}

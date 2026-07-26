package control

import (
	"context"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

// CapabilityController is the room-scoped capability for queue ordering and playback control.
const CapabilityController = "controller"

// RoomGrantStore is the grant lookup required by the room authorizer.
type RoomGrantStore interface {
	HasRoomGrant(ctx context.Context, roomID, principalID, capability string) (bool, error)
}

// Authorizer is the single authority for room-scoped controller checks.
type Authorizer struct {
	grants RoomGrantStore
}

func NewAuthorizer(grants RoomGrantStore) *Authorizer {
	return &Authorizer{grants: grants}
}

// IsController applies the complete controller contract: a global room_admin role
// or a controller grant for this exact room and principal.
func (a *Authorizer) IsController(ctx context.Context, roomID string, principal auth.Identity) (bool, error) {
	if principal.HasRole(auth.RoleRoomAdmin) {
		return true, nil
	}
	if a == nil || a.grants == nil || principal.ID == "" {
		return false, nil
	}
	return a.grants.HasRoomGrant(ctx, roomID, principal.ID, CapabilityController)
}

func (a *Authorizer) RequireController(ctx context.Context, roomID string, principal auth.Identity) error {
	allowed, err := a.IsController(ctx, roomID, principal)
	if err != nil {
		return err
	}
	if !allowed {
		return forbidden("controller capability required")
	}
	return nil
}

type forbidden string

func (err forbidden) Error() string { return string(err) }
func (err forbidden) Unwrap() error { return room.ErrForbidden }

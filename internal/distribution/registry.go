package distribution

import (
	"context"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// Registry resolves acceleration-scoped machine credentials from persistent
// state. Pending inbound credentials are accepted during staged rotation.
type Registry struct {
	st *store.Store
}

func NewRegistry(st *store.Store) *Registry {
	return &Registry{st: st}
}

func (r *Registry) ResolvePublisher(ctx context.Context, token string) (store.Acceleration, error) {
	if r == nil || r.st == nil || token == "" {
		return store.Acceleration{}, ErrInvalidCredential
	}
	acceleration, err := r.st.ResolveAccelerationPublisherToken(ctx, HashCredential(token))
	if err != nil {
		return store.Acceleration{}, ErrInvalidCredential
	}
	return acceleration, nil
}

func (r *Registry) ResolveDelivery(ctx context.Context, token string) (store.Acceleration, error) {
	if r == nil || r.st == nil || token == "" {
		return store.Acceleration{}, ErrInvalidCredential
	}
	acceleration, err := r.st.ResolveAccelerationDeliveryToken(ctx, HashCredential(token))
	if err != nil {
		return store.Acceleration{}, ErrInvalidCredential
	}
	return acceleration, nil
}

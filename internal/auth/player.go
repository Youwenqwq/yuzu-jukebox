package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var ErrInvalidPlayerCredentials = errors.New("invalid player credentials")

type PlayerRegistry struct {
	st *store.Store
}

func NewPlayerRegistry(st *store.Store) *PlayerRegistry {
	return &PlayerRegistry{st: st}
}

func NewPlayerKey() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	key := "yzp_" + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(key))
	return key, digest[:], nil
}

func HashPlayerKey(key string) []byte {
	digest := sha256.Sum256([]byte(key))
	return digest[:]
}

func (r *PlayerRegistry) ResolveKey(ctx context.Context, key string) (store.Player, error) {
	player, err := r.ValidateKey(ctx, key)
	if err != nil {
		return store.Player{}, err
	}
	if err := r.st.TouchPlayer(ctx, player.ID, time.Now().UnixMilli()); err != nil {
		return store.Player{}, ErrInvalidPlayerCredentials
	}
	return player, nil
}

func (r *PlayerRegistry) ValidateKey(ctx context.Context, key string) (store.Player, error) {
	if r == nil || r.st == nil || key == "" {
		return store.Player{}, ErrInvalidPlayerCredentials
	}
	player, err := r.st.ResolvePlayerToken(ctx, HashPlayerKey(key))
	if err != nil {
		return store.Player{}, ErrInvalidPlayerCredentials
	}
	return player, nil
}

func PlayerIdentity(player store.Player) Identity {
	return Identity{
		ID: playerIdentityID(player.ID), Name: player.Name, Kind: "player",
		PlayerID: player.ID,
	}
}

func playerIdentityID(playerID string) string {
	digest := sha256.Sum256([]byte("player:" + playerID))
	return "pl_" + base64.RawURLEncoding.EncodeToString(digest[:12])
}

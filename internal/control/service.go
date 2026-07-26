// Package control provides the authenticated command and query boundary for room state.
package control

import (
	"context"
	"errors"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

var (
	// ErrInvalidArgument classifies caller input that cannot form a room command.
	ErrInvalidArgument = errors.New("invalid control argument")
	// ErrProvider classifies a provider failure while materializing a queue request.
	ErrProvider = errors.New("provider error")
)

type roomSource interface {
	Get(id string) (*room.Room, error)
}

// Service is the reusable server-side boundary for room commands and state queries.
type Service struct {
	rooms      roomSource
	providers  *provider.Registry
	authorizer *Authorizer
}

func NewService(rooms roomSource, providers *provider.Registry, authorizer *Authorizer) *Service {
	return &Service{rooms: rooms, providers: providers, authorizer: authorizer}
}

// GetRoom resolves a live room actor. Connection adapters use the result only for
// listener membership; all room commands remain methods on Service.
func (s *Service) GetRoom(roomID string) (*room.Room, error) {
	return s.rooms.Get(roomID)
}

// RoomSnapshot returns a complete, identity-specific state projection without
// joining the caller to the room listener set.
func (s *Service) RoomSnapshot(ctx context.Context, roomID string, principal auth.Identity) (room.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return room.Snapshot{}, err
	}
	r, err := s.GetRoom(roomID)
	if err != nil {
		return room.Snapshot{}, err
	}
	return r.Snapshot(principal)
}

// QueueAdd atomically materializes and appends all requested tracks.
func (s *Service) QueueAdd(ctx context.Context, roomID string, principal auth.Identity, trackRefs []string) ([]string, error) {
	if !principal.HasRole(auth.RoleRequester) {
		return nil, forbidden("role required: " + auth.RoleRequester)
	}
	if len(trackRefs) == 0 {
		return nil, classify(ErrInvalidArgument, errors.New("track_ref or track_refs required"))
	}
	if len(trackRefs) > 100 {
		return nil, classify(ErrInvalidArgument, errors.New("track_refs limited to 100"))
	}
	r, err := s.GetRoom(roomID)
	if err != nil {
		return nil, err
	}

	entries := make([]room.QueueEntry, 0, len(trackRefs))
	for _, rawRef := range trackRefs {
		ref := provider.TrackRef(rawRef)
		p, _, err := s.providers.ForRef(ref)
		if err != nil {
			return nil, classify(ErrInvalidArgument, err)
		}
		track, err := p.GetTrack(ctx, ref)
		if err != nil {
			return nil, classify(ErrProvider, err)
		}
		entries = append(entries, room.EntryFromTrack(track, principal.ID))
	}
	if err := r.AddBatchFor(principal, entries); err != nil {
		return nil, err
	}
	entryIDs := make([]string, len(entries))
	for i := range entries {
		entryIDs[i] = entries[i].EntryID
	}
	return entryIDs, nil
}

// QueueRemove preserves owner removal while allowing an authorized controller to
// remove any pending entry. Ownership remains an atomic room-actor decision.
func (s *Service) QueueRemove(ctx context.Context, roomID string, principal auth.Identity, entryID string) error {
	r, err := s.GetRoom(roomID)
	if err != nil {
		return err
	}
	ownerErr := r.RemoveFor(principal, entryID)
	if !errors.Is(ownerErr, room.ErrForbidden) {
		return ownerErr
	}
	allowed, err := s.authorizer.IsController(ctx, roomID, principal)
	if err != nil {
		return err
	}
	if !allowed {
		return ownerErr
	}
	return r.Remove(entryID)
}

func (s *Service) QueueMove(ctx context.Context, roomID string, principal auth.Identity, entryID string, toIndex int) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	err = r.Move(entryID, toIndex)
	if errors.Is(err, room.ErrInvalidQueueIndex) {
		return classify(ErrInvalidArgument, err)
	}
	return err
}

func (s *Service) Pause(ctx context.Context, roomID string, principal auth.Identity) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.Pause()
}

func (s *Service) Resume(ctx context.Context, roomID string, principal auth.Identity) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.Resume()
}

func (s *Service) Skip(ctx context.Context, roomID string, principal auth.Identity) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.Skip()
}

func (s *Service) Seek(ctx context.Context, roomID string, principal auth.Identity, positionMs int64) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.SeekTo(positionMs)
}

func (s *Service) RadioPlay(ctx context.Context, roomID string, principal auth.Identity, source string, shuffle, once bool) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.PlayRadio(source, shuffle, once)
}

func (s *Service) RadioStop(ctx context.Context, roomID string, principal auth.Identity) error {
	r, err := s.controlledRoom(ctx, roomID, principal)
	if err != nil {
		return err
	}
	return r.StopRadio()
}

func (s *Service) controlledRoom(ctx context.Context, roomID string, principal auth.Identity) (*room.Room, error) {
	if err := s.authorizer.RequireController(ctx, roomID, principal); err != nil {
		return nil, err
	}
	return s.GetRoom(roomID)
}

type classifiedError struct {
	class error
	cause error
}

func classify(class, cause error) error {
	return classifiedError{class: class, cause: cause}
}

func (err classifiedError) Error() string { return err.cause.Error() }
func (err classifiedError) Unwrap() error { return err.cause }
func (err classifiedError) Is(target error) bool {
	return target == err.class || errors.Is(err.cause, target)
}

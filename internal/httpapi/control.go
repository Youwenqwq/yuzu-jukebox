package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

const maxControlBodyBytes = 1 << 20

func (s *Server) roomCapabilities(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	capabilities, err := s.controls.RoomCapabilities(r.Context(), roomID, identity)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Capabilities control.RoomCapabilities `json:"capabilities"`
	}{Capabilities: capabilities})
}

func (s *Server) roomState(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}

	snapshot, err := s.controls.RoomSnapshot(r.Context(), roomID, identity)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) queueAdd(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		TrackRef  string   `json:"track_ref"`
		TrackRefs []string `json:"track_refs"`
	}
	if err := decodeControlJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid queue payload")
		return
	}

	var refs []string
	switch {
	case body.TrackRef != "" && len(body.TrackRefs) > 0:
		writeErr(w, http.StatusBadRequest, "bad_request", "track_ref and track_refs are mutually exclusive")
		return
	case body.TrackRef != "":
		refs = []string{body.TrackRef}
	case len(body.TrackRefs) > 0:
		refs = body.TrackRefs
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "track_ref or track_refs required")
		return
	}
	if len(refs) > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "track_refs limited to 100")
		return
	}

	entryIDs, err := s.controls.QueueAdd(r.Context(), roomID, identity, refs)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		EntryIDs []string `json:"entry_ids"`
	}{EntryIDs: entryIDs})
}

func (s *Server) queueRemove(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	entryID, ok := controlPathValue(w, r, "entry_id")
	if !ok {
		return
	}

	if err := s.controls.QueueRemove(r.Context(), roomID, identity, entryID); err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) queueMove(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	entryID, ok := controlPathValue(w, r, "entry_id")
	if !ok {
		return
	}
	var body struct {
		ToIndex *int `json:"to_index"`
	}
	if err := decodeControlJSON(r, &body); err != nil || body.ToIndex == nil || *body.ToIndex < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "to_index must be a non-negative integer")
		return
	}

	if err := s.controls.QueueMove(r.Context(), roomID, identity, entryID, *body.ToIndex); err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) playbackControl(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}

	var err error
	switch r.PathValue("op") {
	case "pause", "resume", "skip":
		if decodeErr := decodeOptionalEmptyControlJSON(r); decodeErr != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "playback operation does not accept a payload")
			return
		}
		switch r.PathValue("op") {
		case "pause":
			err = s.controls.Pause(r.Context(), roomID, identity)
		case "resume":
			err = s.controls.Resume(r.Context(), roomID, identity)
		case "skip":
			err = s.controls.Skip(r.Context(), roomID, identity)
		}
	case "seek":
		var body struct {
			PositionMs *int64 `json:"position_ms"`
		}
		if decodeErr := decodeControlJSON(r, &body); decodeErr != nil || body.PositionMs == nil || *body.PositionMs < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "position_ms must be a non-negative integer")
			return
		}
		err = s.controls.Seek(r.Context(), roomID, identity, *body.PositionMs)
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown playback operation")
		return
	}
	if err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) radioPlay(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Source  string `json:"source"`
		Shuffle bool   `json:"shuffle"`
		Once    bool   `json:"once"`
	}
	if err := decodeControlJSON(r, &body); err != nil || strings.TrimSpace(body.Source) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "source required")
		return
	}

	if err := s.controls.RadioPlay(r.Context(), roomID, identity, body.Source, body.Shuffle, body.Once); err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) radioStop(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.controlIdentity(w, r)
	if !ok {
		return
	}
	roomID, ok := controlPathValue(w, r, "id")
	if !ok {
		return
	}

	if err := s.controls.RadioStop(r.Context(), roomID, identity); err != nil {
		writeControlErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) controlIdentity(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	identity, err := s.authenticate(r)
	if errors.Is(err, auth.ErrSessionNotFound) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
		return auth.Identity{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal server error")
		return auth.Identity{}, false
	}
	return identity, true
}

func controlPathValue(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := r.PathValue(name)
	if strings.TrimSpace(value) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", name+" required")
		return "", false
	}
	return value, true
}

func newControlJSONDecoder(r *http.Request) *json.Decoder {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxControlBodyBytes))
	decoder.DisallowUnknownFields()
	return decoder
}

func decodeControlJSON(r *http.Request, dst any) error {
	decoder := newControlJSONDecoder(r)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	return requireControlJSONEnd(decoder)
}

func decodeOptionalEmptyControlJSON(r *http.Request) error {
	decoder := newControlJSONDecoder(r)
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return requireControlJSONEnd(decoder)
}

func requireControlJSONEnd(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeControlErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrInvalidArgument),
		errors.Is(err, room.ErrInvalidSource),
		errors.Is(err, room.ErrInvalidPolicy):
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, room.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, room.ErrRoomNotFound),
		errors.Is(err, room.ErrRoomClosed),
		errors.Is(err, room.ErrEntryNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, room.ErrQueueFull),
		errors.Is(err, room.ErrQuotaExceeded),
		errors.Is(err, room.ErrQueueEmpty),
		errors.Is(err, room.ErrNoPlayback):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, control.ErrProvider):
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

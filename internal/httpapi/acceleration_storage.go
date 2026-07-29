package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func (s *Server) accelerationReserve(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner     string `json:"owner"`
		Locator   string `json:"locator"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	reservation, err := s.st.ReserveAccelerationStorage(r.Context(), r.PathValue("id"),
		strings.TrimSpace(body.Owner), body.Locator, body.SizeBytes, time.Now().UnixMilli())
	switch {
	case errors.Is(err, store.ErrAccelerationStorageFull):
		writeErr(w, http.StatusInsufficientStorage, "acceleration_storage_full", "acceleration storage budget exceeded")
		return
	case errors.Is(err, store.ErrAccelerationStorageUnmanaged):
		writeErr(w, http.StatusConflict, "acceleration_storage_unmanaged", "acceleration storage budget is not configured")
		return
	case errors.Is(err, store.ErrStorageReservationInProgress):
		writeErr(w, http.StatusConflict, "acceleration_storage_reserved", "storage locator is already being published")
		return
	case errors.Is(err, store.ErrStorageReservationInvalid):
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case err != nil:
		writeDistributionLeaseError(w, err)
		return
	}
	if reservation.AccelerationID != acceleration.ID {
		writeErr(w, http.StatusForbidden, "forbidden", "lease belongs to another acceleration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservation": reservation})
}

func (s *Server) accelerationInventory(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner      string                         `json:"owner"`
		SnapshotID string                         `json:"snapshot_id"`
		ObservedAt int64                          `json:"observed_at"`
		Objects    []store.StorageInventoryObject `json:"objects"`
		Complete   bool                           `json:"complete"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if len(body.Objects) > 1000 {
		writeErr(w, http.StatusBadRequest, "bad_request", "inventory batch exceeds 1000 objects")
		return
	}
	if err := s.st.AppendAccelerationInventory(r.Context(), acceleration.ID,
		strings.TrimSpace(body.Owner), strings.TrimSpace(body.SnapshotID), body.ObservedAt,
		body.Objects, body.Complete, time.Now().UnixMilli()); err != nil {
		if errors.Is(err, store.ErrStorageInventoryInvalid) {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to record storage inventory")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accelerationDeletionClaim(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner        string `json:"owner"`
		LeaseSeconds int    `json:"lease_seconds"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	deletion, err := s.st.ClaimAccelerationDeletion(r.Context(), acceleration.ID,
		strings.TrimSpace(body.Owner), time.Duration(body.LeaseSeconds)*time.Second,
		time.Now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, store.ErrStorageDeletionInvalid) {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to claim storage deletion")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deletion": deletion})
}

func (s *Server) accelerationDeletionComplete(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if err := s.st.CompleteAccelerationDeletion(r.Context(), acceleration.ID,
		r.PathValue("id"), strings.TrimSpace(body.Owner), time.Now().UnixMilli()); err != nil {
		if errors.Is(err, store.ErrStorageDeletionInvalid) {
			writeErr(w, http.StatusConflict, "deletion_invalid", err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to complete storage deletion")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) accelerationDeletionFail(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner             string `json:"owner"`
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if body.RetryAfterSeconds < 0 || body.RetryAfterSeconds > 3600 {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid retry_after_seconds")
		return
	}
	now := time.Now().UnixMilli()
	if err := s.st.FailAccelerationDeletion(r.Context(), acceleration.ID,
		r.PathValue("id"), strings.TrimSpace(body.Owner), body.Error,
		now+int64(body.RetryAfterSeconds)*1000, now); err != nil {
		if errors.Is(err, store.ErrStorageDeletionInvalid) {
			writeErr(w, http.StatusConflict, "deletion_invalid", err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to record storage deletion failure")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

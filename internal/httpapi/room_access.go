package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type roomAccessResponse struct {
	Mode              room.AccessMode `json:"mode"`
	CodePeriodSeconds int64           `json:"code_period_seconds,omitempty"`
	TrustedRoles      []string        `json:"trusted_roles"`
}

func createRoomAccessConfig(modeValue, password string, periodSeconds int64, trustedRoles []string) (room.AccessConfig, error) {
	mode := room.AccessModeOpen
	var err error
	if modeValue != "" {
		mode, err = room.ParseAccessMode(modeValue)
		if err != nil {
			return room.AccessConfig{}, err
		}
	} else if password != "" {
		mode = room.AccessModeStaticPassword
	}
	if periodSeconds == 0 {
		periodSeconds = room.DefaultCodePeriodSeconds
	}
	normalizedRoles, err := room.NormalizeTrustedRoles(trustedRoles)
	if err != nil {
		return room.AccessConfig{}, err
	}
	config := room.AccessConfig{
		Mode: mode, CodePeriodSeconds: periodSeconds, TrustedRoles: normalizedRoles,
	}
	switch mode {
	case room.AccessModeOpen, room.AccessModeRotatingCode:
		if password != "" {
			return room.AccessConfig{}, fmt.Errorf("guest_password is only valid for static_password mode")
		}
	case room.AccessModeStaticPassword:
		if password == "" {
			return room.AccessConfig{}, room.ErrStaticPasswordEmpty
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return room.AccessConfig{}, fmt.Errorf("invalid guest_password: %w", err)
		}
		config.PasswordHash = string(hash)
	}
	return config, nil
}

func updateRoomAccessConfig(
	row store.Room,
	modeValue *string,
	password *string,
	periodSeconds *int64,
	trustedRoles *[]string,
) (room.AccessConfig, error) {
	mode := room.AccessMode(row.AccessMode)
	if mode == "" {
		mode = room.AccessModeOpen
		if row.PasswordHash != "" {
			mode = room.AccessModeStaticPassword
		}
	}
	period := row.CodePeriodSeconds
	if period == 0 {
		period = room.DefaultCodePeriodSeconds
	}
	config := room.AccessConfig{
		Mode: mode, PasswordHash: row.PasswordHash, CodePeriodSeconds: period,
		TrustedRoles: append([]string(nil), row.TrustedRoles...),
	}
	if modeValue != nil {
		parsed, err := room.ParseAccessMode(*modeValue)
		if err != nil {
			return room.AccessConfig{}, err
		}
		config.Mode = parsed
	}
	if periodSeconds != nil {
		config.CodePeriodSeconds = *periodSeconds
	}
	if trustedRoles != nil {
		config.TrustedRoles = append([]string(nil), (*trustedRoles)...)
	}
	normalizedRoles, err := room.NormalizeTrustedRoles(config.TrustedRoles)
	if err != nil {
		return room.AccessConfig{}, err
	}
	config.TrustedRoles = normalizedRoles

	if password != nil {
		if modeValue == nil {
			if *password == "" {
				config.Mode = room.AccessModeOpen
				config.PasswordHash = ""
				return config, nil
			}
			config.Mode = room.AccessModeStaticPassword
		}
		if config.Mode != room.AccessModeStaticPassword {
			if *password != "" {
				return room.AccessConfig{}, fmt.Errorf("guest_password is only valid for static_password mode")
			}
			config.PasswordHash = ""
		} else {
			if *password == "" {
				return room.AccessConfig{}, room.ErrStaticPasswordEmpty
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
			if err != nil {
				return room.AccessConfig{}, fmt.Errorf("invalid guest_password: %w", err)
			}
			config.PasswordHash = string(hash)
		}
	}
	if config.Mode != room.AccessModeStaticPassword {
		config.PasswordHash = ""
	} else if config.PasswordHash == "" {
		return room.AccessConfig{}, room.ErrStaticPasswordEmpty
	}
	return config, nil
}

func accessResponse(config room.AccessConfig) roomAccessResponse {
	response := roomAccessResponse{
		Mode: config.Mode, TrustedRoles: append([]string{}, config.TrustedRoles...),
	}
	if config.Mode == room.AccessModeRotatingCode {
		response.CodePeriodSeconds = config.CodePeriodSeconds
	}
	return response
}

func (s *Server) getRoomAccessCode(w http.ResponseWriter, r *http.Request) {
	identity, err := s.authenticate(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "login first")
		return
	}
	roomID := r.PathValue("id")
	if !identity.HasRole(auth.RoleRoomAdmin) &&
		(identity.IntegrationID == "" || identity.IntegrationRoomID != roomID) {
		writeErr(w, http.StatusForbidden, "forbidden", "room access code is unavailable to this session")
		return
	}
	rm, err := s.rooms.Get(roomID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	code, err := rm.CurrentAccessCode()
	switch {
	case errors.Is(err, room.ErrAccessCodeDisabled):
		writeErr(w, http.StatusConflict, "conflict", "rotating room code is not enabled")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.audit(r.Context(), identity.ID, "room.access_code.read", roomID, map[string]any{
		"expires_at": code.ExpiresAt,
	})
	writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "access_code": code})
}

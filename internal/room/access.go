package room

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/shortcode"
	"golang.org/x/crypto/bcrypt"
)

type AccessMode string

const (
	AccessModeOpen           AccessMode = "open"
	AccessModeStaticPassword AccessMode = "static_password"
	AccessModeRotatingCode   AccessMode = "rotating_code"

	DefaultCodePeriodSeconds int64 = 24 * 60 * 60
	MinCodePeriodSeconds     int64 = 60
	MaxCodePeriodSeconds     int64 = 30 * 24 * 60 * 60

	previousCodeGraceSeconds int64 = 15
)

var (
	ErrInvalidAccessMode   = errors.New("invalid room access mode")
	ErrInvalidCodePeriod   = errors.New("room code period must be between 60 and 2592000 seconds")
	ErrStaticPasswordEmpty = errors.New("static room password required")
	ErrAccessCodeDisabled  = errors.New("rotating room code is not enabled")
	ErrAccessCodeKeyAbsent = errors.New("server secret_key is required for rotating room codes")
)

type AccessConfig struct {
	Mode              AccessMode
	PasswordHash      string
	CodePeriodSeconds int64
}

type AccessCode struct {
	Code          string `json:"code"`
	PeriodSeconds int64  `json:"period_seconds"`
	ValidFrom     int64  `json:"valid_from"`
	ExpiresAt     int64  `json:"expires_at"`
}

type roomAccess struct {
	roomID    string
	createdAt int64
	key       []byte
	config    atomic.Pointer[AccessConfig]
}

func newRoomAccess(roomID string, createdAt int64, key []byte, config AccessConfig) *roomAccess {
	a := &roomAccess{roomID: roomID, createdAt: createdAt, key: key}
	a.store(config)
	return a
}

func ValidateAccessConfig(config AccessConfig, hasCodeKey bool) error {
	if config.CodePeriodSeconds == 0 {
		config.CodePeriodSeconds = DefaultCodePeriodSeconds
	}
	switch config.Mode {
	case AccessModeOpen:
		return nil
	case AccessModeStaticPassword:
		if config.PasswordHash == "" {
			return ErrStaticPasswordEmpty
		}
		return nil
	case AccessModeRotatingCode:
		if !hasCodeKey {
			return ErrAccessCodeKeyAbsent
		}
		if config.CodePeriodSeconds < MinCodePeriodSeconds || config.CodePeriodSeconds > MaxCodePeriodSeconds {
			return ErrInvalidCodePeriod
		}
		return nil
	default:
		return ErrInvalidAccessMode
	}
}

func (a *roomAccess) store(config AccessConfig) {
	if config.CodePeriodSeconds == 0 {
		config.CodePeriodSeconds = DefaultCodePeriodSeconds
	}
	if config.Mode != AccessModeStaticPassword {
		config.PasswordHash = ""
	}
	copy := config
	a.config.Store(&copy)
}

func (a *roomAccess) load() AccessConfig {
	return *a.config.Load()
}

func (a *roomAccess) set(config AccessConfig) error {
	if err := ValidateAccessConfig(config, len(a.key) > 0); err != nil {
		return err
	}
	a.store(config)
	return nil
}

func (a *roomAccess) check(credential string, now time.Time) bool {
	config := a.load()
	switch config.Mode {
	case AccessModeOpen:
		return true
	case AccessModeStaticPassword:
		return bcrypt.CompareHashAndPassword([]byte(config.PasswordHash), []byte(credential)) == nil
	case AccessModeRotatingCode:
		candidate, ok := shortcode.Normalize(credential)
		if !ok || len(a.key) == 0 {
			return false
		}
		current, _, validFrom := a.codeAt(config.CodePeriodSeconds, now)
		if hmac.Equal([]byte(candidate), []byte(current)) {
			return true
		}
		if now.Unix()-validFrom >= previousCodeGraceSeconds {
			return false
		}
		previous, _, _ := a.codeAt(config.CodePeriodSeconds, time.Unix(validFrom-1, 0))
		return hmac.Equal([]byte(candidate), []byte(previous))
	default:
		return false
	}
}

func (a *roomAccess) currentCode(now time.Time) (AccessCode, error) {
	config := a.load()
	if config.Mode != AccessModeRotatingCode {
		return AccessCode{}, ErrAccessCodeDisabled
	}
	if len(a.key) == 0 {
		return AccessCode{}, ErrAccessCodeKeyAbsent
	}
	canonical, expiresAt, validFrom := a.codeAt(config.CodePeriodSeconds, now)
	display, _ := shortcode.Format(canonical)
	return AccessCode{
		Code:          display,
		PeriodSeconds: config.CodePeriodSeconds,
		ValidFrom:     validFrom * 1000,
		ExpiresAt:     expiresAt * 1000,
	}, nil
}

func (a *roomAccess) codeAt(periodSeconds int64, now time.Time) (canonical string, expiresAt, validFrom int64) {
	counter := now.Unix() / periodSeconds
	validFrom = counter * periodSeconds
	expiresAt = validFrom + periodSeconds

	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte("yuzu-room-access-v1\x00"))
	_, _ = mac.Write([]byte(a.roomID))
	_, _ = mac.Write([]byte{0})
	var fields [24]byte
	binary.BigEndian.PutUint64(fields[0:8], uint64(a.createdAt))
	binary.BigEndian.PutUint64(fields[8:16], uint64(periodSeconds))
	binary.BigEndian.PutUint64(fields[16:24], uint64(counter))
	_, _ = mac.Write(fields[:])
	digest := mac.Sum(nil)
	canonical, _ = shortcode.Encode(digest)
	return canonical, expiresAt, validFrom
}

func ParseAccessMode(value string) (AccessMode, error) {
	mode := AccessMode(strings.TrimSpace(value))
	switch mode {
	case AccessModeOpen, AccessModeStaticPassword, AccessModeRotatingCode:
		return mode, nil
	default:
		return "", ErrInvalidAccessMode
	}
}

package room

import (
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestRotatingAccessCodePeriodAndBoundaryGrace(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	access := newRoomAccess("daily", 1720000000000, key, AccessConfig{
		Mode: AccessModeRotatingCode, CodePeriodSeconds: DefaultCodePeriodSeconds,
	})
	now := time.Date(2026, time.July, 28, 14, 0, 0, 0, time.UTC)
	current, err := access.currentCode(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Code) != 14 || current.PeriodSeconds != 86400 || current.ExpiresAt-current.ValidFrom != 86400000 {
		t.Fatalf("current code = %#v", current)
	}
	if !access.check(current.Code, time.UnixMilli(current.ExpiresAt-1)) {
		t.Fatal("current daily code was rejected within its period")
	}
	next, err := access.currentCode(time.UnixMilli(current.ExpiresAt))
	if err != nil {
		t.Fatal(err)
	}
	if next.Code == current.Code {
		t.Fatal("code did not rotate at the period boundary")
	}
	boundary := time.UnixMilli(current.ExpiresAt)
	if !access.check(current.Code, boundary.Add(14*time.Second)) {
		t.Fatal("previous code was rejected during boundary grace")
	}
	if access.check(current.Code, boundary.Add(15*time.Second)) {
		t.Fatal("previous code remained valid after boundary grace")
	}
	if access.check("0000-0000-0000", now) {
		t.Fatal("unrelated code was accepted")
	}
}

func TestRoomAccessModes(t *testing.T) {
	open := newRoomAccess("open", 1, nil, AccessConfig{Mode: AccessModeOpen})
	if !open.check("", time.Now()) {
		t.Fatal("open Room rejected admission")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("room-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	static := newRoomAccess("static", 1, nil, AccessConfig{
		Mode: AccessModeStaticPassword, PasswordHash: string(hash),
	})
	if !static.check("room-secret", time.Now()) || static.check("wrong", time.Now()) {
		t.Fatal("static password validation mismatch")
	}
	if err := ValidateAccessConfig(AccessConfig{
		Mode: AccessModeRotatingCode, CodePeriodSeconds: DefaultCodePeriodSeconds,
	}, false); err != ErrAccessCodeKeyAbsent {
		t.Fatalf("missing key error = %v", err)
	}
}

package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestGuestAuthValidatesName(t *testing.T) {
	tests := []struct {
		name    string
		guest   string
		wantErr bool
	}{
		{name: "empty", guest: "", wantErr: true},
		{name: "65 bytes", guest: strings.Repeat("a", 65), wantErr: true},
		{name: "newline", guest: "line\nbreak", wantErr: true},
		{name: "delete", guest: "bad\x7fname", wantErr: true},
		{name: "64 bytes", guest: strings.Repeat("a", 64)},
		{name: "unicode", guest: "阿柚"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, _, err := NewManager("", nil).GuestAuth(test.guest, "", "198.51.100.4:5000")
			if test.wantErr {
				if !errors.Is(err, ErrInvalidGuestName) {
					t.Fatalf("GuestAuth error = %v, want ErrInvalidGuestName", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GuestAuth error = %v", err)
			}
			if identity.Name != test.guest {
				t.Fatalf("identity name = %q, want %q", identity.Name, test.guest)
			}
		})
	}
}

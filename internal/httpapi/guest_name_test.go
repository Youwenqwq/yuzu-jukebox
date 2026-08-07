package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

func TestGuestAuthInvalidNameResponse(t *testing.T) {
	for _, test := range []struct {
		name  string
		guest string
	}{
		{name: "oversized", guest: strings.Repeat("a", 70)},
		{name: "control character", guest: "line\nbreak"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{authm: auth.NewManager("", nil)}
			response := performGuestAuthName(t, s, test.guest, "", "198.51.100.9:3000")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "invalid_name" {
				t.Fatalf("error code = %q, want invalid_name", body.Error.Code)
			}
		})
	}
}

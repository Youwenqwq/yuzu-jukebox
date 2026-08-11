package provider

import (
	"encoding/json"
	"testing"
)

func TestContributorEntityIDJSON(t *testing.T) {
	tests := []struct {
		name string
		in   Contributor
		want string
	}{
		{
			name: "provider entity id",
			in:   Contributor{Role: "artist", Name: "Artist", EntityID: "123"},
			want: `{"role":"artist","name":"Artist","entity_id":"123"}`,
		},
		{
			name: "legacy contributor omits empty entity id",
			in:   Contributor{Role: "artist", Name: "Artist"},
			want: `{"role":"artist","name":"Artist"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("json.Marshal() = %s, want %s", got, tt.want)
			}
		})
	}
}

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type restExpectation struct {
	method     string
	requestURI string
	token      string
	body       any
	response   string
}

func expectREST(t *testing.T, want restExpectation) *httptest.Server {
	t.Helper()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != want.method {
			t.Errorf("method = %q, want %q", r.Method, want.method)
		}
		if r.RequestURI != want.requestURI {
			t.Errorf("request URI = %q, want %q", r.RequestURI, want.requestURI)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+want.token {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+want.token)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if want.body == nil {
			if len(data) != 0 {
				t.Errorf("body = %s, want empty", data)
			}
		} else {
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var gotBody any
			if err := json.Unmarshal(data, &gotBody); err != nil {
				t.Errorf("decode body %q: %v", data, err)
			}
			wantData, err := json.Marshal(want.body)
			if err != nil {
				t.Fatal(err)
			}
			var wantBody any
			if err := json.Unmarshal(wantData, &wantBody); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotBody, wantBody) {
				t.Errorf("body = %#v, want %#v", gotBody, wantBody)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, want.response)
	}))
	t.Cleanup(func() {
		server.Close()
		if !called {
			t.Error("expected REST request was not made")
		}
	})
	return server
}

func TestRESTRoomState(t *testing.T) {
	server := expectREST(t, restExpectation{
		method:     http.MethodGet,
		requestURI: "/api/v1/rooms/room%2Fa%20b/state",
		token:      "actor-token",
		response:   `{"playback":{"current":null,"position_ms":1250,"updated_at":99,"playing":true,"rate":1},"queue":[{"entry_id":"entry-1","track_ref":"local:one","title":"One"}],"radio":null,"listeners":[{"id":"listener-1","name":"Listener"}]}`,
	})

	state, err := RESTRoomState(context.Background(), server.URL, "actor-token", "room/a b")
	if err != nil {
		t.Fatal(err)
	}
	if state.Playback.PositionMs != 1250 || !state.Playback.Playing {
		t.Fatalf("playback = %#v", state.Playback)
	}
	if len(state.Queue) != 1 || state.Queue[0].EntryID != "entry-1" {
		t.Fatalf("queue = %#v", state.Queue)
	}
	if len(state.Listeners) != 1 || state.Listeners[0].Name != "Listener" {
		t.Fatalf("listeners = %#v", state.Listeners)
	}
}

func TestRESTRoomCapabilities(t *testing.T) {
	server := expectREST(t, restExpectation{
		method:     http.MethodGet,
		requestURI: "/api/v1/rooms/room%2Fa%20b/capabilities",
		token:      "actor-token",
		response:   `{"capabilities":{"controller":true}}`,
	})
	capabilities, err := RESTRoomCapabilities(context.Background(), server.URL, "actor-token", "room/a b")
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Controller {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}

func TestRESTRoomQueue(t *testing.T) {
	t.Run("add one", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodPost,
			requestURI: "/api/v1/rooms/room%2Fone/queue",
			token:      "actor-token",
			body:       RoomQueueAddRequest{TrackRef: "provider:track/1"},
			response:   `{"entry_ids":["entry-1"]}`,
		})
		entryID, err := RESTRoomQueueAdd(context.Background(), server.URL, "actor-token", "room/one", "provider:track/1")
		if err != nil {
			t.Fatal(err)
		}
		if entryID != "entry-1" {
			t.Fatalf("entry ID = %q, want entry-1", entryID)
		}
	})

	t.Run("add many", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodPost,
			requestURI: "/api/v1/rooms/room%2Fone/queue",
			token:      "actor-token",
			body:       RoomQueueAddRequest{TrackRefs: []string{"provider:one", "provider:two"}},
			response:   `{"entry_ids":["entry-1","entry-2"]}`,
		})
		entryIDs, err := RESTRoomQueueAddMany(context.Background(), server.URL, "actor-token", "room/one", []string{"provider:one", "provider:two"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(entryIDs, []string{"entry-1", "entry-2"}) {
			t.Fatalf("entry IDs = %#v", entryIDs)
		}
	})

	t.Run("remove", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodDelete,
			requestURI: "/api/v1/rooms/room%2Fone/queue/entry%2F1%20x",
			token:      "actor-token",
			response:   `{}`,
		})
		if err := RESTRoomQueueRemove(context.Background(), server.URL, "actor-token", "room/one", "entry/1 x"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("move", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodPatch,
			requestURI: "/api/v1/rooms/room%2Fone/queue/entry%2F1",
			token:      "actor-token",
			body:       RoomQueueMoveRequest{ToIndex: 3},
			response:   `{}`,
		})
		if err := RESTRoomQueueMove(context.Background(), server.URL, "actor-token", "room/one", "entry/1", 3); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRESTRoomPlayback(t *testing.T) {
	tests := []struct {
		name string
		op   string
		body any
		call func(string) error
	}{
		{name: "pause", op: "pause", call: func(server string) error {
			return RESTRoomPlaybackPause(context.Background(), server, "actor-token", "room/one")
		}},
		{name: "resume", op: "resume", call: func(server string) error {
			return RESTRoomPlaybackResume(context.Background(), server, "actor-token", "room/one")
		}},
		{name: "skip", op: "skip", call: func(server string) error {
			return RESTRoomPlaybackSkip(context.Background(), server, "actor-token", "room/one")
		}},
		{name: "seek", op: "seek", body: RoomPlaybackSeekRequest{PositionMs: 4321}, call: func(server string) error {
			return RESTRoomPlaybackSeek(context.Background(), server, "actor-token", "room/one", 4321)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := expectREST(t, restExpectation{
				method:     http.MethodPost,
				requestURI: "/api/v1/rooms/room%2Fone/playback/" + tt.op,
				token:      "actor-token",
				body:       tt.body,
				response:   `{}`,
			})
			if err := tt.call(server.URL); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRESTRoomRadio(t *testing.T) {
	t.Run("play", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodPost,
			requestURI: "/api/v1/rooms/room%2Fone/radio",
			token:      "actor-token",
			body:       RoomRadioPlayRequest{Source: "playlist/source", Shuffle: true, Once: true},
			response:   `{}`,
		})
		if err := RESTRoomRadioPlay(context.Background(), server.URL, "actor-token", "room/one", "playlist/source", true, true); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		server := expectREST(t, restExpectation{
			method:     http.MethodDelete,
			requestURI: "/api/v1/rooms/room%2Fone/radio",
			token:      "actor-token",
			response:   `{}`,
		})
		if err := RESTRoomRadioStop(context.Background(), server.URL, "actor-token", "room/one"); err != nil {
			t.Fatal(err)
		}
	})
}

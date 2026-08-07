package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

type rawControlJSON string

func controlRESTRequest(t *testing.T, e *env, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	switch value := body.(type) {
	case nil:
	case rawControlJSON:
		reader = bytes.NewBufferString(string(value))
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, responseBody
}

func requireControlStatus(t *testing.T, status, want int, body []byte) {
	t.Helper()
	if status != want {
		t.Fatalf("status = %d, want %d; body = %s", status, want, body)
	}
}

func requireControlError(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	requireControlStatus(t, status, wantStatus, body)
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode error response: %v; body = %s", err, body)
	}
	if response.Error.Code != wantCode || response.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q and a message", response.Error, wantCode)
	}
}

func createControlRoom(t *testing.T, e *env, adminToken, roomID string) {
	t.Helper()
	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{"id": roomID, "name": roomID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create room %s: status = %d, body = %s", roomID, resp.StatusCode, body)
	}
}

func createControlPlaylist(t *testing.T, e *env, adminToken, trackRef string) string {
	t.Helper()
	resp := e.post(adminToken, "/api/v1/playlists", map[string]any{"name": "control radio"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create playlist: status = %d, body = %s", resp.StatusCode, body)
	}
	var created struct {
		Playlist struct {
			ID string `json:"id"`
		} `json:"playlist"`
	}
	decode(t, resp, &created)
	if created.Playlist.ID == "" {
		t.Fatal("create playlist returned an empty id")
	}
	resp = e.post(adminToken, "/api/v1/playlists/"+created.Playlist.ID+"/items", map[string]any{
		"track_refs": []string{trackRef},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add playlist item: status = %d, body = %s", resp.StatusCode, body)
	}
	return created.Playlist.ID
}

func TestRoomStateRESTDoesNotJoinListener(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("state-admin", "admin123")
	createControlRoom(t, e, adminToken, "state-room")
	trackRef := uploadTrack(t, e, adminToken, "state-track")
	_, requesterToken := e.guestAuth("state-requester", "")

	status, body := controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/state-room/queue", requesterToken, map[string]any{
		"track_ref": trackRef,
	})
	requireControlStatus(t, status, http.StatusOK, body)
	var queued struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || len(queued.EntryIDs) != 1 || queued.EntryIDs[0] == "" {
		t.Fatalf("queue response = %s, err = %v", body, err)
	}

	status, body = controlRESTRequest(t, e, http.MethodGet, "/api/v1/rooms/state-room/state", requesterToken, nil)
	requireControlStatus(t, status, http.StatusOK, body)
	var snapshot room.Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decode state: %v; body = %s", err, body)
	}
	if snapshot.Playback.Current == nil || snapshot.Playback.Current.TrackRef != trackRef {
		t.Fatalf("current = %+v, want track %q", snapshot.Playback.Current, trackRef)
	}
	if snapshot.Playback.Current.StreamURL == "" {
		t.Fatal("identity-specific state omitted current stream_url")
	}
	streamStatus, streamBody := controlRESTRequest(t, e, http.MethodGet, snapshot.Playback.Current.StreamURL, "", nil)
	if streamStatus != http.StatusOK || len(streamBody) == 0 {
		t.Fatalf("signed stream_url response: status = %d, bytes = %d", streamStatus, len(streamBody))
	}
	if len(snapshot.Listeners) != 0 {
		t.Fatalf("state query joined listener set: %+v", snapshot.Listeners)
	}

	status, secondBody := controlRESTRequest(t, e, http.MethodGet, "/api/v1/rooms/state-room/state", requesterToken, nil)
	requireControlStatus(t, status, http.StatusOK, secondBody)
	var second room.Snapshot
	if err := json.Unmarshal(secondBody, &second); err != nil {
		t.Fatalf("decode second state: %v", err)
	}
	if len(second.Listeners) != 0 {
		t.Fatalf("repeated state query joined listener set: %+v", second.Listeners)
	}
}

func TestRoomControlRESTUsesScopedControllerAndGlobalAdmin(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("control-admin", "admin123")
	controllerID, controllerToken := e.guestAuth("room-a-controller", "")
	_, requesterToken := e.guestAuth("queue-owner", "")
	createControlRoom(t, e, adminToken, "control-a")
	createControlRoom(t, e, adminToken, "control-b")
	if err := e.a.Store.GrantRoomGrant(context.Background(), "control-a", controllerID, control.CapabilityController); err != nil {
		t.Fatal(err)
	}
	trackRef := uploadTrack(t, e, adminToken, "control-track")
	playlistID := createControlPlaylist(t, e, adminToken, trackRef)

	status, body := controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-a/queue", requesterToken, map[string]any{
		"track_refs": []string{trackRef, trackRef, trackRef},
	})
	requireControlStatus(t, status, http.StatusOK, body)
	var queued struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || len(queued.EntryIDs) != 3 {
		t.Fatalf("queue response = %s, err = %v", body, err)
	}

	status, body = controlRESTRequest(t, e, http.MethodPatch, "/api/v1/rooms/control-a/queue/"+queued.EntryIDs[2], controllerToken, map[string]any{
		"to_index": 0,
	})
	requireControlStatus(t, status, http.StatusOK, body)
	status, body = controlRESTRequest(t, e, http.MethodDelete, "/api/v1/rooms/control-a/queue/"+queued.EntryIDs[1], controllerToken, nil)
	requireControlStatus(t, status, http.StatusOK, body)
	status, body = controlRESTRequest(t, e, http.MethodPatch, "/api/v1/rooms/control-a/queue/"+queued.EntryIDs[2], controllerToken, map[string]any{
		"to_index": 9,
	})
	requireControlError(t, status, body, http.StatusBadRequest, "bad_request")

	for _, call := range []struct {
		op   string
		body any
	}{
		{op: "pause"},
		{op: "seek", body: map[string]any{"position_ms": 1000}},
		{op: "resume"},
		{op: "skip"},
	} {
		status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-a/playback/"+call.op, controllerToken, call.body)
		requireControlStatus(t, status, http.StatusOK, body)
	}

	status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-a/radio", controllerToken, map[string]any{
		"source":  "playlist:" + playlistID,
		"shuffle": false,
		"once":    true,
	})
	requireControlStatus(t, status, http.StatusOK, body)
	status, body = controlRESTRequest(t, e, http.MethodDelete, "/api/v1/rooms/control-a/radio", controllerToken, nil)
	requireControlStatus(t, status, http.StatusOK, body)

	status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-b/queue", requesterToken, map[string]any{
		"track_ref": trackRef,
	})
	requireControlStatus(t, status, http.StatusOK, body)
	status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-b/playback/pause", controllerToken, nil)
	requireControlError(t, status, body, http.StatusForbidden, "forbidden")
	status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/control-b/playback/pause", adminToken, nil)
	requireControlStatus(t, status, http.StatusOK, body)
}

func TestQueueClearWSAndRESTPreservesPlaybackAndRadio(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("clear-admin", "admin123")
	controllerID, controllerToken := e.guestAuth("clear-controller", "")
	trustedID, trustedToken := e.guestAuth("clear-trusted", "")
	_, requesterToken := e.guestAuth("clear-requester", "")

	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
		"id":                "clear-room",
		"name":              "Clear Room",
		"guest_access_mode": "static_password",
		"guest_password":    "room-secret",
		"trusted_roles":     []string{"vip"},
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create clear room: status = %d, body = %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if err := e.a.Store.GrantRoomGrant(context.Background(), "clear-room", controllerID, control.CapabilityController); err != nil {
		t.Fatal(err)
	}
	principal, err := e.a.Store.GetPrincipal(context.Background(), trustedID)
	if err != nil {
		t.Fatal(err)
	}
	principal.Roles = append(principal.Roles, "vip")
	if err := e.a.Store.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}

	trackRef := uploadTrack(t, e, adminToken, "clear-track")
	playlistID := createControlPlaylist(t, e, adminToken, trackRef)
	status, body := controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/clear-room/queue", requesterToken, map[string]any{
		"track_refs": []string{trackRef, trackRef, trackRef},
	})
	requireControlStatus(t, status, http.StatusOK, body)
	status, body = controlRESTRequest(t, e, http.MethodPost, "/api/v1/rooms/clear-room/radio", adminToken, map[string]any{
		"source": "playlist:" + playlistID,
		"once":   false,
	})
	requireControlStatus(t, status, http.StatusOK, body)

	before := waitStableQueueState(t, e, adminToken, "clear-room")
	if before.Playback.Current == nil || before.Playback.Current.TrackRef != trackRef ||
		before.Radio == nil || before.Radio.Source != "playlist:"+playlistID || len(before.Queue) == 0 {
		t.Fatalf("clear precondition state = %#v", before)
	}

	trusted := e.dialWS()
	trusted.send("auth", "trusted-auth", map[string]any{"session_token": trustedToken})
	trusted.waitFor("auth.ok")
	trusted.send("room.join", "trusted-join", map[string]any{"room_id": "clear-room"})
	trusted.waitFor("room.joined")
	trusted.waitForQueueState("trusted join queue snapshot", func(items []client.QueueEntry) bool {
		return len(items) == len(before.Queue)
	})
	trusted.send("queue.clear", "trusted-clear", map[string]any{"room_id": "clear-room"})
	assertErrCode(t, trusted.waitFor("error"), "trusted-clear", "forbidden")
	afterDenied := readControlRoomState(t, e, adminToken, "clear-room")
	if len(afterDenied.Queue) != len(before.Queue) {
		t.Fatalf("trusted role cleared queue: before=%d after=%d", len(before.Queue), len(afterDenied.Queue))
	}

	controller := e.dialWS()
	controller.send("auth", "controller-auth", map[string]any{"session_token": controllerToken})
	controller.waitFor("auth.ok")
	controller.send("room.join", "controller-join", map[string]any{"room_id": "clear-room"})
	controller.waitFor("room.joined")
	controller.waitForQueueState("controller join queue snapshot", func(items []client.QueueEntry) bool {
		return len(items) == len(before.Queue)
	})
	controller.send("queue.clear", "controller-clear", map[string]any{"room_id": "clear-room"})
	ack := controller.waitFor("ack")
	if ack.Ref != "controller-clear" {
		t.Fatalf("clear ack ref = %q", ack.Ref)
	}
	controller.waitForQueueState("cleared queue patch", func(items []client.QueueEntry) bool {
		return len(items) == 0
	})

	afterWS := readControlRoomState(t, e, controllerToken, "clear-room")
	assertClearPreservedState(t, before, afterWS)
	status, body = controlRESTRequest(t, e, http.MethodDelete, "/api/v1/rooms/clear-room/queue", controllerToken, nil)
	requireControlStatus(t, status, http.StatusOK, body)
	afterREST := readControlRoomState(t, e, controllerToken, "clear-room")
	assertClearPreservedState(t, before, afterREST)
}

func readControlRoomState(t *testing.T, e *env, token, roomID string) room.Snapshot {
	t.Helper()
	status, body := controlRESTRequest(t, e, http.MethodGet, "/api/v1/rooms/"+roomID+"/state", token, nil)
	requireControlStatus(t, status, http.StatusOK, body)
	var snapshot room.Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decode room state: %v; body = %s", err, body)
	}
	return snapshot
}

// waitStableQueueState 等待队列长度两次采样一致后返回该状态。M5 起 radio.play
// 在异步 refill 完成前即返回，测试必须先等 refill 落地再取基准状态。
func waitStableQueueState(t *testing.T, e *env, token, roomID string) room.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	prevLen := -1
	for time.Now().Before(deadline) {
		s := readControlRoomState(t, e, token, roomID)
		if len(s.Queue) == prevLen {
			return s
		}
		prevLen = len(s.Queue)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("queue state did not stabilize for %s", roomID)
	return room.Snapshot{}
}

func assertClearPreservedState(t *testing.T, before, after room.Snapshot) {
	t.Helper()
	if len(after.Queue) != 0 {
		t.Fatalf("queue after clear = %#v, want empty", after.Queue)
	}
	if before.Playback.Current == nil || after.Playback.Current == nil ||
		after.Playback.Current.EntryID != before.Playback.Current.EntryID ||
		after.Playback.Current.TrackRef != before.Playback.Current.TrackRef ||
		after.Playback.Playing != before.Playback.Playing {
		t.Fatalf("playback changed by queue clear: before=%#v after=%#v", before.Playback, after.Playback)
	}
	if before.Radio == nil || after.Radio == nil ||
		after.Radio.Source != before.Radio.Source ||
		after.Radio.Shuffle != before.Radio.Shuffle ||
		after.Radio.Once != before.Radio.Once {
		t.Fatalf("radio changed by queue clear: before=%#v after=%#v", before.Radio, after.Radio)
	}
}

func TestRoomControlRESTRejectsBadPayloads(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("payload-admin", "admin123")
	createControlRoom(t, e, adminToken, "payload-room")

	status, body := controlRESTRequest(t, e, http.MethodGet, "/api/v1/rooms/payload-room/state", "", nil)
	requireControlError(t, status, body, http.StatusUnauthorized, "unauthorized")
	status, body = controlRESTRequest(t, e, http.MethodGet, "/api/v1/rooms/missing/state", adminToken, nil)
	requireControlError(t, status, body, http.StatusNotFound, "not_found")

	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = "local:any"
	}
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "queue missing ref", method: http.MethodPost, path: "/api/v1/rooms/payload-room/queue", body: map[string]any{}},
		{name: "queue forms conflict", method: http.MethodPost, path: "/api/v1/rooms/payload-room/queue", body: map[string]any{"track_ref": "local:a", "track_refs": []string{"local:b"}}},
		{name: "queue too large", method: http.MethodPost, path: "/api/v1/rooms/payload-room/queue", body: map[string]any{"track_refs": tooMany}},
		{name: "queue malformed", method: http.MethodPost, path: "/api/v1/rooms/payload-room/queue", body: rawControlJSON(`{"track_ref":`)},
		{name: "move missing index", method: http.MethodPatch, path: "/api/v1/rooms/payload-room/queue/entry", body: map[string]any{}},
		{name: "move negative index", method: http.MethodPatch, path: "/api/v1/rooms/payload-room/queue/entry", body: map[string]any{"to_index": -1}},
		{name: "seek missing position", method: http.MethodPost, path: "/api/v1/rooms/payload-room/playback/seek", body: map[string]any{}},
		{name: "seek negative position", method: http.MethodPost, path: "/api/v1/rooms/payload-room/playback/seek", body: map[string]any{"position_ms": -1}},
		{name: "pause unexpected payload", method: http.MethodPost, path: "/api/v1/rooms/payload-room/playback/pause", body: map[string]any{"unexpected": true}},
		{name: "unknown playback op", method: http.MethodPost, path: "/api/v1/rooms/payload-room/playback/restart", body: nil},
		{name: "radio missing source", method: http.MethodPost, path: "/api/v1/rooms/payload-room/radio", body: map[string]any{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, body := controlRESTRequest(t, e, test.method, test.path, adminToken, test.body)
			requireControlError(t, status, body, http.StatusBadRequest, "bad_request")
		})
	}
}

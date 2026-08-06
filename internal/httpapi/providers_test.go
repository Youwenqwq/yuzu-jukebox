package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/ncm"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

type providerEndpointBase struct {
	id string
}

func (p *providerEndpointBase) ID() string { return p.id }

func (p *providerEndpointBase) Search(context.Context, string) ([]provider.Track, error) {
	return nil, nil
}

func (p *providerEndpointBase) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, nil
}

func (p *providerEndpointBase) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, nil
}

type providerAccountWriterFake struct {
	providerEndpointBase
	st            *store.Store
	likeCalls     []string
	playlistCalls [][2]string
}

func (p *providerAccountWriterFake) ReportPlay(context.Context, string, int64, int64) error {
	return nil
}

func (p *providerAccountWriterFake) Like(_ context.Context, id string) error {
	p.likeCalls = append(p.likeCalls, id)
	return nil
}

func (p *providerAccountWriterFake) AddToPlaylist(_ context.Context, playlistID, trackID string) error {
	p.playlistCalls = append(p.playlistCalls, [2]string{playlistID, trackID})
	return nil
}
func (p *providerAccountWriterFake) SetCredential(ctx context.Context, payload string) error {
	return p.st.UpsertCredential(ctx, p.ID(), payload, "ok")
}

func (p *providerAccountWriterFake) CredentialStatus(context.Context) string {
	return "ok"
}

func (p *providerAccountWriterFake) QRLoginStart(context.Context) (string, string, error) {
	return "key", "content", nil
}

func (p *providerAccountWriterFake) QRLoginPoll(ctx context.Context, _ string) (string, string, error) {
	if err := p.st.UpsertCredential(ctx, p.ID(), "qr-credential", "ok"); err != nil {
		return "", "", err
	}
	return "ok", "authorized", nil
}

type providerEndpointFixture struct {
	handler    http.Handler
	st         *store.Store
	writer     *providerAccountWriterFake
	ownerToken string
	otherToken string
}

func setupProviderEndpoints(t *testing.T) providerEndpointFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	authm := auth.NewManager("", st)
	ownerToken := authm.IssueSession(auth.Identity{
		ID: "provider-owner", Name: "Owner", Kind: "guest",
		Roles: []string{auth.RoleRequester, auth.RoleMediaAdmin},
	})
	otherToken := authm.IssueSession(auth.Identity{
		ID: "provider-other", Name: "Other", Kind: "guest",
		Roles: []string{auth.RoleRequester, auth.RoleMediaAdmin},
	})
	if ownerToken == "" || otherToken == "" {
		t.Fatal("issue provider endpoint sessions")
	}

	reg := provider.NewRegistry()
	writer := &providerAccountWriterFake{
		providerEndpointBase: providerEndpointBase{id: "writer"},
		st:                   st,
	}
	reg.Register(writer)
	reg.Register(&providerEndpointBase{id: "basic"})
	reg.Register(ncm.New("http://127.0.0.1", "", st))
	if err := st.UpsertCredential(context.Background(), writer.ID(), "credential", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCredentialOwner(context.Background(), writer.ID(), "provider-owner"); err != nil {
		t.Fatal(err)
	}

	controls := control.NewService(nil, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, reg: reg, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	return providerEndpointFixture{
		handler: s.Handler(), st: st, writer: writer, ownerToken: ownerToken, otherToken: otherToken,
	}
}

func providerEndpointRequest(
	t *testing.T,
	f providerEndpointFixture,
	method, path, token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestProviderCredentialOwnerRebinding(t *testing.T) {
	f := setupProviderEndpoints(t)

	setByOther := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/credential", f.otherToken,
		map[string]string{"payload": "other-credential"})
	if setByOther.Code != http.StatusOK {
		t.Fatalf("set credential status = %d, body = %s", setByOther.Code, setByOther.Body.String())
	}
	owner, exists, err := f.st.GetCredentialOwner(context.Background(), f.writer.ID())
	if err != nil || !exists || owner.PrincipalID != "provider-other" {
		t.Fatalf("owner after other set = %#v, exists = %v, err = %v", owner, exists, err)
	}

	setByOwner := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/credential", f.ownerToken,
		map[string]string{"payload": "owner-credential"})
	if setByOwner.Code != http.StatusOK {
		t.Fatalf("second set credential status = %d, body = %s", setByOwner.Code, setByOwner.Body.String())
	}
	owner, exists, err = f.st.GetCredentialOwner(context.Background(), f.writer.ID())
	if err != nil || !exists || owner.PrincipalID != "provider-owner" {
		t.Fatalf("owner after owner set = %#v, exists = %v, err = %v", owner, exists, err)
	}

	qrByOther := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/qrlogin/key", f.otherToken, nil)
	if qrByOther.Code != http.StatusOK {
		t.Fatalf("qr login poll status = %d, body = %s", qrByOther.Code, qrByOther.Body.String())
	}
	owner, exists, err = f.st.GetCredentialOwner(context.Background(), f.writer.ID())
	if err != nil || !exists || owner.PrincipalID != "provider-other" {
		t.Fatalf("owner after qr login = %#v, exists = %v, err = %v", owner, exists, err)
	}
}

func TestProviderAccountWriteEndpoints(t *testing.T) {
	f := setupProviderEndpoints(t)

	nonOwnerLike := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/like", f.otherToken, map[string]string{"track": "101"})
	if nonOwnerLike.Code != http.StatusForbidden {
		t.Fatalf("non-owner like status = %d, body = %s", nonOwnerLike.Code, nonOwnerLike.Body.String())
	}
	if len(f.writer.likeCalls) != 0 {
		t.Fatalf("non-owner triggered like: %#v", f.writer.likeCalls)
	}

	ownerLike := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/like", f.ownerToken, map[string]string{"track": "101"})
	if ownerLike.Code != http.StatusOK {
		t.Fatalf("owner like status = %d, body = %s", ownerLike.Code, ownerLike.Body.String())
	}
	if !reflect.DeepEqual(f.writer.likeCalls, []string{"101"}) {
		t.Fatalf("like calls = %#v", f.writer.likeCalls)
	}

	nonOwnerPlaylistAdd := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/playlist-add", f.otherToken,
		map[string]string{"playlist_id": "9", "track": "202"})
	if nonOwnerPlaylistAdd.Code != http.StatusForbidden {
		t.Fatalf("non-owner playlist-add status = %d, body = %s", nonOwnerPlaylistAdd.Code, nonOwnerPlaylistAdd.Body.String())
	}
	if len(f.writer.playlistCalls) != 0 {
		t.Fatalf("non-owner triggered playlist-add: %#v", f.writer.playlistCalls)
	}

	missingPlaylistID := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/playlist-add", f.ownerToken, map[string]string{"track": "202"})
	if missingPlaylistID.Code != http.StatusBadRequest {
		t.Fatalf("missing playlist_id status = %d, body = %s", missingPlaylistID.Code, missingPlaylistID.Body.String())
	}
	if len(f.writer.playlistCalls) != 0 {
		t.Fatalf("invalid playlist-add triggered provider: %#v", f.writer.playlistCalls)
	}

	ownerPlaylistAdd := providerEndpointRequest(t, f, http.MethodPost,
		"/api/v1/providers/writer/playlist-add", f.ownerToken,
		map[string]string{"playlist_id": "9", "track": "202"})
	if ownerPlaylistAdd.Code != http.StatusOK {
		t.Fatalf("owner playlist-add status = %d, body = %s", ownerPlaylistAdd.Code, ownerPlaylistAdd.Body.String())
	}
	if !reflect.DeepEqual(f.writer.playlistCalls, [][2]string{{"9", "202"}}) {
		t.Fatalf("playlist-add calls = %#v", f.writer.playlistCalls)
	}
}

func TestListProvidersOwnershipAndCapabilities(t *testing.T) {
	f := setupProviderEndpoints(t)

	assertList := func(t *testing.T, token string, writerOwned bool) {
		t.Helper()
		rec := providerEndpointRequest(t, f, http.MethodGet, "/api/v1/providers", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list providers status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Providers []map[string]any `json:"providers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		entries := make(map[string]map[string]any, len(body.Providers))
		for _, entry := range body.Providers {
			id, _ := entry["id"].(string)
			entries[id] = entry
		}
		writer := entries["writer"]
		if owned, ok := writer["owned"].(bool); !ok || owned != writerOwned {
			t.Fatalf("writer owned = %#v, want %v", writer["owned"], writerOwned)
		}
		capabilities, ok := writer["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("writer capabilities = %#v", writer["capabilities"])
		}
		if got := capabilities["account_write"]; !reflect.DeepEqual(got, []any{"play_report", "like", "playlist_add"}) {
			t.Fatalf("writer account_write = %#v", got)
		}
		if _, ok := capabilities["radio_sources"]; ok {
			t.Fatalf("writer provider advertises radio_sources: %#v", capabilities)
		}
		ncmEntry := entries["ncm"]
		ncmCapabilities, ok := ncmEntry["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("ncm capabilities = %#v", ncmEntry["capabilities"])
		}
		if got := ncmCapabilities["account_write"]; !reflect.DeepEqual(got, []any{"play_report", "like", "playlist_add"}) {
			t.Fatalf("ncm account_write = %#v", got)
		}
		wantRadioSources := []any{
			map[string]any{"spec": "daily", "name": "每日推荐", "finite": true},
			map[string]any{"spec": "fm", "name": "私人 FM", "finite": false},
			map[string]any{"spec": "simi", "arg": "track_id", "name": "相似歌曲", "finite": false},
			map[string]any{"spec": "heart", "arg": "track_id", "name": "心动模式", "finite": false},
		}
		if got := ncmCapabilities["radio_sources"]; !reflect.DeepEqual(got, wantRadioSources) {
			t.Fatalf("ncm radio_sources = %#v, want %#v", got, wantRadioSources)
		}
		basic := entries["basic"]
		if _, ok := basic["capabilities"]; ok {
			t.Fatalf("basic provider includes capabilities: %#v", basic)
		}
	}

	t.Run("owner", func(t *testing.T) {
		assertList(t, f.ownerToken, true)
	})
	t.Run("other principal", func(t *testing.T) {
		assertList(t, f.otherToken, false)
	})
}

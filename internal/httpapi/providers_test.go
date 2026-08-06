package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
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

type providerCategorySearcherFake struct {
	providerEndpointBase
	categories         []provider.SearchCategory
	searchResults      []provider.SearchResult
	entityTracks       []provider.Track
	entityErr          error
	lastSearchCategory provider.SearchCategory
	lastSearchQuery    string
	lastEntityCategory provider.SearchCategory
	lastEntityID       string
}

func (p *providerCategorySearcherFake) SearchCategories() []provider.SearchCategory {
	return p.categories
}

func (p *providerCategorySearcherFake) SearchCategory(
	_ context.Context,
	category provider.SearchCategory,
	query string,
) ([]provider.SearchResult, error) {
	p.lastSearchCategory = category
	p.lastSearchQuery = query
	return p.searchResults, nil
}

func (p *providerCategorySearcherFake) EntityTracks(
	_ context.Context,
	category provider.SearchCategory,
	entityID string,
) ([]provider.Track, error) {
	p.lastEntityCategory = category
	p.lastEntityID = entityID
	return p.entityTracks, p.entityErr
}

type providerAccountWriterFake struct {
	providerEndpointBase
	st                   *store.Store
	likeCalls            []string
	likeCheckCalls       []string
	likeCheckErr         error
	playlistCalls        [][2]string
	accountPlaylists     []provider.AccountPlaylist
	accountPlaylistsErr  error
	accountPlaylistCalls int
}

func (p *providerAccountWriterFake) ReportPlay(context.Context, string, int64, int64) error {
	return nil
}

func (p *providerAccountWriterFake) Like(_ context.Context, id string) error {
	p.likeCalls = append(p.likeCalls, id)
	return nil
}

func (p *providerAccountWriterFake) LikeCheck(_ context.Context, id string) (bool, error) {
	p.likeCheckCalls = append(p.likeCheckCalls, id)
	return true, p.likeCheckErr
}

func (p *providerAccountWriterFake) AddToPlaylist(_ context.Context, playlistID, trackID string) error {
	p.playlistCalls = append(p.playlistCalls, [2]string{playlistID, trackID})
	return nil
}

func (p *providerAccountWriterFake) AccountPlaylists(context.Context) ([]provider.AccountPlaylist, error) {
	p.accountPlaylistCalls++
	return p.accountPlaylists, p.accountPlaylistsErr
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
	category   *providerCategorySearcherFake
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
		accountPlaylists: []provider.AccountPlaylist{{
			ID:         "playlist-7",
			Name:       "测试歌单",
			CoverURL:   "https://example.com/playlist.jpg",
			TrackCount: 23,
		}},
	}
	category := &providerCategorySearcherFake{
		providerEndpointBase: providerEndpointBase{id: "categorized"},
		categories: []provider.SearchCategory{
			provider.SearchCategorySong,
			provider.SearchCategoryArtist,
		},
		searchResults: []provider.SearchResult{{
			Type:     provider.SearchCategoryArtist,
			EntityID: "artist-7",
			Name:     "测试歌手",
			Detail:   "歌手简介",
			CoverURL: "https://example.com/artist.jpg",
		}},
		entityTracks: []provider.Track{{
			Ref:        provider.NewRef("categorized", "track-9"),
			Title:      "测试歌曲",
			Artist:     "测试歌手",
			DurationMs: 123000,
		}},
	}
	reg.Register(writer)
	reg.Register(category)
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
		handler: s.Handler(), st: st, writer: writer, category: category,
		ownerToken: ownerToken, otherToken: otherToken,
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

func TestProviderAccountPlaylists(t *testing.T) {
	f := setupProviderEndpoints(t)

	owner := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/account-playlists", f.ownerToken, nil)
	if owner.Code != http.StatusOK {
		t.Fatalf("owner account-playlists status = %d, body = %s", owner.Code, owner.Body.String())
	}
	var ownerBody struct {
		Playlists []provider.AccountPlaylist `json:"playlists"`
	}
	if err := json.Unmarshal(owner.Body.Bytes(), &ownerBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ownerBody.Playlists, f.writer.accountPlaylists) {
		t.Fatalf("account playlists = %#v, want %#v", ownerBody.Playlists, f.writer.accountPlaylists)
	}
	if f.writer.accountPlaylistCalls != 1 {
		t.Fatalf("account playlist calls = %d", f.writer.accountPlaylistCalls)
	}

	nonOwner := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/account-playlists", f.otherToken, nil)
	if nonOwner.Code != http.StatusForbidden {
		t.Fatalf("non-owner account-playlists status = %d, body = %s", nonOwner.Code, nonOwner.Body.String())
	}
	if f.writer.accountPlaylistCalls != 1 {
		t.Fatalf("non-owner triggered account-playlists: calls = %d", f.writer.accountPlaylistCalls)
	}

	unsupported := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/basic/account-playlists", f.ownerToken, nil)
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported account-playlists status = %d, body = %s", unsupported.Code, unsupported.Body.String())
	}

	f.writer.accountPlaylistsErr = errors.New("playlist upstream failed")
	upstreamFailure := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/account-playlists", f.ownerToken, nil)
	if upstreamFailure.Code != http.StatusBadGateway {
		t.Fatalf("account-playlists provider error status = %d, body = %s",
			upstreamFailure.Code, upstreamFailure.Body.String())
	}
}

func TestProviderLikeCheck(t *testing.T) {
	f := setupProviderEndpoints(t)

	owner := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/like-check?track=101", f.ownerToken, nil)
	if owner.Code != http.StatusOK {
		t.Fatalf("owner like-check status = %d, body = %s", owner.Code, owner.Body.String())
	}
	var ownerBody struct {
		Liked bool `json:"liked"`
	}
	if err := json.Unmarshal(owner.Body.Bytes(), &ownerBody); err != nil {
		t.Fatal(err)
	}
	if !ownerBody.Liked {
		t.Fatalf("owner like-check body = %s", owner.Body.String())
	}
	if !reflect.DeepEqual(f.writer.likeCheckCalls, []string{"101"}) {
		t.Fatalf("like-check calls = %#v", f.writer.likeCheckCalls)
	}

	missingTrack := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/like-check", f.ownerToken, nil)
	if missingTrack.Code != http.StatusBadRequest {
		t.Fatalf("missing track status = %d, body = %s", missingTrack.Code, missingTrack.Body.String())
	}
	if !reflect.DeepEqual(f.writer.likeCheckCalls, []string{"101"}) {
		t.Fatalf("missing track triggered like-check: %#v", f.writer.likeCheckCalls)
	}

	nonOwner := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/like-check?track=202", f.otherToken, nil)
	if nonOwner.Code != http.StatusForbidden {
		t.Fatalf("non-owner like-check status = %d, body = %s", nonOwner.Code, nonOwner.Body.String())
	}
	if !reflect.DeepEqual(f.writer.likeCheckCalls, []string{"101"}) {
		t.Fatalf("non-owner triggered like-check: %#v", f.writer.likeCheckCalls)
	}

	f.writer.likeCheckErr = errors.New("like upstream failed")
	upstreamFailure := providerEndpointRequest(t, f, http.MethodGet,
		"/api/v1/providers/writer/like-check?track=303", f.ownerToken, nil)
	if upstreamFailure.Code != http.StatusBadGateway {
		t.Fatalf("like-check provider error status = %d, body = %s",
			upstreamFailure.Code, upstreamFailure.Body.String())
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
		if got := capabilities["account_write"]; !reflect.DeepEqual(got, []any{"play_report", "like", "like_check", "playlist_add"}) {
			t.Fatalf("writer account_write = %#v", got)
		}
		if _, ok := capabilities["radio_sources"]; ok {
			t.Fatalf("writer provider advertises radio_sources: %#v", capabilities)
		}
		if _, ok := capabilities["search_categories"]; ok {
			t.Fatalf("writer provider advertises search_categories: %#v", capabilities)
		}
		ncmEntry := entries["ncm"]
		ncmCapabilities, ok := ncmEntry["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("ncm capabilities = %#v", ncmEntry["capabilities"])
		}
		if got := ncmCapabilities["account_write"]; !reflect.DeepEqual(got, []any{"play_report", "like", "like_check", "playlist_add"}) {
			t.Fatalf("ncm account_write = %#v", got)
		}
		wantRadioSources := []any{
			map[string]any{"spec": "daily", "name": "每日推荐", "finite": true},
			map[string]any{"spec": "fm", "name": "私人 FM", "finite": false},
			map[string]any{"spec": "simi", "arg": "track_id", "name": "相似歌曲", "finite": false},
			map[string]any{"spec": "heart", "arg": "track_id", "name": "心动模式", "finite": false},
			map[string]any{"spec": "playlist", "arg": "playlist_id", "name": "歌单电台", "finite": true},
		}
		if got := ncmCapabilities["radio_sources"]; !reflect.DeepEqual(got, wantRadioSources) {
			t.Fatalf("ncm radio_sources = %#v, want %#v", got, wantRadioSources)
		}
		categorized := entries["categorized"]
		categoryCapabilities, ok := categorized["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("categorized capabilities = %#v", categorized["capabilities"])
		}
		if got := categoryCapabilities["search_categories"]; !reflect.DeepEqual(got, []any{"song", "artist"}) {
			t.Fatalf("categorized search_categories = %#v", got)
		}
		if _, ok := categoryCapabilities["radio_sources"]; ok {
			t.Fatalf("categorized provider advertises radio_sources: %#v", categoryCapabilities)
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

func TestSearchCategories(t *testing.T) {
	f := setupProviderEndpoints(t)

	t.Run("legacy and explicit song retain tracks response", func(t *testing.T) {
		legacy := providerEndpointRequest(
			t, f, http.MethodGet, "/api/v1/search?provider=basic&q=test", f.ownerToken, nil,
		)
		explicitSong := providerEndpointRequest(
			t, f, http.MethodGet, "/api/v1/search?provider=basic&q=test&category=song", f.ownerToken, nil,
		)
		if legacy.Code != http.StatusOK {
			t.Fatalf("legacy search status = %d, body = %s", legacy.Code, legacy.Body.String())
		}
		if explicitSong.Code != http.StatusOK {
			t.Fatalf("song search status = %d, body = %s", explicitSong.Code, explicitSong.Body.String())
		}
		const want = "{\"tracks\":null}\n"
		if legacy.Body.String() != want || explicitSong.Body.String() != want {
			t.Fatalf("legacy/song bodies = %q / %q, want %q", legacy.Body.String(), explicitSong.Body.String(), want)
		}
	})

	t.Run("artist search returns entities", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=categorized&q=%E6%B5%8B%E8%AF%95&category=artist",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("artist search status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Results []provider.SearchResult `json:"results"`
			Tracks  json.RawMessage         `json:"tracks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body.Results, f.category.searchResults) {
			t.Fatalf("artist results = %#v, want %#v", body.Results, f.category.searchResults)
		}
		if body.Tracks != nil {
			t.Fatalf("artist response includes tracks: %s", body.Tracks)
		}
		if f.category.lastSearchCategory != provider.SearchCategoryArtist || f.category.lastSearchQuery != "测试" {
			t.Fatalf(
				"search call = (%q, %q)",
				f.category.lastSearchCategory,
				f.category.lastSearchQuery,
			)
		}
	})

	t.Run("unsupported category lists provider categories", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=categorized&q=test&category=album",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unsupported category status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "song, artist") {
			t.Fatalf("unsupported category response does not list supported categories: %s", rec.Body.String())
		}
	})

	t.Run("unknown category is rejected", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=categorized&q=test&category=bogus",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown category") {
			t.Fatalf("bogus category status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("provider without category search is rejected", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=basic&q=test&category=artist",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "capability not supported") {
			t.Fatalf("basic category search status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestSearchEntity(t *testing.T) {
	f := setupProviderEndpoints(t)

	t.Run("artist drill returns tracks", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=artist&id=artist-7",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("artist drill status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tracks []provider.Track `json:"tracks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body.Tracks, f.category.entityTracks) {
			t.Fatalf("artist tracks = %#v, want %#v", body.Tracks, f.category.entityTracks)
		}
		if f.category.lastEntityCategory != provider.SearchCategoryArtist || f.category.lastEntityID != "artist-7" {
			t.Fatalf("entity call = (%q, %q)", f.category.lastEntityCategory, f.category.lastEntityID)
		}
	})

	t.Run("playlist entities are not drilled", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=playlist&id=list-1",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "imported, not drilled") {
			t.Fatalf("playlist drill status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("id is required", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=artist",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "id is required") {
			t.Fatalf("missing id status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("provider without category search is rejected", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=basic&category=artist&id=artist-7",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "capability not supported") {
			t.Fatalf("basic entity drill status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("not supported sentinel maps to bad request", func(t *testing.T) {
		f.category.entityErr = provider.ErrNotSupported
		t.Cleanup(func() { f.category.entityErr = nil })
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=album&id=album-2",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "capability not supported") {
			t.Fatalf("unsupported entity status = %d, body = %s", rec.Code, rec.Body.String())
		}
		f.category.entityErr = nil
	})

	t.Run("provider failure maps to bad gateway", func(t *testing.T) {
		f.category.entityErr = errors.New("upstream failed")
		t.Cleanup(func() { f.category.entityErr = nil })
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=album&id=album-2",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "provider_error") {
			t.Fatalf("failed entity status = %d, body = %s", rec.Code, rec.Body.String())
		}
		f.category.entityErr = nil
	})
}

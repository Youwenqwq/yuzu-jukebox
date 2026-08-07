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
	id               string
	lastSearchQuery  string
	lastSearchLimit  int
	lastSearchOffset int
}

func (p *providerEndpointBase) ID() string { return p.id }

func (p *providerEndpointBase) Search(_ context.Context, query string, limit, offset int) ([]provider.Track, error) {
	p.lastSearchQuery = query
	p.lastSearchLimit = limit
	p.lastSearchOffset = offset
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
	lastSearchLimit    int
	lastSearchOffset   int
	lastEntityCategory provider.SearchCategory
	lastEntityID       string
	lastEntityLimit    int
	lastEntityOffset   int
}

func (p *providerCategorySearcherFake) SearchCategories() []provider.SearchCategory {
	return p.categories
}

func (p *providerCategorySearcherFake) SearchCategory(
	_ context.Context,
	category provider.SearchCategory,
	query string,
	limit, offset int,
) ([]provider.SearchResult, error) {
	p.lastSearchCategory = category
	p.lastSearchQuery = query
	p.lastSearchLimit = limit
	p.lastSearchOffset = offset
	return p.searchResults, nil
}

func (p *providerCategorySearcherFake) EntityTracks(
	_ context.Context,
	category provider.SearchCategory,
	entityID string,
	limit, offset int,
) ([]provider.Track, error) {
	p.lastEntityCategory = category
	p.lastEntityID = entityID
	p.lastEntityLimit = limit
	p.lastEntityOffset = offset
	return p.entityTracks, p.entityErr
}

type providerEntityAlbumListerFake struct {
	providerEndpointBase
	albums           []provider.SearchResult
	lastArtistID     string
	lastAlbumsLimit  int
	lastAlbumsOffset int
}

func (p *providerEntityAlbumListerFake) EntityAlbums(
	_ context.Context,
	artistID string,
	limit, offset int,
) ([]provider.SearchResult, error) {
	p.lastArtistID = artistID
	p.lastAlbumsLimit = limit
	p.lastAlbumsOffset = offset
	return p.albums, nil
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
	albums     *providerEntityAlbumListerFake
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
	albums := &providerEntityAlbumListerFake{
		providerEndpointBase: providerEndpointBase{id: "albums"},
		albums: []provider.SearchResult{{
			Type:     provider.SearchCategoryAlbum,
			EntityID: "album-8",
			Name:     "测试专辑",
			Detail:   "2026",
			CoverURL: "https://example.com/album.jpg",
		}},
	}
	reg.Register(writer)
	reg.Register(category)
	reg.Register(albums)
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
	s.SetCoverSecret([]byte("0123456789abcdef0123456789abcdef"))
	return providerEndpointFixture{
		handler: s.Handler(), st: st, writer: writer, category: category, albums: albums,
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
		// 快照未设置：任何用户都不应看到 account 块
		if _, ok := writer["account"]; ok {
			t.Fatalf("writer account present without snapshot: %#v", writer["account"])
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
		if _, ok := capabilities["entity_albums"]; ok {
			t.Fatalf("writer provider advertises entity_albums: %#v", capabilities)
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
		if _, ok := categoryCapabilities["entity_albums"]; ok {
			t.Fatalf("categorized provider advertises entity_albums: %#v", categoryCapabilities)
		}

		albumCapabilities, ok := entries["albums"]["capabilities"].(map[string]any)
		if !ok || albumCapabilities["entity_albums"] != true {
			t.Fatalf("albums capabilities = %#v", entries["albums"]["capabilities"])
		}
		if _, ok := entries["basic"]["capabilities"]; ok {
			t.Fatalf("basic provider includes capabilities: %#v", entries["basic"])
		}
	}

	t.Run("owner", func(t *testing.T) {
		assertList(t, f.ownerToken, true)
	})
	t.Run("other principal", func(t *testing.T) {
		assertList(t, f.otherToken, false)
	})

	t.Run("account snapshot is owner-only and uid-free", func(t *testing.T) {
		ctx := context.Background()
		if err := f.st.SetCredentialAccount(ctx, f.writer.ID(), store.AccountProfile{
			UID: "9988", Name: "小明", Avatar: "https://av/1",
		}); err != nil {
			t.Fatalf("SetCredentialAccount: %v", err)
		}

		owner := providerEndpointRequest(t, f, http.MethodGet, "/api/v1/providers", f.ownerToken, nil)
		if owner.Code != http.StatusOK {
			t.Fatalf("owner list status = %d", owner.Code)
		}
		var ownerBody struct {
			Providers []map[string]any `json:"providers"`
		}
		if err := json.Unmarshal(owner.Body.Bytes(), &ownerBody); err != nil {
			t.Fatal(err)
		}
		var ownerEntry map[string]any
		for _, entry := range ownerBody.Providers {
			if entry["id"] == "writer" {
				ownerEntry = entry
			}
		}
		account, ok := ownerEntry["account"].(map[string]any)
		if !ok {
			t.Fatalf("owner writer account = %#v, want snapshot", ownerEntry["account"])
		}
		if account["name"] != "小明" || account["avatar"] != "https://av/1" {
			t.Fatalf("owner account = %#v, want name+avatar", account)
		}
		if _, leaked := account["uid"]; leaked {
			t.Fatalf("account leaks uid: %#v", account)
		}
		if strings.Contains(owner.Body.String(), "9988") {
			t.Fatalf("response leaks uid 9988: %s", owner.Body.String())
		}

		// 非所有者看不到 account 块
		other := providerEndpointRequest(t, f, http.MethodGet, "/api/v1/providers", f.otherToken, nil)
		var otherBody struct {
			Providers []map[string]any `json:"providers"`
		}
		if err := json.Unmarshal(other.Body.Bytes(), &otherBody); err != nil {
			t.Fatal(err)
		}
		for _, entry := range otherBody.Providers {
			if entry["id"] == "writer" {
				if _, ok := entry["account"]; ok {
					t.Fatalf("non-owner sees writer account: %#v", entry["account"])
				}
			}
		}
	})

	t.Run("empty snapshot omits account block", func(t *testing.T) {
		ctx := context.Background()
		if err := f.st.SetCredentialAccount(ctx, f.writer.ID(), store.AccountProfile{UID: "9988"}); err != nil {
			t.Fatalf("SetCredentialAccount: %v", err)
		}
		rec := providerEndpointRequest(t, f, http.MethodGet, "/api/v1/providers", f.ownerToken, nil)
		var body struct {
			Providers []map[string]any `json:"providers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, entry := range body.Providers {
			if entry["id"] == "writer" {
				if _, ok := entry["account"]; ok {
					t.Fatalf("empty snapshot still emits account: %#v", entry["account"])
				}
			}
		}
	})
}

func TestSearchCategories(t *testing.T) {
	f := setupProviderEndpoints(t)

	t.Run("legacy and explicit song retain tracks response", func(t *testing.T) {
		legacy := providerEndpointRequest(
			t, f, http.MethodGet, "/api/v1/search?provider=categorized&q=test", f.ownerToken, nil,
		)
		if legacy.Code != http.StatusOK {
			t.Fatalf("legacy search status = %d, body = %s", legacy.Code, legacy.Body.String())
		}
		if f.category.providerEndpointBase.lastSearchLimit != 30 ||
			f.category.providerEndpointBase.lastSearchOffset != 0 {
			t.Fatalf(
				"legacy paging = (%d, %d), want (30, 0)",
				f.category.providerEndpointBase.lastSearchLimit,
				f.category.providerEndpointBase.lastSearchOffset,
			)
		}
		explicitSong := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=categorized&q=test&category=song&limit=7&offset=4",
			f.ownerToken,
			nil,
		)
		if explicitSong.Code != http.StatusOK {
			t.Fatalf("song search status = %d, body = %s", explicitSong.Code, explicitSong.Body.String())
		}
		if f.category.providerEndpointBase.lastSearchLimit != 7 ||
			f.category.providerEndpointBase.lastSearchOffset != 4 {
			t.Fatalf(
				"song paging = (%d, %d), want (7, 4)",
				f.category.providerEndpointBase.lastSearchLimit,
				f.category.providerEndpointBase.lastSearchOffset,
			)
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
			"/api/v1/search?provider=categorized&q=%E6%B5%8B%E8%AF%95&category=artist&limit=12&offset=3",
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
		if f.category.lastSearchCategory != provider.SearchCategoryArtist ||
			f.category.lastSearchQuery != "测试" ||
			f.category.lastSearchLimit != 12 ||
			f.category.lastSearchOffset != 3 {
			t.Fatalf(
				"search call = (%q, %q, %d, %d)",
				f.category.lastSearchCategory,
				f.category.lastSearchQuery,
				f.category.lastSearchLimit,
				f.category.lastSearchOffset,
			)
		}
	})

	t.Run("invalid paging is rejected and non-positive limit defaults", func(t *testing.T) {
		defaulted := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search?provider=categorized&q=test&limit=0",
			f.ownerToken,
			nil,
		)
		if defaulted.Code != http.StatusOK || f.category.providerEndpointBase.lastSearchLimit != 30 {
			t.Fatalf(
				"zero limit status = %d, forwarded = %d, body = %s",
				defaulted.Code,
				f.category.providerEndpointBase.lastSearchLimit,
				defaulted.Body.String(),
			)
		}
		for _, path := range []string{
			"/api/v1/search?provider=categorized&q=test&offset=-1",
			"/api/v1/search?provider=categorized&q=test&offset=nope",
			"/api/v1/search?provider=categorized&q=test&limit=nope",
			"/api/v1/search?provider=categorized&q=test&limit=101",
		} {
			rec := providerEndpointRequest(t, f, http.MethodGet, path, f.ownerToken, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
			}
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
		if f.category.lastEntityCategory != provider.SearchCategoryArtist ||
			f.category.lastEntityID != "artist-7" ||
			f.category.lastEntityLimit != 30 ||
			f.category.lastEntityOffset != 0 {
			t.Fatalf(
				"entity call = (%q, %q, %d, %d)",
				f.category.lastEntityCategory,
				f.category.lastEntityID,
				f.category.lastEntityLimit,
				f.category.lastEntityOffset,
			)
		}
	})

	t.Run("artist drill into albums proxies covers and forwards paging", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=albums&category=artist&id=artist-7&into=albums&limit=6&offset=2",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("album drill status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Results []provider.SearchResult `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Results) != 1 ||
			!strings.HasPrefix(body.Results[0].CoverURL, "/api/v1/cover/ext/") {
			t.Fatalf("album results = %#v", body.Results)
		}
		if f.albums.lastArtistID != "artist-7" ||
			f.albums.lastAlbumsLimit != 6 ||
			f.albums.lastAlbumsOffset != 2 {
			t.Fatalf(
				"album call = (%q, %d, %d)",
				f.albums.lastArtistID,
				f.albums.lastAlbumsLimit,
				f.albums.lastAlbumsOffset,
			)
		}
	})

	t.Run("album drill requires artist category", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=albums&category=song&id=artist-7&into=albums",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), "into=albums requires category=artist") {
			t.Fatalf("song into albums status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("album drill requires capability", func(t *testing.T) {
		rec := providerEndpointRequest(
			t,
			f,
			http.MethodGet,
			"/api/v1/search/entity?provider=categorized&category=artist&id=artist-7&into=albums",
			f.ownerToken,
			nil,
		)
		if rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), "capability not supported") {
			t.Fatalf("unsupported album drill status = %d, body = %s", rec.Code, rec.Body.String())
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

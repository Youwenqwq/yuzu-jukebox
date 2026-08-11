package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

// discoveryFakeProvider 实现 ArtistDetailer 与 ArtistIDDetailer。
type discoveryFakeProvider struct {
	id          string
	detail      provider.ArtistDetail
	detailErr   error
	detailCalls *[]string
	// detailName 非空时仅在该名字匹配时返回 detail（模拟真实 detailer 的名字敏感）
	detailName   string
	track        provider.Track
	trackErr     error
	idDetail     provider.ArtistDetail
	idDetailErr  error
	callSequence *[]string
}

func (p *discoveryFakeProvider) ID() string { return p.id }
func (*discoveryFakeProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (p *discoveryFakeProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	if p.callSequence != nil {
		*p.callSequence = append(*p.callSequence, "track:"+ref.String())
	}
	return p.track, p.trackErr
}
func (*discoveryFakeProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errors.New("unused")
}
func (p *discoveryFakeProvider) ArtistDetail(_ context.Context, name string) (provider.ArtistDetail, error) {
	if p.detailCalls != nil {
		*p.detailCalls = append(*p.detailCalls, p.id+":"+name)
	}
	if p.callSequence != nil {
		*p.callSequence = append(*p.callSequence, "name:"+name)
	}
	if p.detailName != "" && name != p.detailName {
		return provider.ArtistDetail{}, errors.New("artist not found")
	}
	return p.detail, p.detailErr
}
func (p *discoveryFakeProvider) ArtistDetailByID(_ context.Context, entityID string) (provider.ArtistDetail, error) {
	if p.callSequence != nil {
		*p.callSequence = append(*p.callSequence, "id:"+entityID)
	}
	return p.idDetail, p.idDetailErr
}

// discoveryNameOnlyProvider exercises the fallback when ArtistIDDetailer is absent.
type discoveryNameOnlyProvider struct {
	id           string
	track        provider.Track
	detail       provider.ArtistDetail
	callSequence *[]string
}

func (p *discoveryNameOnlyProvider) ID() string { return p.id }
func (*discoveryNameOnlyProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (p *discoveryNameOnlyProvider) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	*p.callSequence = append(*p.callSequence, "track:"+ref.String())
	return p.track, nil
}
func (*discoveryNameOnlyProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errors.New("unused")
}
func (p *discoveryNameOnlyProvider) ArtistDetail(_ context.Context, name string) (provider.ArtistDetail, error) {
	*p.callSequence = append(*p.callSequence, "name:"+name)
	return p.detail, nil
}

type discoveryFixture struct {
	handler http.Handler
	st      *store.Store
	reg     *provider.Registry
	authm   *auth.Manager
	token   string
}

func setupDiscoveryEndpoints(t *testing.T) discoveryFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "discovery.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authm := auth.NewManager("", st)
	token := authm.IssueSession(auth.Identity{
		ID: "alice", Name: "Alice", Kind: "guest", Roles: []string{auth.RoleRequester},
	})
	if token == "" {
		t.Fatal("issue requester session")
	}
	reg := provider.NewRegistry()
	controls := control.NewService(nil, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, reg: reg, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	s.SetCoverSecret([]byte("test-secret"))
	return discoveryFixture{handler: s.Handler(), st: st, reg: reg, authm: authm, token: token}
}

func discoveryRequest(t *testing.T, f discoveryFixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestArtistProfileEndpoint(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm",
		detail: provider.ArtistDetail{
			Name: "周杰伦", EntityID: "777", AvatarURL: "https://img/avatar.jpg", Bio: "歌手",
		},
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("周杰伦"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist map[string]any `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist["name"] != "周杰伦" || body.Artist["provider"] != "ncm" || body.Artist["entity_id"] != "777" || body.Artist["bio"] != "歌手" {
		t.Fatalf("artist = %#v, want resolved ncm entity", body.Artist)
	}
	avatar, _ := body.Artist["avatar_url"].(string)
	if !strings.HasPrefix(avatar, "/api/v1/cover/ext/") {
		t.Fatalf("avatar_url = %q, want ext proxy", avatar)
	}
	for _, removed := range []string{"play_count", "last_played_at", "top_tracks"} {
		if _, exists := body.Artist[removed]; exists {
			t.Fatalf("artist unexpectedly contains removed field %q: %#v", removed, body.Artist)
		}
	}
}

func TestArtistProfileDirectByIDWithoutNameSearch(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm", callSequence: &calls,
		idDetail: provider.ArtistDetail{
			Name: "Chevy", EntityID: "47992679", AvatarURL: "https://img/chevy.jpg", Bio: "Singer",
		},
		detailErr: errors.New("name search must not run"),
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/Chevy?provider=ncm&entity_id=47992679")
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Name != "Chevy" || body.Artist.Provider != "ncm" ||
		body.Artist.EntityID != "47992679" || body.Artist.Bio != "Singer" {
		t.Fatalf("artist = %#v, want direct ncm entity", body.Artist)
	}
	if !strings.HasPrefix(body.Artist.AvatarURL, "/api/v1/cover/ext/") {
		t.Fatalf("avatar_url = %q, want ext proxy", body.Artist.AvatarURL)
	}
	wantCalls := []string{"id:47992679"}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] {
		t.Fatalf("calls = %#v, want %#v (zero name searches)", calls, wantCalls)
	}
}

func TestArtistProfileDirectByIDRequiresBothParameters(t *testing.T) {
	for _, query := range []string{
		"provider=ncm",
		"entity_id=47992679",
		"provider=ncm&entity_id=",
		"provider=&entity_id=47992679",
		"provider=&entity_id=",
	} {
		t.Run(query, func(t *testing.T) {
			f := setupDiscoveryEndpoints(t)
			rec := discoveryRequest(t, f, "/api/v1/artists/Chevy?"+query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "bad_request" {
				t.Fatalf("error = %#v, want bad_request", body.Error)
			}
		})
	}
}

func TestArtistProfileDirectByIDRejectsUnknownProvider(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	rec := discoveryRequest(t, f, "/api/v1/artists/Chevy?provider=missing&entity_id=47992679")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "not_found" {
		t.Fatalf("error = %#v, want not_found", body.Error)
	}
}

func TestArtistProfileDirectByIDFailureFallsBackToNameSearch(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm", callSequence: &calls,
		idDetailErr: errors.New("artist unavailable"),
		detail:      provider.ArtistDetail{Name: "Chevy", EntityID: "searched"},
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/Chevy?provider=ncm&entity_id=47992679")
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "ncm" || body.Artist.EntityID != "searched" {
		t.Fatalf("artist = %#v, want name-search fallback", body.Artist)
	}
	wantCalls := []string{"id:47992679", "name:Chevy"}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] || calls[1] != wantCalls[1] {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestArtistProfileDirectByIDFallsBackWhenCapabilityIsAbsent(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryNameOnlyProvider{
		id: "ncm", callSequence: &calls,
		detail: provider.ArtistDetail{Name: "Chevy", EntityID: "searched"},
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/Chevy?provider=ncm&entity_id=47992679")
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "ncm" || body.Artist.EntityID != "searched" {
		t.Fatalf("artist = %#v, want name-search fallback", body.Artist)
	}
	wantCalls := []string{"name:Chevy"}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestArtistProfileMultiArtistSegmentFallback(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm", detailCalls: &calls,
		detailName: "张晶",
		detail:     provider.ArtistDetail{Name: "张晶", EntityID: "777", Bio: "歌手"},
	})

	// 完整 join 串与前面各段都解析失败，第三段 "张晶" 成功 → 返回该段实体。
	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("塞壬唱片-MSR/F.L.O.A.T (白羽）/张晶"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "ncm" || body.Artist.EntityID != "777" || body.Artist.Bio != "歌手" {
		t.Fatalf("artist = %#v, want segment-resolved ncm entity", body.Artist)
	}
	// 调用序列：完整串 → 各段，取首个成功
	want := []string{"ncm:塞壬唱片-MSR/F.L.O.A.T (白羽）/张晶", "ncm:塞壬唱片-MSR", "ncm:F.L.O.A.T (白羽）", "ncm:张晶"}
	if len(calls) != len(want) {
		t.Fatalf("detail calls = %#v, want %#v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("detail call[%d] = %q, want %q (all = %#v)", i, calls[i], want[i], calls)
		}
	}
}

func TestArtistProfileAllProvidersFailReturnsNameOnly(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	f.reg.Register(&discoveryFakeProvider{id: "ncm", detailErr: errors.New("artist not found")})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("本地歌手"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist map[string]any `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Artist) != 1 || body.Artist["name"] != "本地歌手" {
		t.Fatalf("artist = %#v, want name only", body.Artist)
	}
}

func TestArtistProfileTrackRefResolvesContributorByIDWithoutNameSearch(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "aaa", callSequence: &calls,
		detail: provider.ArtistDetail{Name: "全局首项", EntityID: "global"},
	})
	f.reg.Register(&discoveryFakeProvider{
		id: "zzz", callSequence: &calls,
		track: provider.Track{Contributors: []provider.Contributor{
			{Role: "composer", Name: "歌手", EntityID: "wrong-role"},
			{Role: "artist", Name: "别的歌手", EntityID: "wrong-name"},
			{Role: "uploader", Name: "歌手", EntityID: "artist-42"},
		}},
		idDetail: provider.ArtistDetail{
			Name: "歌手", EntityID: "artist-42", AvatarURL: "https://img/direct.jpg", Bio: "直接解析",
		},
		detailErr: errors.New("name search must not run"),
	})

	path := "/api/v1/artists/" + url.PathEscape("歌手") + "?track_ref=" + url.QueryEscape("zzz:track")
	rec := discoveryRequest(t, f, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "zzz" || body.Artist.EntityID != "artist-42" || body.Artist.Bio != "直接解析" {
		t.Fatalf("artist = %#v, want contributor-ID resolved zzz entity", body.Artist)
	}
	wantCalls := []string{"track:zzz:track", "id:artist-42"}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %#v, want %#v (zero name searches)", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("call[%d] = %q, want %q (all = %#v)", i, calls[i], wantCalls[i], calls)
		}
	}
}

func TestArtistProfileTrackRefFallsBackToNameSearch(t *testing.T) {
	tests := []struct {
		name      string
		track     provider.Track
		trackErr  error
		idErr     error
		wantCalls []string
	}{
		{
			name: "track lookup failure", trackErr: errors.New("track unavailable"),
			wantCalls: []string{"track:zzz:track", "name:歌手"},
		},
		{
			name:      "matching contributor has no entity ID",
			track:     provider.Track{Contributors: []provider.Contributor{{Role: "artist", Name: "歌手"}}},
			wantCalls: []string{"track:zzz:track", "name:歌手"},
		},
		{
			name:      "direct entity lookup failure",
			track:     provider.Track{Contributors: []provider.Contributor{{Role: "artist", Name: "歌手", EntityID: "artist-42"}}},
			idErr:     errors.New("artist unavailable"),
			wantCalls: []string{"track:zzz:track", "id:artist-42", "name:歌手"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDiscoveryEndpoints(t)
			var calls []string
			f.reg.Register(&discoveryFakeProvider{
				id: "aaa", callSequence: &calls,
				detail: provider.ArtistDetail{Name: "全局首项", EntityID: "global"},
			})
			f.reg.Register(&discoveryFakeProvider{
				id: "zzz", callSequence: &calls,
				track: tc.track, trackErr: tc.trackErr, idDetailErr: tc.idErr,
				detail: provider.ArtistDetail{Name: "锚定项", EntityID: "anchored"},
			})

			path := "/api/v1/artists/" + url.PathEscape("歌手") + "?track_ref=" + url.QueryEscape("zzz:track")
			rec := discoveryRequest(t, f, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Artist artistProfileResponse `json:"artist"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Artist.Provider != "zzz" || body.Artist.EntityID != "anchored" {
				t.Fatalf("artist = %#v, want anchored name-search fallback", body.Artist)
			}
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %#v, want %#v", calls, tc.wantCalls)
			}
			for i := range tc.wantCalls {
				if calls[i] != tc.wantCalls[i] {
					t.Fatalf("call[%d] = %q, want %q (all = %#v)", i, calls[i], tc.wantCalls[i], calls)
				}
			}
		})
	}
}

func TestArtistProfileTrackRefFallsBackWhenIDCapabilityIsAbsent(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryNameOnlyProvider{
		id: "zzz",
		track: provider.Track{Contributors: []provider.Contributor{
			{Role: "artist", Name: "歌手", EntityID: "artist-42"},
		}},
		detail:       provider.ArtistDetail{Name: "歌手", EntityID: "searched"},
		callSequence: &calls,
	})

	path := "/api/v1/artists/" + url.PathEscape("歌手") + "?track_ref=" + url.QueryEscape("zzz:track")
	rec := discoveryRequest(t, f, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "zzz" || body.Artist.EntityID != "searched" {
		t.Fatalf("artist = %#v, want name-search fallback", body.Artist)
	}
	wantCalls := []string{"track:zzz:track", "name:歌手"}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] || calls[1] != wantCalls[1] {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestArtistProfileValidation(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	rec := discoveryRequest(t, f, "/api/v1/artists/")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = discoveryRequest(t, f, "/api/v1/artists/x?track_ref=invalid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid track_ref status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "bad_request" {
		t.Fatalf("invalid track_ref error = %#v, want bad_request", errBody.Error)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous artist status = %d, want 401", rec.Code)
	}

	listenerToken := f.authm.IssueSession(auth.Identity{
		ID: "bob", Name: "Bob", Kind: "guest", Roles: []string{auth.RoleListener},
	})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/artists/x", nil)
	req.Header.Set("Authorization", "Bearer "+listenerToken)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("listener artist status = %d, want 403", rec.Code)
	}
}

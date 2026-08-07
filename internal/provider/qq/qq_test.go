package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// envelope 构造 sidecar 标准响应。
func envelope(data any) string {
	body, _ := json.Marshal(map[string]any{"code": 0, "msg": "ok", "data": data})
	return string(body)
}

// testProvider 构造指向 httptest server 的 Provider（无 store）。
func testProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	t.Cleanup(server.Close)
	return New(server.URL, defaultFileType, nil)
}

// testProviderWithStore 构造带 store 的 Provider（凭据测试用）。
func testProviderWithStore(t *testing.T, server *httptest.Server) (*Provider, *store.Store) {
	t.Helper()
	t.Cleanup(server.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(server.URL, defaultFileType, st), st
}

const (
	testMid  = "003aQmDc4dRTXQ"
	testName = "晴天"
)

func songFixture(mid, name string, id, interval int64, singerName string) map[string]any {
	return map[string]any{
		"id":       id,
		"mid":      mid,
		"name":     name,
		"type":     1,
		"interval": interval,
		"singer":   []map[string]any{{"mid": "001", "name": singerName}},
		"album":    map[string]any{"mid": "002", "name": "专辑", "pmid": ""},
	}
}

func TestThumbnailCoverURL(t *testing.T) {
	p := &Provider{}
	tests := []string{
		"https://y.gtimg.cn/music/photo_new/T002R300x300M000002.jpg",
		"https://example.com/cover.jpg?size=custom",
		"",
	}
	for _, raw := range tests {
		if got := p.ThumbnailCoverURL(raw); got != raw {
			t.Errorf("ThumbnailCoverURL(%q) = %q, want unchanged input", raw, got)
		}
	}
}

func TestSearchParamsAndMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/search_by_type" {
			t.Errorf("path = %q, want /search/search_by_type", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("search_type"); got != "0" {
			t.Errorf("search_type = %q, want 0", got)
		}
		if got := q.Get("keyword"); got != "query" {
			t.Errorf("keyword = %q, want query", got)
		}
		if got := q.Get("page"); got != "1" {
			t.Errorf("page = %q, want 1", got)
		}
		if got := q.Get("num"); got != "30" {
			t.Errorf("num = %q, want 30", got)
		}
		if got := q.Get("highlight"); got != "false" {
			t.Errorf("highlight = %q, want false", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(envelope(map[string]any{"song": []any{
			songFixture(testMid, testName, 101, 267, "周杰伦"),
			songFixture("002abc", "安静", 102, 190, "周杰伦"),
		}})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	got, err := p.Search(context.Background(), "query", 0, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search() = %d tracks, want 2", len(got))
	}
	first := got[0]
	if first.Ref.String() != "qq:"+testMid || first.Title != testName || first.Artist != "周杰伦" {
		t.Fatalf("first track = %#v", first)
	}
	if first.DurationMs != 267000 {
		t.Fatalf("DurationMs = %d, want 267000", first.DurationMs)
	}
	if first.CoverURL != "https://y.gtimg.cn/music/photo_new/T002R300x300M000002.jpg" {
		t.Fatalf("CoverURL = %q, want deterministic photo_new url", first.CoverURL)
	}
	if first.SourceURL != "https://y.qq.com/n/ryqq/songDetail/"+testMid {
		t.Fatalf("SourceURL = %q", first.SourceURL)
	}
}

func TestSearchPaginationMapping(t *testing.T) {
	// offset=35 limit=30 → page=2 num=30，本地丢弃 5 个前导项。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("page"); got != "2" {
			t.Errorf("page = %q, want 2", got)
		}
		if got := q.Get("num"); got != "30" {
			t.Errorf("num = %q, want 30", got)
		}
		var songs []any
		for i := 0; i < 30; i++ {
			songs = append(songs, songFixture("mid"+string(rune('a'+i)), "s", 1000+int64(i), 100, "a"))
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"song": songs})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	got, err := p.Search(context.Background(), "q", 30, 35)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("Search() = %d tracks, want 25 (30 - 5 dropped)", len(got))
	}
	if got[0].Ref.String() != "qq:midf" { // 索引 5 是丢弃后第一项
		t.Fatalf("first ref = %s, want qq:midf", got[0].Ref)
	}
}

func TestGetTrack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/song/"+testMid+"/detail" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"track": songFixture(testMid, testName, 101, 267, "周杰伦")})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	ref := provider.NewRef(p.ID(), testMid)
	got, err := p.GetTrack(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetTrack() error = %v", err)
	}
	if got.Ref != ref || got.Title != testName || got.Artist != "周杰伦" {
		t.Fatalf("GetTrack() = %#v", got)
	}
}

func TestResolveWithDispatchAndFallback(t *testing.T) {
	var urlCalls, dispatchCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/song/get_cdn_dispatch":
			dispatchCalls++
			// 真实形态：sip 带路径前缀与尾斜杠（purl 无前导斜杠）。
			_, _ = w.Write([]byte(envelope(map[string]any{"retcode": 0, "sip": []string{
				"http://106.119.86.89/amobile.music.tc.qq.com/",
				"https://aqqmusic.tc.qq.com/",
			}})))
		case "/song/get_song_urls":
			urlCalls++
			var body struct {
				FileInfo []struct {
					Mid string `json:"mid"`
				} `json:"file_info"`
				FileType int `json:"file_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			if len(body.FileInfo) != 1 || body.FileInfo[0].Mid != testMid {
				t.Errorf("file_info = %+v, want [{mid:%s}]", body.FileInfo, testMid)
			}
			// 配置档 12(MP3_320) 拿不到 → 回退 13(MP3_128)
			if body.FileType == 12 {
				_, _ = w.Write([]byte(envelope(map[string]any{"expiration": 300, "data": []any{
					map[string]any{"mid": testMid, "filename": "x", "purl": "", "vkey": "", "ekey": "", "result": 104003},
				}})))
				return
			}
			if body.FileType != 13 {
				t.Errorf("file_type = %d, want fallback 13", body.FileType)
			}
			_, _ = w.Write([]byte(envelope(map[string]any{"expiration": 600, "data": []any{
				// 真实形态：purl 无前导斜杠
				map[string]any{"mid": testMid, "filename": "M500x.m3", "purl": "M500" + testMid + ".mp3?guid=1&vkey=vk", "vkey": "vk", "ekey": "", "result": 0},
			}})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)

	loc, err := p.Resolve(context.Background(), provider.NewRef("qq", testMid))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// 直拼契约：sip 尾斜杠 + purl（无前导斜杠）——去掉尾斜杠会拼出 418 畸形 URL。
	wantURL := "http://106.119.86.89/amobile.music.tc.qq.com/M500" + testMid + ".mp3?guid=1&vkey=vk"
	if loc.URL != wantURL {
		t.Fatalf("URL = %q, want %q", loc.URL, wantURL)
	}
	if loc.Format != "mp3" || loc.BitrateKbps != 128 {
		t.Fatalf("loc = %+v, want mp3/128", loc)
	}
	if loc.ExpiresAt.IsZero() || time.Until(loc.ExpiresAt) > 601*time.Second || time.Until(loc.ExpiresAt) < 599*time.Second {
		t.Fatalf("ExpiresAt = %v, want now+600s", loc.ExpiresAt)
	}
	if urlCalls != 2 || dispatchCalls != 1 {
		t.Fatalf("calls: url=%d dispatch=%d, want 2/1", urlCalls, dispatchCalls)
	}
}

func TestResolveAllTiersFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/song/get_cdn_dispatch":
			_, _ = w.Write([]byte(envelope(map[string]any{"retcode": 0, "sip": []string{"u.y.qq.com"}})))
		case "/song/get_song_urls":
			_, _ = w.Write([]byte(envelope(map[string]any{"expiration": 0, "data": []any{
				map[string]any{"mid": testMid, "purl": "", "result": 104003},
			}})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)

	_, err := p.Resolve(context.Background(), provider.NewRef("qq", testMid))
	if err == nil || !strings.Contains(err.Error(), "登录或会员") {
		t.Fatalf("Resolve() error = %v, want login hint", err)
	}
}

func TestLyrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/song/"+testMid+"/lyric" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("trans"); got != "true" {
			t.Errorf("trans = %q, want true", got)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"songid": 101, "lyric": "[00:00.00]词", "trans": "[00:00.00]译"})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	got, err := p.Lyrics(context.Background(), provider.NewRef("qq", testMid))
	if err != nil {
		t.Fatalf("Lyrics() error = %v", err)
	}
	if got.Type != "lrc" || got.LRC != "[00:00.00]词" || got.TLRC != "[00:00.00]译" {
		t.Fatalf("Lyrics() = %#v", got)
	}
}

// ---------- 凭据 ----------

const testCredJSON = `{"openid":"o1","refresh_token":"rt","access_token":"at","expired_at":1700000000,"musicid":12345,"musickey":"MK_TEST","unionid":"u1","str_musicid":"12345","refresh_key":"rk","musickeyCreateTime":1690000000,"keyExpiresIn":8640000,"bindAccountType":2,"needRefreshKeyIn":100,"encryptUin":"EAA%3D","loginType":1}`

func TestParseCredentialJSONNormalizesAliases(t *testing.T) {
	cred, err := parseCredential(testCredJSON)
	if err != nil {
		t.Fatalf("parseCredential() error = %v", err)
	}
	if cred.MusicID != 12345 || cred.MusicKey != "MK_TEST" || cred.RefreshKey != "rk" {
		t.Fatalf("cred = %+v", cred)
	}
	if cred.MusicKeyCreateTime != 1690000000 || cred.KeyExpiresIn != 8640000 ||
		cred.EncryptUin != "EAA%3D" || cred.LoginType != 1 || cred.BindAccountType != 2 {
		t.Fatalf("alias fields not normalized: %+v", cred)
	}
}

func TestParseCredentialCookieString(t *testing.T) {
	cred, err := parseCredential("musicid=12345; musickey=MK_TEST; refresh_key=rk; openid=o1")
	if err != nil {
		t.Fatalf("parseCredential() error = %v", err)
	}
	if cred.MusicID != 12345 || cred.MusicKey != "MK_TEST" || cred.RefreshKey != "rk" || cred.OpenID != "o1" {
		t.Fatalf("cred = %+v", cred)
	}
	if cred.StrMusicID != "12345" {
		t.Fatalf("StrMusicID = %q, want musicid fallback", cred.StrMusicID)
	}
}

func TestParseCredentialRejectsIncomplete(t *testing.T) {
	for _, payload := range []string{"", "musickey=only", "musicid=123", `{"musicid":1}`} {
		if _, err := parseCredential(payload); err == nil {
			t.Errorf("parseCredential(%q) unexpectedly succeeded", payload)
		}
	}
}

func TestCredentialCookieHeader(t *testing.T) {
	cred, err := parseCredential(testCredJSON)
	if err != nil {
		t.Fatal(err)
	}
	header := cred.cookieHeader()
	for _, want := range []string{"musicid=12345", "musickey=MK_TEST", "openid=o1", "refresh_token=rt", "refresh_key=rk"} {
		if !strings.Contains(header, want) {
			t.Errorf("cookie header %q missing %q", header, want)
		}
	}
}

func TestSetCredentialValidatesAndStores(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/refresh_credential":
			refreshCalls++
			if got := r.Header.Get("Cookie"); !strings.Contains(got, "musicid=12345") || !strings.Contains(got, "musickey=MK_TEST") {
				t.Errorf("Cookie = %q, want musicid+musickey", got)
			}
			// 刷新成功：返回轮换后的凭据
			_, _ = w.Write([]byte(envelope(map[string]any{
				"musicid": 12345, "musickey": "MK_REFRESHED", "str_musicid": "12345", "refresh_key": "rk",
				"openid": "o1", "refresh_token": "rt2", "encrypt_uin": "EAA=",
			})))
		case "/user/EAA=/homepage": // r.URL.Path 是解码后的路径
			_, _ = w.Write([]byte(envelope(map[string]any{"base_info": map[string]any{"encrypted_uin": "EAA=", "name": "小明", "avatar": "https://av/1"}})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p, st := testProviderWithStore(t, server)

	if err := p.SetCredential(context.Background(), testCredJSON); err != nil {
		t.Fatalf("SetCredential() error = %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	owner, ok, err := st.GetCredentialOwner(context.Background(), "qq")
	if err != nil || !ok {
		t.Fatalf("GetCredentialOwner: ok=%v err=%v", ok, err)
	}
	if owner.Account.UID != "12345" || owner.Account.Name != "小明" {
		t.Fatalf("account profile = %+v", owner.Account)
	}
	payload, err := st.GetCredential(context.Background(), "qq")
	if err != nil {
		t.Fatal(err)
	}
	// 落盘的是刷新后的凭据（轮换后的 musickey）
	cred, err := parseCredential(payload)
	if err != nil {
		t.Fatalf("stored payload unparseable: %v (%q)", err, payload)
	}
	if cred.MusicKey != "MK_REFRESHED" {
		t.Fatalf("stored musickey = %q, want refreshed MK_REFRESHED", cred.MusicKey)
	}
}

func TestSetCredentialStructuralOnlyWithoutRefreshMaterial(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()
	p, st := testProviderWithStore(t, server)

	// 无 refresh 材料：结构校验后接受，不发任何网络请求
	if err := p.SetCredential(context.Background(), "musicid=12345; musickey=MK_ONLY"); err != nil {
		t.Fatalf("SetCredential() error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
	if status, err := st.GetCredentialStatus(context.Background(), "qq"); err != nil || status != "ok" {
		t.Fatalf("credential status = %q err=%v, want ok", status, err)
	}
}

func TestSetCredentialRejectsInvalidRefresh(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		if r.URL.Path != "/login/refresh_credential" {
			t.Errorf("path = %q, want /login/refresh_credential", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":-1,"msg":"未授权"}`))
	}))
	defer server.Close()
	p, st := testProviderWithStore(t, server)

	if err := p.SetCredential(context.Background(), testCredJSON); err == nil {
		t.Fatal("SetCredential() error = nil, want invalid refresh rejection")
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	// 校验失败不得落盘
	if status, err := st.GetCredentialStatus(context.Background(), "qq"); err != nil || status != "unset" {
		t.Fatalf("credential status after failed set = %q err=%v, want unset", status, err)
	}
}

func TestCredentialStatusExpiredThenRefreshed(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/login/refresh_credential" {
			t.Errorf("path = %q, want /login/refresh_credential", r.URL.Path)
		}
		refreshCalls++
		_, _ = w.Write([]byte(envelope(map[string]any{"musicid": 12345, "musickey": "MK_NEW", "refresh_key": "rk", "str_musicid": "12345"})))
	}))
	defer server.Close()
	p, st := testProviderWithStore(t, server)

	cred, err := parseCredential(testCredJSON)
	if err != nil {
		t.Fatal(err)
	}
	// testCredJSON 的 key 字段已过期（create_time + expires_in < now）
	p.cred.Store(cred)

	if got := p.CredentialStatus(context.Background()); got != "ok" {
		t.Fatalf("CredentialStatus() = %q, want ok (refreshed)", got)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if got := p.cred.Load().MusicKey; got != "MK_NEW" {
		t.Fatalf("credential musickey after refresh = %q, want MK_NEW", got)
	}
	if payload, err := st.GetCredential(context.Background(), "qq"); err != nil || !strings.Contains(payload, "MK_NEW") {
		t.Fatalf("stored payload = %q err=%v, want refreshed musickey", payload, err)
	}
}

// ---------- 二维码登录 ----------

func TestQRLoginStartAndPoll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/qrcode/qq":
			_, _ = w.Write([]byte(envelope(map[string]any{"qr_type": "qq", "identifier": "id-1", "mimetype": "image/png", "data": qrPNGFixture, "img": "data:image/png;base64," + qrPNGFixture})))
		case "/login/qrcode/qq/status":
			if got := r.URL.Query().Get("identifier"); got != "id-1" {
				t.Errorf("identifier = %q, want id-1", got)
			}
			// 一次 done 带凭据
			_, _ = w.Write([]byte(envelope(map[string]any{"event": 0, "done": true, "identifier": "id-1", "login_type": "qq", "credential": map[string]any{"musicid": 12345, "musickey": "MK_TEST", "str_musicid": "12345", "refresh_key": "rk"}})))
		case "/login/refresh_credential":
			// QR 凭据带 refresh_key → SetCredential 走 refresh 探针
			_, _ = w.Write([]byte(envelope(map[string]any{"musicid": 12345, "musickey": "MK_TEST", "str_musicid": "12345", "refresh_key": "rk"})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p, _ := testProviderWithStore(t, server)

	key, qr, err := p.QRLoginStart(context.Background())
	if err != nil {
		t.Fatalf("QRLoginStart() error = %v", err)
	}
	// qrContent 必须是扫码内容文本（客户端按 ncm/bili 契约把它编码成二维码），
	// 绝不能是 data URL——手机端会把 "data:image/..." 当文本提示而不是授权。
	if key != "id-1" || qr != qrTextFixture {
		t.Fatalf("QRLoginStart() = %q %q, want %q", key, qr, qrTextFixture)
	}
	status, msg, err := p.QRLoginPoll(context.Background(), key)
	if err != nil {
		t.Fatalf("QRLoginPoll() error = %v", err)
	}
	if status != "ok" || msg == "" {
		t.Fatalf("QRLoginPoll() = %q %q, want ok", status, msg)
	}
	if got := p.cred.Load().MusicKey; got != "MK_TEST" {
		t.Fatalf("credential after QR = %q, want MK_TEST", got)
	}
}

func TestDecodeQRContent(t *testing.T) {
	got, err := decodeQRContent(qrPNGFixture)
	if err != nil {
		t.Fatalf("decodeQRContent() error = %v", err)
	}
	if got != qrTextFixture {
		t.Fatalf("decodeQRContent() = %q, want %q", got, qrTextFixture)
	}

	// data: URL 前缀兼容
	got, err = decodeQRContent("data:image/png;base64," + qrPNGFixture)
	if err != nil || got != qrTextFixture {
		t.Fatalf("decodeQRContent(data url) = %q err=%v", got, err)
	}

	for _, bad := range []string{"", "not-base64!!", "aGVsbG8=" /* 合法 base64 但非 PNG */} {
		if _, err := decodeQRContent(bad); err == nil {
			t.Errorf("decodeQRContent(%q) unexpectedly succeeded", bad)
		}
	}
}

// qrTextFixture 是二维码 PNG 中编码的内容（腾讯授权 URL 形态）。
const qrTextFixture = "http://txz.qq.com/p?k=FIXTUREKEY123&f=1"

// qrPNGFixture 是内容为 qrTextFixture 的 111x111 PNG（base64）。
const qrPNGFixture = "iVBORw0KGgoAAAANSUhEUgAAAXIAAAFyAQAAAADAX2ykAAACe0lEQVR4nO2bTW6cQBBGX6WRZgnSHMBHgRvkSFGOlBvQR/EBLNFLS6Avi+6GmdiWbWU8gahqMYLhLUoqfV1/YOIzFr99CgfnnXfeeeedd/4t3oo1EDuAZAapyVfZhjv64/yN+V6SNIENqaHEksVsIEiSdM1/tT/O35hPRaHStFh9GKQxq7u5tz/OfxEfuyCN7ZxP6qrkf+eP83/FN3/cW6/FFL8/YbAp+W7+OH9bvsa3FZBAAJb/b5/y08sRyN78d/5DfDQzsw5sSCfRT0H0jyflIsvM7Jr/an+cvxGPXrFaWoUXT8a9+e/8R/jSFaUmK1kjQK/nXFPbsGXiffrv/JtWdJmvZqQJsn41hXxSa2yl3CK7fg/G5wrK+uk8GwQBYYZ0xvpfINJi9I9rIb03/51/x6p+a9fbr/otqp3R2M54/j0kn+NWo1qtn4K247oE3uN7WF4/H9YIrkqOdhKxC4J08vnzsfl0UlkYQVki9ZqxIUv32evnY/K1/51CrZApoc1ZdyQIPP8elb+sr7Qm4X4baFAyseffQ/IX8yuoBTO0JbQF2dS9N/+df8e2+lmqs4x8tR7cXj8fl6/nc1t+YFXtSKiNk+ffo/JVvyqjjTLLmMq4MttFOt6b/86/Yy9WRxdDrBLafOvn8xH5Mn8ud2EWqUHxQRAHgHSeiQ9z7Yz35r/zH+LX9yehnbEf02J5tFHkHHx+dWy+vj+Zl8BQVoNEMyN2vv/9T/g8iYbFiN0r24e9++/8tb14f5I2iNhNApZGsDSQzrL7+OP8bfm1P3plqgGlZ8qbQq+fD8hz+fHJtkuYypKhTCp9fnVU3vz7buedd955552/O/8bga4YqD+xMokAAAAASUVORK5CYII="

func TestQRLoginStatusMapping(t *testing.T) {
	cases := []struct {
		event  int
		status string
		err    bool
	}{
		{event: 1, status: "waiting"},
		{event: 2, status: "scanned"},
		{event: 3, status: "expired"},
		{event: 4, err: true},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(envelope(map[string]any{"event": tc.event, "done": tc.event == 0, "identifier": "k", "login_type": "qq"})))
		}))
		p := testProvider(t, server)
		status, _, err := p.QRLoginPoll(context.Background(), "k")
		if tc.err {
			if err == nil {
				t.Errorf("event %d: error = nil, want error", tc.event)
			}
		} else if err != nil || status != tc.status {
			t.Errorf("event %d: status=%q err=%v, want %q", tc.event, status, err, tc.status)
		}
		server.Close()
	}
}

func TestInterfaceSatisfaction(t *testing.T) {
	p := &Provider{}
	var (
		_ provider.CredentialAware  = p
		_ provider.QRLoginAware     = p
		_ provider.LyricsProvider   = p
		_ provider.CategorySearcher = p
		_ provider.SourceCatalog    = p
		_ provider.PlaylistImporter = p
		_ provider.AccountWriter    = p
	)
	// PlayReporter 明确不实现（QQ 无上报端点）。
}

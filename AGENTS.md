# Yuzu Jukebox — Agent Guide

## What It Is

Music jukebox server: multi-room, multi-provider, queue-based playback with real-time WebSocket push. Written in Go, standard library `net/http`, SQLite backend.

Four binaries:
- `yuzu-server` — the HTTP+WS server
- `yuzu-cli` — CLI control and administration client
- `yuzu-agent` — MPV-based headless playback renderer (player plane)
- `yuzu-edgeone` — optional EdgeOne publisher adapter (media CDN offload)

## Directory Layout

```
cmd/
  yuzu-server/      — main() entry, loads config, assembles deps via app.New()
  yuzu-cli/         — CLI client
  yuzu-agent/       — headless player (MPV)
  yuzu-edgeone/     — EdgeOne publisher adapter (optional, CDN offload)
internal/
  app/              — dependency assembly (app.New), returns http.Handler + store
  auth/             — session management, guest/OIDC auth, ticket auth, roles
  cache/            — LRU disk cache for streaming audio
  client/           — Go reference implementation of WS protocol
  control/           — shared room command/query service and room-scoped authorization
  config/           — Config struct, JSON deserialization, defaults
  credmon/          — hot-reload credential monitor for providers
  distribution/     — acceleration service: leases, candidates, capacity & GC governance
  edgeonepublisher/ — EdgeOne adapter: upload, inventory, deletion client
  httpapi/          — REST handlers: /api/v1/*, /stream/v1/*, /api/v1/cover/*
  shortcode/        — Crockford Base32 short-code helpers
  wsapi/            — WebSocket handler: /ws/v1
  provider/         — Track provider abstraction + registry
    local/          — local media file provider
    ncm/            — NeteaseCloudMusicApi provider
    bili/           — bilibili-api provider
  room/             — room actor: queue, playback state machine, radio mode, broadcast
  store/            — SQLite via database/sql + modernc, goose migrations
data/
  yuzu.db          — SQLite database
  cache/           — audio cache files
  media/           — uploaded media files (local provider)
```

## Data Flow (Key Paths)

```
Web Client ←→ REST + /ws/v1 (auth → room join/control → real-time broadcasts)
                ↓
Admin/CLI   ←→ /api/v1/* (rooms, media, integrations, grants)
                ↓
IM Gateway  ←→ Integration credential → actor resolve → standard Room REST/WS
                ↓
Renderer    ←→ /stream/v1/{ref}?ticket=...  (HTTP audio streaming, Range supported)
                ↓
Cover       ←→ /api/v1/cover/{ref}  (proxy to provider cover, unauthenticated)
```

### Authentication Flow

Authoritative contract: [docs/spec-v1.md](docs/spec-v1.md) §3 (auth) and §4.6 (stream tickets). Quick reference:
- Guest/OIDC login issues a `session_token` used as `Authorization: Bearer <token>` for REST; WS accepts the same token to establish a connection identity.
- Streams use ticket auth in the query param (for `<audio>` tags, no headers); covers need **no auth** (for `<img>` tags).
- An Integration credential only calls `POST /api/v1/integrations/actors/resolve` to map an external scope/subject to a 5-minute actor session; it carries no Yuzu roles, and authorization always reloads the current Principal and Room grant.

### Room → Playback Flow

Authoritative sequencing: [docs/spec-v1.md](docs/spec-v1.md) §4 (room session) and §5 (state machine). Quick reference:
- After WS `room.join`, the server pushes current state: `playback.changed`, revisioned `queue.snapshot`/`queue.patch`, `radio.changed`, `listeners.changed`.
- The current track arrives via `playback.changed` with an identity-bound `stream_url` (ticket); the next track is pre-resolved and pre-cached when the current one enters PLAYING.

## Code Conventions

### HTTP Handlers
- Registered in `httpapi.Server.Handler()` using Go 1.22+ `http.ServeMux` patterns:
  ```go
  mux.HandleFunc("GET /api/v1/rooms", s.listRooms)
  mux.HandleFunc("PATCH /api/v1/rooms/{id}", s.updateRoom)
  ```
- Path vars via `r.PathValue("id")`
- Error response format:
  ```json
  {"error": {"code": "not_found", "message": "room not found"}}
  ```
- Public/auth-special endpoints are explicit exceptions. Standard REST uses session Bearer auth; actor resolve uses an Integration Bearer credential; management routes require `room_admin`.

### WebSocket Protocol
- JSON text frames with envelope: `{"type": "...", "ref": "...", "data": {...}}`
- ref is client-generated (loop counter or UUID), echoed back by server
- Broadcast messages (playback/queue/listeners.changed) have no ref
- See `docs/spec-v1.md` for full protocol spec

### Integration Boundary
- Integration records and token hashes are persistent database resources managed by `room_admin`; plaintext tokens are returned only on create/rotation.
- External `(integration, adapter, scope)` maps to a default Room; adding `subject` maps to a persistent Principal.
- Unlinked subjects resolve to stable synthetic guest Principals. Linked OIDC Principals share the same roles and Room grants as WebUI/CLI sessions.
- Integration adapters translate platform events into standard Room REST/WS. There is no platform-specific `/im/v1` protocol in the server.
- External write retries use `Idempotency-Key`; never invent client-side deduplication from song metadata or message text.

### WS Handler Initialization
```go
ws := wsapi.NewServer(authm, playerAuth, controlService, playerBindings)
// mount via mux.Handle("/ws/v1", sws)
```

## Configuration (`config.json`)

```json
{
  "addr": ":8080",
  "admin_password": "...",
  "secret_key": "...",
  "oidc": {
    "enabled": true,
    "issuer": "https://sso.example.com",
    "client_id": "...",
    "role_mapping": {"admin": ["room_admin", "media_admin"]}
  },
  "cors": {
    "enabled": true,
    "allowed_origins": ["*"]
  },
  "ncm": {
    "enabled": true,
    "base_url": "http://127.0.0.1:3000",
    "level": "exhigh"
  },
  "bili": {
    "enabled": true,
    "base_url": "http://127.0.0.1:3002"
  },
  "qq": {
    "enabled": true,
    "base_url": "http://127.0.0.1:8080",
    "file_type": 12
  }
}
```

All fields optional if not needed. `secret_key` auto-generated on first run.
`qq.file_type`: 明文音质档位 0-16（12=MP3_320 默认 / 13=MP3_128 / 14=AAC_192 / 7=FLAC）；17+ 为加密格式，播放器无法解码。

## Database

- **Driver**: `database/sql` + `modernc.org/sqlite` (pure Go, no CGO; WAL, busy_timeout, foreign_keys pragmas)
- **Migrations**: goose, in `internal/store/migrations/`
- **Key tables** (grouped):
  - Identity/sessions/credentials: `users`, `sessions`, `credentials`
  - Rooms/queue/media: `rooms`, `room_queue`, `play_history`, `media_files`, `media_cache`, `audit_log`
  - Playlists: `playlists`, `playlist_items`
  - Integrations/grants: `integrations`, `external_scope_rooms`, `external_identity_links`, `external_binding_codes`, `room_principal_grants`, `idempotency_records`
  - Players: `players`, `room_player_bindings`, `room_output_state`
  - Acceleration: `accelerations`, `distribution_*`, `acceleration_*`
- **Secrets**: provider credentials use AES-GCM with `secret_key`; Integration tokens are high-entropy bearer credentials stored only as hashes and shown once.

## Build & Run

```bash
go build -o bin/yuzu-server  ./cmd/yuzu-server
go build -o bin/yuzu-cli     ./cmd/yuzu-cli
go build -o bin/yuzu-agent   ./cmd/yuzu-agent
go build -o bin/yuzu-edgeone ./cmd/yuzu-edgeone   # optional: EdgeOne media offload
./bin/yuzu-server -config config.json
```

## External Dependencies (Sidecars)

- **ffprobe/ffmpeg**: yuzu-server 的 local provider 上传媒体时用它做容器校验/标签提取/封面抽取（`ffprobe` 必需；缺失时上传返回 400 并提示安装）。发行版安装 `ffmpeg` 包即可提供两者。
- **NCM provider**: requires [NeteaseCloudMusicApi](http://127.0.0.1:3000) running (config `ncm.enabled` + `ncm.base_url`)
- **Bili provider**: requires [bilibili-api](http://127.0.0.1:3002) sidecar (config `bili.enabled` + `bili.base_url`)
- **QQ provider**: requires [QQMusicApi](http://127.0.0.1:8080) web sidecar (config `qq.enabled` + `qq.base_url`; run `uv sync --group web --no-dev && uv run --no-sync web/run.py` in the QQMusicApi repo). Sidecar defaults: `security.enabled=true` with IP-based rate limit 60/min/IP — whitelist the yuzu host via `rate_limit_exempt_ips` in its `config.toml` (or rely on yuzu's audio cache dedup, which resolves each track once).

## Roles

| Role | Capability |
|---|---|
| `listener` | See room state, receive playback |
| `requester` | Add to queue, remove own entries |
| `room_admin` | Manage rooms, integrations and grants; controller in every Room |
| `media_admin` | Manage media, upload, provider credentials |
| `sys_admin` | Manage the acceleration/distribution control plane: create/modify/delete accelerations, control/backend URLs, delivery credentials, inventory refresh. Granted to password-authenticated admins and via OIDC `role_mapping`; deliberately NOT granted to `media_admin` (credentialed-SSRF surface) |
| Room grant `controller` | Control playback, radio and queue ordering in one Room; not a global role |

## Identity Follow-ups

Keep these separate from Integration credential lifecycle work.

Done:
1. **OIDC self-service external-subject binding** — implemented (spec-v1 §3.7; `POST /api/v1/auth/external-binding-codes` + `POST /api/v1/integrations/bindings/redeem`; migration 0008).
2. **Principal and grant administration queries** — implemented: principal list (`GET /api/v1/principals`), Integration scope/subject binding management, and Room controller grant/revoke.
3. **OIDC avatar passthrough** — `picture` claim rides the preferred_username path (id_token + userinfo backfill via `OIDCClaims.ApplyUserinfo`; userinfo fills only when id_token lacks it), surfaces as `identity.avatar` on login and persists to `users.avatar` (migration 0027) shown in `GET /api/v1/principals`. Zitadel strips profile claims from id_token by default (`IDTokenUserinfoAssertion` flag off), so the access_token + userinfo path is the reliable source.

Still planned:
1. **Principal lifecycle administration** — first-party disable/enable and bulk external-access revocation have no admin endpoints yet; the persisted `active` flag already enforces disabled Principals, but no complete admin workflow exists.
2. **OIDC role refresh/revocation strategy** — roles refresh when OIDC login updates the Principal; IdP-side removal is not pushed immediately. Choose webhook/SCIM/periodic refresh later rather than implying real-time revocation.

## Agent Caveats

- **Cover endpoint is unauthenticated** by design (`/api/v1/cover/{ref}`) — for `<img>` tags that can't send auth headers. Entity covers (artist/album/playlist, no TrackRef) use `/api/v1/cover/ext/{token}` with a server-minted HMAC token (secret_key-derived) — never a url-passthrough proxy (SSRF). All search/drill responses rewrite `cover_url` to proxy paths at the serialization layer; keep the invariant "clients never see raw source URLs" when adding new track/entity surfaces
- **Cover fetch mode is provider-declared** (`provider.CoverModeAware`, `CoverModeProxy`/`CoverModeRedirect`): ncm/qq declare Redirect (302 to source, no Referer needed — saves server bandwidth), bili declares Proxy (CoverAware: hdslb needs Referer). Decision priority in `httpapi.Server.coverMode`: CoverAware always proxies (302 would drop required headers) → declared mode wins → undeclared providers follow the global `ncm.cover_direct` default. `proxyCover` is the single fetch/redirect implementation for both track and entity covers; local covers bypass it (served from `cover_path` on disk)
- **Stream endpoint uses ticket auth** (`?ticket=...`) — for `<audio>` tags. Tickets are 5-minute TTL bound to ref + identity
- **CORS**: methods/headers/max-age are hardcoded in `httpapi.corsMiddleware`, only origins configurable
- **WS state machine**: playback position is computed from `position_ms + updated_at + playing` — never stored as a continuous counter
- **`position_ms` can be negative**: on track switch the room schedules position 0 at `updated_at + start_lead_ms` (room policy `start_lead_ms`, default 600ms) so clients can load and all start on time without losing the head of the track. A negative computed `should_be` means "starts in |x| ms" — clients load-and-pause, skip drift correction, and clamp to 0 when rendering. See `docs/spec-v1.md` §2.2
- **Room actor**: per-room goroutine. Queue entries persisted in SQLite. Radio mode state is runtime-only (lost on restart)
- **Unplayable tracks auto-skip**: when a track becomes the current, the room asynchronously preflights it (cache hit → pass; otherwise `provider.Resolve`). Resolve failure — e.g. QQ Music's 104003 without a credential, or a missing local file — reports back to the actor, which advances with `end_reason="unplayable"` instead of stalling until the duration timer fires. The report carries the ref and is ignored if the user already skipped away (race guard). A queue of uniformly unplayable tracks drains quickly; radio keeps refilling, so a broken radio source spins without deadlock. `Cache.Prefetch` (next-track warm-up) stays silent-by-design — the preflight is the failure detector for the *current* track
- **Current track stays in the queue**: `room_queue` holds the playing entry plus everything upcoming; `is_current` marks the cursor. The wire format is unchanged — `queue.snapshot`/`queue.patch` still carry upcoming entries only, and the playing entry is delivered via `playback.changed`. Persisting it means the playing track is queryable in SQL (acceleration pins the object being streamed) and a restart resumes that track instead of skipping to the next one. `position_ms` is still runtime-only, so resume starts from the head
- **TrackRef format**: `provider:id` — opaque string below the API layer (e.g. `ncm:347230`, `bili:BV1xx`, `qq:<song mid>`, `local:<uuid>`)
- **All timestamps**: Unix milliseconds (UTC)

### Provider Credential Ownership
- **Singleton owner**: a provider credential is server-wide; every human-driven credential write (direct set or successful QR login) re-binds it to the writing Principal. Credential monitoring and row rotation preserve the owner/account binding.
- **Whitelisted writes only**: interactive like/playlist-add requests require the acting Principal to equal the credential owner; automatic play reporting instead requires the track's requester to be that owner. These are the complete whitelist—never expose a generic NCM endpoint/cookie proxy; the cookie stays server-side.
- **Scrobble rule**: report only tracks personally requested by the owner, after `played_ms >= min(total_ms/2, 240000)`; unknown duration uses 240000 ms. Reporting is fire-and-forget, audited, and never blocks playback.
- **Per-principal discovery**: `owned` in `GET /api/v1/providers` is computed for the requesting Principal and must never be cached across users. The `account` block (`name`/`avatar`, from the credential row's `account_*` snapshot) is emitted **only to the credential owner** and deliberately excludes the platform UID — the client has no consumer for it, and leaking it would reveal which external account the server is logged in as. Empty snapshots (no name/avatar) omit the block.

### Roaming Experience Backend (WebUI home feed)
- **Radio authorization is policy-driven**: room policy `radio_control` (`"controller"` default | `"requester"`) widens `radio.play`/`radio.stop` beyond controllers; REST and WS share the single path in `control.Service.radioRoom`. Clients gate the radio button on `GET /api/v1/rooms/{id}/capabilities` → `radio`, never on local role guesses.
- **Cross-room history**: `GET /api/v1/history?requester=me` is caller-only (no admin override); backed by index `idx_play_history_requester` (migration 0024). Rows carry `artist` (snapshot from the queue entry, migration 0030).
- **Global hot feed**: `GET /api/v1/stats/hot?days=&limit=` aggregates `play_history` across rooms; skipped tracks still count (intent signal) — do not filter by `end_reason`. Entries carry `artist`.
- **Artist entity resolution**: `GET /api/v1/artists/{name}?track_ref=` does not aggregate `play_history`; it resolves the requested name through `provider.ArtistDetailer`. A valid `track_ref` anchors its Provider first, then failures fall back to `registry.All()` in ID order. Success returns `name` plus optional `provider`/`entity_id`/proxied `avatar_url`/`bio`; total failure returns only `name`. Clients use `provider` + `entity_id` with `GET /api/v1/search/entity?provider=<provider>&category=artist&id=<entity_id>` for drill-down. Invalid `track_ref` and empty names are 400.
- **Recommendations feed**: `GET /api/v1/recommendations` aggregates shelves from all `provider.RecommendationProvider` implementers (ncm: `/toplist` top 3 → `/playlist/track/all`, 10 tracks/shelf — **never `/toplist/detail`**, whose `tracks[]` is only `{first,second}` summaries in enhanced v4.39.0 and would yield shell tracks). Per-provider failures are logged and skipped; no source → `{"shelves":[]}` 200. `provider.Registry.All()` is sorted by ID — callers depend on deterministic provider order (enrichment priority, feed aggregation, listProviders response).
- **Radio sources**: `capabilities.radio_sources` in `/api/v1/providers` comes from the optional `provider.SourceCatalog` interface; `finite=false` sources reject `shuffle`/`once`. `GET /api/v1/radio/catalog` is the client-facing aggregate: provider ID order, then each provider's finite no-arg static sources before concrete `provider.RadioSourceCatalogLister` entries; specs are fully prefixed, covers use entity-cover proxies, and one lister failure is logged/skipped without a 5xx or losing other entries. Empty is always `{"entries":[]}`. Template-arg and infinite sources plus the room-level generic `playlist:<id>` are deliberately excluded. Provider-scoped dynamic catalogs remain at `GET /api/v1/providers/{id}/radio-catalog`: qq maps `/top/get_category` to `top:<id>` with covers; bili supplies anonymous coverless `ranking:music`, `ranking:kichiku`, and `ranking:all` instances whose tracks come from `/region-ranking` (no WBI). NCM's anonymous finite `newsong` comes from `/personalized/newsong`.
- **Track description**: optional `provider.Track.Description` rich field, populated only where zero-cost — qq `info.intro[].value|content` from the detail call it already makes, bili `desc` from `/detail`. NCM song wiki (`/song/wiki/summary`, anonymous, deeply nested blocks/creatives/resources) is NOT wired into the GetTrack hot path.

### Categorized Search
- **Capability-driven**: `provider.CategorySearcher` (song/artist/album/playlist) is advertised via `capabilities.search_categories`; clients render only reported tabs. Asymmetry is correct — bili has no album/playlist tabs, local has no categories.
- **Two-level model**: category search returns discriminated `SearchResult` entities (song wraps a queueable `track`; artist/album/playlist carry `entity_id`); drill = `GET /api/v1/search/entity` (artist/album only). All search/drill endpoints take `limit` (default 30, max 100) + `offset` — upstream-paged where available (ncm `/search`, `/artist/album`; bili `/search`, `/space/videos`, sidecar `/search/up?pn=`), locally sliced `[offset:offset+limit]` where not (ncm artist/top/song, `/album`). Artist→albums drill = `?into=albums` (returns album entities, drillable again to tracks — endpoint stays normalized); capability via `capabilities.entity_albums` (`provider.EntityAlbumLister`), bili does not implement it. Playlist entities have three paths by role: media_admin imports/binds them; any radio-allowed caller plays them as `ncm:playlist:<id>` / `bili:fav:<media_id>` sources; small entities can be looped into `queue.add` client-side.
- **Legacy path is frozen**: `/api/v1/search` without `category` (or `category=song`) keeps the exact `{"tracks":[...]}` contract — app_test.go pins it.
- **Bili sidecar contract**: `/search/up`, `/space/videos`, `/fav/resource/list`, `/fav/folders` live in the bilibili-api sidecar (WBI signing there); yuzu parses snake_case fields, normalizes protocol-relative covers, and requires a login cookie for favorites import. The sidecar has no folder-cover endpoint yet — bili `ImportPlaylist` returns an empty coverURL (a `/fav/folder/info` sidecar endpoint would close the gap).
- **QQ sidecar contract**: QQMusicApi's web layer wraps every response in `{code:0,msg,data}`; success payloads serialize with pydantic field names (snake_case) — except `Credential`, which serializes six fields with their aliases (`musickeyCreateTime`, `keyExpiresIn`, `bindAccountType`, `needRefreshKeyIn`, `encryptUin`, `loginType`) because the app does not set `response_model_by_alias=false`. The `qq` provider normalizes these at parse time (`qqCredential.UnmarshalJSON` camel→snake) and sends credentials back as cookies (`musicid`+`musickey` pair, snake_case names, exactly what sidecar `auth.py` reads). TrackRef = `qq:<mid>` (song mid, stable across search/detail/lyric/url); account writes need the numeric id, re-read via detail.
- **QQ anonymous playback is blocked**: the sidecar returns `result=104003` (无权限) for ANY unauthenticated stream request — QQ Music has no guest tier. `Resolve` tries the configured tier then MP3_320→MP3_128 and errors with the standard "可能需要登录或会员" message when all fail. Search/detail/lyrics/covers work anonymously. This is a real behavioral difference from ncm (which serves low quality anonymously).
- **QQ quality tiers**: `file_type` integers 0–16 are plain formats (12=MP3_320 default, 13=MP3_128, 14=AAC_192, 7=FLAC); 17+ are encrypted (need `ekey` decryption, unusable by players) — `qq.New` clamps out-of-range to the default and `Resolve`'s fallback chain only walks plain tiers. Stream URLs are **`sip_entry + purl` concatenated verbatim** — sip entries keep their trailing slash (`http://106.119.86.89/amobile.music.tc.qq.com/`) and purl has no leading slash (`M800xxx.mp3?guid=…`). Trimming the sip slash fuses the path prefix and yields a malformed URL that Tencent's CDN answers with **418** (verified live). The host comes from `/song/get_cdn_dispatch` `sip[]` (30-min cached, `https://isure.stream.qqmusic.qq.com/` fallback), and `expiration` seconds maps to `StreamLocator.ExpiresAt`. CDN downloads need no special headers (bare Go UA downloads fine).
- **QQ credential validation is asymmetric**: sidecar `check_expired` and `get_vip_info` both accept garbage musickey (return null/zeros), only `refresh_credential` discriminates (401 on invalid) — but it rotates the key. `SetCredential` therefore refreshes-and-stores when the credential carries `refresh_token`/`refresh_key` (QR logins always do), and falls back to structural validation (musicid+musickey present) for minimal admin-pasted cookies. `CredentialStatus` does a local expiry check via `musickey_create_time`+`key_expires_in` before attempting refresh.
- **QQ QR content must be decoded from the PNG**: unlike ncm/bili (upstream returns the scannable text URL), the QQ sidecar only returns an already-encoded QR PNG — the content (a `http://txz.qq.com/p?k=...` authorization URL) is never exposed as text, and the `identifier` is only the poll key. `QRLoginStart` decodes the PNG with gozxing so `qr_content` matches the ncm/bili text contract; returning the raw data URL would make phones treat "data:image/..." as a text notice instead of logging in. Decode failure is a hard error (retry gets a fresh code).
- **QQ account ops**: `AccountPlaylist.ID` is a composite `"tid:dirid"` (QQ's add_songs needs both the diss TID and folder dirid; dirid 201 is the fixed "我喜欢" list used by Like). `LikeCheck` pulls the full fav list (`/user/{euin}/fav/songs`, paged) since QQ has no per-song like-state endpoint. No `PlayReporter` — QQ Music exposes no scrobble endpoint, so the provider intentionally doesn't implement it (capability simply absent, room treats it as no-op).
- **Playlist covers**: `playlists.cover_url` stores the FINAL proxy path, never a raw source URL — uploaded covers become `/api/v1/cover/playlist/{id}` (+ `cover_path` on disk), external bound covers are HMAC-minted `/api/v1/cover/ext/<token>` at sync time (survives detach; empty provider covers preserve the previous one). Upload on a bound playlist is 409. `coverurl.Signer` is the single mint/verify implementation shared by httpapi and plsync.
- **Playlist metadata**: `PATCH /api/v1/playlists/{id}` (media_admin) updates `name`/`description`/`pinned` as a partial patch (migration 0031 `playlists.pinned`). Bound playlists reject rename (409 `playlist_bound` — the remote owns the name; detach first) but allow description/pin (local state; sync never touches them). `GET /api/v1/playlists` orders pinned-first, then by creation time.
- **Playlist ownership**: write ops (items add/remove/move, cover set/clear, meta PATCH, delete, detach) authorize `created_by == principal || media_admin`; create requires a non-Guest identity (kind `password`/`oidc`/`player` — guest and integration synthetic guests are excluded). `bind`/`sync`/`import` keep `media_admin` (external-credential surface). Playlist visibility stays global — no private playlists. Error order: 401 → 404 → 403.

### Provider-Bound Playlists
- **Read-only followers**: bound playlists (`bound_provider`+`bound_remote_id`) mirror the external playlist; item mutations return 409 `playlist_bound` — `detach` keeps the snapshot and converts to a normal playlist. Deleting the whole playlist is always allowed.
- **Sync is never destructive**: failure keeps the last good snapshot and records `last_sync_error`; success replaces items in one transaction. `plsync.SyncOne` is the single sync path for both the periodic worker (`playlist_sync_interval_minutes`, default off) and `POST /playlists/{id}/sync`.
- **Sync rides the credential**: fetching uses the singleton provider credential, so a bound playlist implicitly depends on credential validity — and a private playlist (e.g. NCM 我喜欢的音乐) is only syncable while the owning credential is valid.
- **Account read-backs are owner-gated**: `account-playlists` and `like-check` use the same `requireOwnedAccountWriter` gate as the write endpoints; the NCM account uid comes from the credential row's `account_uid` snapshot, never from the client.
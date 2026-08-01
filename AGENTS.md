# Yuzu Jukebox — Agent Guide

## What It Is

Music jukebox server: multi-room, multi-provider, queue-based playback with real-time WebSocket push. Written in Go, standard library `net/http`, SQLite backend.

Three binaries:
- `yuzu-server` — the HTTP+WS server
- `yuzu-cli` — CLI control and administration client
- `yuzu-agent` — MPV-based headless playback renderer (player plane)

## Directory Layout

```
cmd/
  yuzu-server/      — main() entry, loads config, assembles deps via app.New()
  yuzu-cli/         — CLI client
  yuzu-agent/       — headless player (MPV)
internal/
  app/              — dependency assembly (app.New), returns http.Handler + store
  auth/             — session management, guest/OIDC auth, ticket auth, roles
  cache/            — LRU disk cache for streaming audio
  client/           — Go reference implementation of WS protocol
  control/           — shared room command/query service and room-scoped authorization
  config/           — Config struct, JSON deserialization, defaults
  credmon/          — hot-reload credential monitor for providers
  httpapi/          — REST handlers: /api/v1/*, /stream/v1/*, /api/v1/cover/*
  wsapi/            — WebSocket handler: /ws/v1
  provider/         — Track provider abstraction + registry
    local/          — local media file provider
    ncm/            — NeteaseCloudMusicApi provider
    bili/           — bilibili-api provider
  room/             — room actor: queue, playback state machine, radio mode, broadcast
  store/            — SQLite via sqlx, goose migrations
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

1. Guest: `POST /api/v1/auth/guest` with password (optional admin password) → session_token
2. OIDC: `POST /api/v1/auth/oidc` with id_token → session_token
3. Session token used as `Authorization: Bearer <token>` for REST
4. Stream: ticket-based auth in query param (for `<audio>` tags, no headers)
5. Cover: **no auth required** (for `<img>` tags)
6. Integration: a long-lived hashed machine credential resolves an external scope/subject to a 5-minute actor session
7. Authorization always reloads the current Principal and Room grant; Integration credentials do not carry Yuzu roles

### Room → Playback Flow

1. Client joins room via WS (`room.join` with room_id + optional password)
2. Server pushes 5 snapshots: `room.joined`, `playback.changed`, `queue.changed`, `radio.changed`, `listeners.changed`
3. Client adds to queue (`queue.add` with track_ref)
4. Room actor: dequeue → Resolve track → cache → push to renderer
5. Current track gets a signed `stream_url` with ticket
6. Next track: pre-resolved when current enters PLAYING state

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
ws := wsapi.NewServer(authm, rooms, reg)
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
  }
}
```

All fields optional if not needed. `secret_key` auto-generated on first run.

## Database

- **Driver**: `github.com/jmoiron/sqlx` (sqlite3)
- **Migrations**: goose, in `internal/store/migrations/`
- **Key tables**: `sessions`, `principals/users`, `integrations`, `external_scope_rooms`, `external_identity_links`, `room_principal_grants`, `idempotency_records`, `rooms`, `room_queue`, `playlists`, `playlist_items`, `credentials`
- **Secrets**: provider credentials use AES-GCM with `secret_key`; Integration tokens are high-entropy bearer credentials stored only as hashes and shown once.

## Build & Run

```bash
go build ./cmd/yuzu-server           # → bin/yuzu-server
go build ./cmd/yuzu-cli              # → bin/yuzu-cli
go build ./cmd/yuzu-agent            # → bin/yuzu-agent
./bin/yuzu-server -config config.json
```

## External Dependencies (Sidecars)

- **NCM provider**: requires [NeteaseCloudMusicApi](http://127.0.0.1:3000) running (config `ncm.enabled` + `ncm.base_url`)
- **Bili provider**: requires [bilibili-api](http://127.0.0.1:3002) sidecar (config `bili.enabled` + `bili.base_url`)

## Roles

| Role | Capability |
|---|---|
| `listener` | See room state, receive playback |
| `requester` | Add to queue, remove own entries |
| `room_admin` | Manage rooms, integrations and grants; controller in every Room |
| `media_admin` | Manage media, upload, provider credentials |
| Room grant `controller` | Control playback, radio and queue ordering in one Room; not a global role |

## Planned Identity Follow-ups

Keep these separate from Integration credential lifecycle work:

1. **OIDC self-service IM binding (priority)** — authenticated OIDC users generate a short-lived, one-time code and redeem it from the target IM subject through a trusted Integration, proving ownership without an administrator copying Principal IDs.
2. **Principal lifecycle administration** — add first-party disable/enable, link/grant inspection, and bulk external-access revocation. The persisted `active` flag already enforces disabled Principals, but no complete admin workflow exists yet.
3. **OIDC role refresh/revocation strategy** — roles currently refresh when OIDC login updates the Principal; IdP-side removal is not pushed immediately. Choose webhook/SCIM/periodic refresh later rather than implying real-time revocation.

## Agent Caveats

- **Cover endpoint is unauthenticated** by design (`/api/v1/cover/{ref}`) — for `<img>` tags that can't send auth headers
- **Stream endpoint uses ticket auth** (`?ticket=...`) — for `<audio>` tags. Tickets are 5-minute TTL bound to ref + identity
- **CORS**: methods/headers/max-age are hardcoded in `httpapi.corsMiddleware`, only origins configurable
- **WS state machine**: playback position is computed from `position_ms + updated_at + playing` — never stored as a continuous counter
- **`position_ms` can be negative**: on track switch the room schedules position 0 at `updated_at + start_lead_ms` (room policy `start_lead_ms`, default 600ms) so clients can load and all start on time without losing the head of the track. A negative computed `should_be` means "starts in |x| ms" — clients load-and-pause, skip drift correction, and clamp to 0 when rendering. See `docs/spec-v1.md` §2.2
- **Room actor**: per-room goroutine. Queue entries persisted in SQLite. Radio mode state is runtime-only (lost on restart)
- **Current track stays in the queue**: `room_queue` holds the playing entry plus everything upcoming; `is_current` marks the cursor. The wire format is unchanged — `queue.snapshot`/`queue.patch` still carry upcoming entries only, and the playing entry is delivered via `playback.changed`. Persisting it means the playing track is queryable in SQL (acceleration pins the object being streamed) and a restart resumes that track instead of skipping to the next one. `position_ms` is still runtime-only, so resume starts from the head
- **TrackRef format**: `provider:id` — opaque string below the API layer (e.g. `ncm:347230`, `bili:BV1xx`, `local:<uuid>`)
- **All timestamps**: Unix milliseconds (UTC)

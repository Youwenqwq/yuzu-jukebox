# Yuzu Jukebox — Agent Guide

## What It Is

Music jukebox server: multi-room, multi-provider, queue-based playback with real-time WebSocket push. Written in Go, standard library `net/http`, SQLite backend.

Three binaries:
- `yuzu-server` — the HTTP+WS server
- `yuzu-cli` — CLI control client (read-only reference)
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
Web Client ←→ /ws/v1 (auth → room.join → playback/queue/listeners broadcast)
                ↓
Admin/CLI   ←→ /api/v1/* (REST: rooms, search, upload, players)
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
- All REST endpoints except `/api/v1/auth/guest`, `/api/v1/auth/oidc/config`, and `/api/v1/cover/{ref}` require roles via `requireRole(w, r, auth.RoleListener)` or higher.

### WebSocket Protocol
- JSON text frames with envelope: `{"type": "...", "ref": "...", "data": {...}}`
- ref is client-generated (loop counter or UUID), echoed back by server
- Broadcast messages (playback/queue/listeners.changed) have no ref
- See `docs/spec-v1.md` for full protocol spec

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
- **Key tables**: `sessions`, `rooms`, `room_queue`, `playlists`, `playlist_items`, `credentials`
- **Encryption**: `credentials` table AES-GCM encrypted with `secret_key`. Lost key = all provider cookies unreadable.

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
| `room_admin` | Manage room, control playback, manage players |
| `media_admin` | Manage media, upload, provider credentials |

## Agent Caveats

- **Cover endpoint is unauthenticated** by design (`/api/v1/cover/{ref}`) — for `<img>` tags that can't send auth headers
- **Stream endpoint uses ticket auth** (`?ticket=...`) — for `<audio>` tags. Tickets are 5-minute TTL bound to ref + identity
- **CORS**: methods/headers/max-age are hardcoded in `httpapi.corsMiddleware`, only origins configurable
- **WS state machine**: playback position is computed from `position_ms + updated_at + playing` — never stored as a continuous counter
- **Room actor**: per-room goroutine. Queue entries persisted in SQLite. Radio mode state is runtime-only (lost on restart)
- **TrackRef format**: `provider:id` — opaque string below the API layer (e.g. `ncm:347230`, `bili:BV1xx`, `local:<uuid>`)
- **All timestamps**: Unix milliseconds (UTC)

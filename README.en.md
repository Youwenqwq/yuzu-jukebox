# Yuzu Jukebox

A self-hosted multi-room music jukebox — queue up tracks from multiple sources,
play them in sync across rooms, and control everything from CLI, Web UI, or chat bots.

> **Development status**: this release is still under active development. The core features work and can be used with [yuzu-jukebox-webui](https://github.com/Youwenqwq/yuzu-jukebox-webui); the API contract may change at any time and is not recommended for production.

## What can it do?

- **Multi-room playback** — each room has its own queue, playback state, and listeners
- **Multi-provider** — local files and Netease Cloud Music; Bilibili (not available yet)
- **Real-time sync** — WebSocket-pushed playback state; all clients stay in sync within ~100ms
- **Headless playback** — `yuzu-agent` runs on any machine with MPV and acts as a room speaker
- **Chat bot integration** — connect Discord/Telegram/IM bots via the Integration API
- **Radio mode** — playlist shuffle, daily recommendations, personal FM, similar-song stations
- **OIDC login** — authenticate via Zitadel or any OIDC IdP; roles map to Yuzu permissions
- **Streaming cache** — first play caches to disk, replays hit local files; LRU eviction

## How it works

```
You (CLI / WebUI / Chat bot)
        │
        ▼
┌──────────────────────────────┐
│        yuzu-server           │
│  Room state machines         │
│  Queue + playback control    │
│  Provider layer (NCM/Bili)   │
│  Stream cache + ticket auth  │
└──────────┬───────────────────┘
           │
    ┌──────┴──────┐
    ▼             ▼
yuzu-agent     Web browser
(MPV speaker)  (HTML5 Audio)
```

- **Server** is the single source of truth — rooms, queues, playback state
- **Agents** are headless MPV renderers bound to rooms as persistent speakers
- **Clients** (CLI, WebUI, bots) control playback via REST + WebSocket

## Tech stack

| Layer | Tech |
|-------|------|
| Server | Go 1.26+, `net/http` (stdlib), WebSocket |
| Database | SQLite (WAL mode, goose migrations) |
| Playback agent | MPV (JSON IPC) |
| Auth | Guest, OIDC (Zitadel), session tokens, ticket-based stream auth |
| Providers | NeteaseCloudMusicApi (sidecar process); Bilibili not available yet |

## Quick start

**Prerequisites:** Go 1.26+. For the playback agent: [MPV](https://mpv.io/).

```bash
# Build
go build -o bin/yuzu-server ./cmd/yuzu-server
go build -o bin/yuzu-agent  ./cmd/yuzu-agent
go build -o bin/yuzu-cli    ./cmd/yuzu-cli

# Start (auto-generates config.json on first run)
./bin/yuzu-server -config config.json
```

Edit `config.json` to enable providers:

```json
{
  "addr": ":8080",
  "admin_password": "change-me",
  "ncm": { "enabled": true, "base_url": "http://127.0.0.1:3000", "level": "exhigh" }
}
```

NCM requires running its sidecar API — see [External Dependencies](#external-dependencies) below.

## Basic usage

Authenticate, create a room, and start playing:

```bash
export YUZU_SERVER=http://127.0.0.1:8080
export YUZU_PASSWORD=change-me

# Authenticate as admin
TOKEN=$(curl -s -X POST $YUZU_SERVER/api/v1/auth/guest \
  -H 'Content-Type: application/json' \
  -d '{"name":"admin","password":"change-me"}' | jq -r .session_token)

# Create a room
curl -X POST $YUZU_SERVER/api/v1/rooms -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"lobby","name":"Lobby"}'

# Search and queue a track
yuzu-cli search "海阔天空" -provider ncm
yuzu-cli add lobby ncm:347230

# Control playback
yuzu-cli pause lobby
yuzu-cli resume lobby
yuzu-cli skip lobby
yuzu-cli queue lobby
```

### Common CLI commands

| Command | What it does |
|---------|-------------|
| `yuzu-cli search <query> [-provider local\|ncm]` | Search tracks |
| `yuzu-cli add <room> <track_ref>...` | Add to queue |
| `yuzu-cli queue <room>` | Show current track + queue |
| `yuzu-cli skip <room>` | Skip to next track |
| `yuzu-cli pause\|resume\|seek <room> <seconds>` | Playback control |
| `yuzu-cli room list` | List rooms |
| `yuzu-cli room create <id> <name>` | Create a room |
| `yuzu-cli radio play <room> playlist:<id> -shuffle` | Start radio mode |
| `yuzu-cli radio stop <room>` | Stop radio |
| `yuzu-cli media upload <file>` | Upload local audio |
| `yuzu-cli provider qrlogin ncm` | Scan QR to login Netease |
| `yuzu-cli player create <id> <name>` | Create a speaker |
| `yuzu-cli player bind <id> <room>` | Assign speaker to room |
| `yuzu-cli login` | OIDC device login |

Every command has `--help`. Run `yuzu-cli <command> --help` for details.

## Setting up a speaker

Create a persistent Player, bind it to a room, then run the agent:

```bash
yuzu-cli player create living-room "Living Room Speaker"
# Save the one-time key from output!
yuzu-cli player bind living-room lobby

# On the speaker machine (defaults to http://127.0.0.1:8080; use YUZU_SERVER when the speaker is on another machine):
YUZU_SERVER=http://<server-address>:8080 YUZU_PLAYER_KEY=yzp_xxx ./bin/yuzu-agent
```

The agent auto-reconnects, syncs playback position, and stays bound to its assigned room.

## External dependencies

These run as separate sidecar processes:

| Provider | Sidecar | Default URL |
|----------|---------|-------------|
| Netease Cloud Music | [NeteaseCloudMusicApi](https://github.com/neteasecloudmusicapienhanced/api-enhanced) | `http://127.0.0.1:3000` |
| Bilibili | not available yet (sidecar not public) | — |

Disable any provider by setting `"enabled": false` in config.

## Deployment

See [docs/deploy.md](docs/deploy.md) for production setup:
- Reverse proxy (Caddy or nginx) with TLS termination
- systemd service unit
- Backup strategy
- Public vs. private deployment considerations

**Where it runs:** Linux server (x86/ARM). The agent runs on any Linux machine with MPV — Raspberry Pi, old laptop, etc.

For low-bandwidth public servers, an optional [EdgeOne CDN offload](docs/edgeone-distribution.md) can be evaluated — **experimental, still under development, not recommended for now**.

## Documentation

| Doc | Content |
|-----|---------|
| [docs/spec-v1.md](docs/spec-v1.md) | Wire protocol, room state machine, auth flow, integration contract |
| [docs/deploy.md](docs/deploy.md) | Production deployment guide |
| [docs/edgeone-distribution.md](docs/edgeone-distribution.md) | EdgeOne CDN offload design (experimental) |
| [AGENTS.md](AGENTS.md) | Codebase guide for contributors and coding agents |

## Permissions

| Role | Can do |
|------|--------|
| `listener` | Join rooms, see state, receive playback |
| `requester` | Search, add to queue, remove own entries |
| `room_admin` | Manage rooms, integrations, grants; controller in every room |
| `media_admin` | Upload media, manage provider credentials, playlists |
| Room `controller` grant | Control playback in a specific room |

## License

[AGPL-3.0](LICENSE)

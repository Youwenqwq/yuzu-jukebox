# Yuzu EdgeOne distribution module

This Makers project is an optional public deployment layer. LAN deployments do
not need it, and `yuzu-server` keeps serving the existing `/stream/v1` route.

The data plane is deliberately split:

```text
Edge Function
  -> same-project Cloud control bridge
       -> Yuzu Core ticket introspection
       -> short-lived Blob GET signing
  -> Blob Range proxy, or validated origin fallback
```

Custom secrets are kept in Cloud Functions. The Edge Function receives only
the signed URL and validated origin URL returned by the same-project bridge.

## Routes

- `GET /stream/v1/{ref}`: Edge Function Blob Range proxy and origin fallback.
- `POST /yuzu-edge/introspect`: public ticket-bearing Cloud control bridge; it
  introspects Core and returns a short-lived signed Blob URL or validated
  origin fallback URL. Core credentials never enter the Edge runtime.
- `POST /yuzu-edge/event`: ticket-bearing metrics bridge.
- `POST /yuzu-blob/put-urls`: protected Cloud Function batch PUT signer.
- `POST /yuzu-blob/get-urls`: protected experimental batch GET signer.
- `POST /yuzu-blob/metadata`: protected strong metadata verification.
- `POST /yuzu-blob/delete`: protected bounded cleanup operation.

## Configuration

The project requires four Cloud Function environment variables:

| Variable | Purpose |
|---|---|
| `EO_YUZU_CORE_ORIGIN` | Publicly reachable Yuzu origin, without the EdgeOne distribution hostname |
| `EO_YUZU_EDGE_TOKEN` | Matches `distribution.edge_token` in Yuzu Core |
| `EO_YUZU_SIGNER_TOKEN` | Independent credential used by `yuzu-edgeone` for PUT signing and metadata |
| `EO_YUZU_BLOB_STORE` | Blob namespace name, for example `yuzu-media` |

Use independent, high-entropy values for the Core publisher token, Core edge
token, and signer token. The Core and publisher side are configured separately:

```jsonc
// config.json (yuzu-server)
{
  "distribution": {
    "enabled": true,
    "backend": "edgeone",
    "publisher_token": "<publisher-token>",
    "edge_token": "<edge-token>",
    "lease_ttl_seconds": 600
  }
}
```

Copy `edgeone.example.json` to an ignored `edgeone.json`, set the same
publisher token, set `signer_base_url` to the Makers `/yuzu-blob` route, and
run the optional publisher:

```bash
go build -o bin/yuzu-edgeone ./cmd/yuzu-edgeone
./bin/yuzu-edgeone -config edgeone.json
```

For a co-located sidecar, keep `core_url` on loopback. The default upload rate
is 187500 bytes/s (1.5 Mbps), leaving roughly half of a 3 Mbps uplink available
for cold origin fallback. Files over the default 23 MiB object limit are
deferred for the P2 chunked layout instead of being uploaded incorrectly.

For local development, copy `.env.example` to `.env`. Production deployments
do **not** inherit that file: set the variables in the Makers console or with a
working CLI, then deploy:

```bash
edgeone makers env set EO_YUZU_CORE_ORIGIN https://origin.example.com
edgeone makers env set EO_YUZU_EDGE_TOKEN '<core-edge-token>'
edgeone makers env set EO_YUZU_SIGNER_TOKEN '<independent-signer-token>'
edgeone makers env set EO_YUZU_BLOB_STORE yuzu-media

pnpm install
pnpm run check
edgeone makers deploy --env production --area global
```

EdgeOne CLI 1.6.16 has a known `makers env set` bug: it may exit with status 0
without making the API request. A successful command must print an explicit
`Add environment ... succeed!` or `Update environment ... succeed!` message.
If it does not, use the Makers console or upgrade the CLI. During deployment,
also require `Pulling environment variables to .env succeed!`; `No environment
variables found` means the production functions are not configured.

Never point `EO_YUZU_CORE_ORIGIN` at the public EdgeOne distribution hostname;
it must be a distinct origin URL to prevent a fallback loop.

## Public path mounting

Core still emits the relative URL `/stream/v1/{ref}?ticket=...`. The public
EdgeOne configuration therefore needs to route that path to this Makers
project at the edge, while REST and WebSocket paths continue to use Yuzu Core.
Do not proxy Blob responses back through the 3 Mbps origin server, or the
bandwidth saving is lost.

## Health checks

- `GET /yuzu-edge/health` is public and reports only configuration booleans.
- `GET /yuzu-blob/health` requires an `Authorization: Bearer <token>` header
  using `EO_YUZU_SIGNER_TOKEN`, and verifies signer compatibility.

The GET signer intentionally isolates its dependency on Blob SDK internals. If
the installed SDK becomes incompatible, it returns `501` and the Edge Function
falls back to the original Yuzu stream path.

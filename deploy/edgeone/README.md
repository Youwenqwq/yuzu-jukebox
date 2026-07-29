# Yuzu EdgeOne acceleration

This directory deploys the Makers control plane for the optional EdgeOne
acceleration. The public media data plane is the site-level Edge Function in
[`../edgeone-site/stream.js`](../edgeone-site/stream.js); Makers no longer owns
the public `/stream/v1` route.

## Runtime topology

```text
Yuzu public hostname /stream/v1/*
  -> EdgeOne site Edge Function trigger
       -> Makers /yuzu-edge/introspect
            -> Yuzu Core ticket/candidate introspection
            -> short-lived Blob GET signing
       -> Blob Range proxy
       -> fetch(event.request) for normal site-origin fallback

yuzu-edgeone adapter
  -> Yuzu Core managed acceleration configuration, leases, reservations, and GC jobs
  -> Makers /yuzu-blob upload, inventory, metadata, and delete APIs
  -> EdgeOne Blob
```

REST, WebSocket, cover, and SPA paths continue through the normal EdgeOne site
origin to `yuzu-server`. LAN deployments do not need this module.

## Makers routes

- `POST /yuzu-edge/introspect`: ticket-bearing control bridge.
- `POST /yuzu-edge/event`: delivery metrics bridge.
- `GET /yuzu-edge/health`: public configuration and Blob GET-signing health.
- `POST /yuzu-blob/put-urls`: protected batch PUT URL creation.
- `POST /yuzu-blob/get-urls`: protected experimental batch GET URL creation.
- `POST /yuzu-blob/metadata`: protected strong metadata verification.
- `POST /yuzu-blob/inventory`: protected, strongly consistent paginated inventory.
- `POST /yuzu-blob/delete`: protected bounded deletion.
- `GET /yuzu-blob/health`: protected backend health.

## Create the managed acceleration

Acceleration configuration is persisted by Yuzu Core rather than read from
`config.json`. Create it with a `media_admin` session:

```http
POST /api/v1/accelerations
Authorization: Bearer <media-admin-session>
Content-Type: application/json

{
  "id": "edgeone-main",
  "name": "Main EdgeOne",
  "kind": "edgeone",
  "control_base_url": "https://<makers-host>/yuzu-edge",
  "backend_base_url": "https://<makers-host>/yuzu-blob",
  "publish_on_cache_ready": true,
  "lease_ttl_seconds": 600,
  "upload_rate_bytes_per_second": 187500,
  "max_object_bytes": 24117248,
  "storage_budget_bytes": 891289600,
  "storage_high_watermark_percent": 95,
  "storage_low_watermark_percent": 85
}
```

The resource is created disabled. The response returns `publisher_token`,
`delivery_token`, and `backend_token` exactly once. List/get responses expose
only credential-configured flags. The shown 850 MiB budget leaves headroom in
the current 1 GiB free Blob quota.

## Makers environment

Set the credentials returned by the create API:

| Variable | Purpose |
|---|---|
| `EO_YUZU_CORE_ORIGIN` | Public Yuzu hostname; `/internal/*` must reach Core |
| `EO_YUZU_DELIVERY_TOKEN` | Managed acceleration delivery token |
| `EO_YUZU_BACKEND_TOKEN` | Managed acceleration backend token |
| `EO_YUZU_BLOB_STORE` | Blob namespace, for example `yuzu-media` |

```bash
edgeone makers env set EO_YUZU_CORE_ORIGIN https://jukebox.example.com
edgeone makers env set EO_YUZU_DELIVERY_TOKEN '<delivery-token>'
edgeone makers env set EO_YUZU_BACKEND_TOKEN '<backend-token>'
edgeone makers env set EO_YUZU_BLOB_STORE yuzu-media
pnpm install
pnpm run check
edgeone makers deploy --env production --area global
```

EdgeOne CLI 1.6.16 may report status 0 without writing an environment variable.
Require an explicit `Add environment ... succeed!` or `Update environment ...
succeed!` message, and require `Pulling environment variables to .env
succeed!` in deployment output.

### Upgrade from the P1 deployment

Migration `0015_acceleration_storage.sql` preserves existing credential values
while renaming their database columns. Before deploying the new Makers
functions, set `EO_YUZU_DELIVERY_TOKEN` to the former edge-token value and
`EO_YUZU_BACKEND_TOKEN` to the former signer-token value. Deploy Core,
`yuzu-edgeone`, and Makers together: the old JSON field names, environment
variables, credential purposes, and `/internal/v1/distribution/*` routes are
not retained as aliases.


## Site-level Edge Function

Open the EdgeOne site Dashboard, create an Edge Function, and paste the complete
contents of [`../edgeone-site/stream.js`](../edgeone-site/stream.js).
Before pasting, change the single `CONTROL_ORIGIN` constant at the top to the
managed acceleration's `control_base_url`.

Create a trigger rule with:

- match type: `URL path`;
- operator: the Dashboard operator that matches a path prefix/wildcard;
- value: `/stream/v1/*`;
- optional request-method condition: `GET`.

Do not attach this function to `/api/*`, `/internal/*`, `/ws/v1`, or the SPA.
The function calls Makers using the absolute control URL. For not-ready,
disabled, backend-failure, Blob-failure, and control-outage paths it executes
`fetch(event.request)`, which uses the current EdgeOne site's normal cache and
origin configuration while preserving Range and ticket semantics.

## Publisher adapter

The adapter keeps only bootstrap-local settings. Backend URL/token, upload
policy, and storage budget are fetched from Core:

```json
{
  "core_url": "http://127.0.0.1:8080",
  "publisher_token": "<publisher-token>",
  "state_path": "data/yuzu-edgeone.db",
  "owner": "publisher-1",
  "poll_interval_seconds": 2,
  "http_timeout_seconds": 600
}
```

```bash
go build -o bin/yuzu-edgeone ./cmd/yuzu-edgeone
./bin/yuzu-edgeone -config edgeone.json
```

The adapter reports heartbeat, source/download bytes, Blob upload bytes,
current phase, and errors to Core. Progress updates renew the lease up to the
managed resource's `lease_ttl_seconds`. It also reports a strongly consistent
Blob inventory every six hours, executes Core-issued deletion jobs, and never
uploads without a Core storage reservation.

## Enable and observe

After the adapter heartbeat is visible and both Makers health endpoints pass:

```http
PATCH /api/v1/accelerations/edgeone-main
Authorization: Bearer <media-admin-session>
Content-Type: application/json

{"enabled": true}
```

Enablement fails with `409 acceleration_not_ready` if endpoints, credentials,
health, or a recent publisher heartbeat are missing.
Readiness also requires a positive storage budget and a recent adapter
heartbeat advertising `storage.inventory` and `object.delete`.

Operational APIs:

- `GET /api/v1/accelerations`
- `GET /api/v1/accelerations/{id}`
- `GET /api/v1/accelerations/{id}/status`
- `GET /api/v1/accelerations/{id}/requests?state=pending|uploading|failed|ready`

The status response includes `storage.accounted_bytes`, `reserved_bytes`,
`observed_bytes`, orphan/missing counts, pending deletions, watermarks, and
`pressure`. At the high watermark Core invalidates least-recently-used
candidates and queues provider-specific deletes until projected usage reaches
the low watermark. Unknown objects found by inventory are recorded as orphans
and become eligible for the same GC path.

All require `media_admin`. Machine endpoints use acceleration-scoped
credentials and never accept an acceleration ID supplied by the caller.

## Credential rotation

Rotation is staged to avoid breaking external components:

```text
POST /api/v1/accelerations/{id}/credentials/{publisher|delivery|backend}/prepare
configure the returned pending token in the external component
POST /api/v1/accelerations/{id}/credentials/{publisher|delivery|backend}/activate
```

Pending publisher and delivery tokens authenticate during rollout. Pending
backend activation first verifies the protected Makers health endpoint.

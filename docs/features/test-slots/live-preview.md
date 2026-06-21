# Live frontend preview

Live frontend preview lets a developer push their app's freshly-built frontend
to a real, co-watchable preview test-environment URL, served override-first over
a **stable** backend, so UI iterates in seconds instead of waiting for a full CI
image build+deploy.

It is a separate lane from validation:

- **image-deploy lane (faithful)** — a validation slot runs *exactly* the CI-built
  image (`deploy-image-to-slot`). Untouched by this feature.
- **live-preview lane (scratch)** — what this feature builds. **Never** a
  validation input; it exists for *seeing* UI iterate.

It shares no vocabulary with the retired hot-swap path; the guard
`scripts/check-deleted-test-slot-hot-swap.mjs` keeps that surface deleted.

## Architecture

A preview test env is a **single serving pod**:

```
            preview wildcard URL (HTTPRoute → Service)
                              │
                       ┌──────▼───────┐
                       │  live-preview │   served port
                       │     edge      │
                       └──────┬───────┘
            override-first ↑  │  ↓ proxy (backend prefixes, fresh passthrough)
                   emptyDir ──┘  └──► app backend container (listens internally)
```

The **edge** (`cmd/live-preview-edge`, core in `internal/livepreview`) is the
data plane. For each request, in order:

1. **Control routes** `/__live-preview/*` — handled locally, never proxied.
2. **Configured backend prefixes** (e.g. `/api`, `/healthz`) — reverse-proxied to
   the app backend, so the stable backend's API stays reachable through the edge.
3. **Otherwise (frontend/asset paths):**
   - override active + file exists → serve the file;
   - override active + file missing → SPA-fallback to the override's `index.html`;
   - no override active → reverse-proxy to the backend, so a fresh preview shows
     the stable app's own frontend until the first push.

The receiver core (extract guards, atomic `current` symlink flip, prune to ~3
releases) is ported from tank-operator's static-override receiver
(`backend-go/cmd/tank-operator/handlers_static_override.go`). tank-operator's
serving/toggle/SSE/daemon surface is **not** ported — that is the retired in-app
path the complete feature replaces.

### On-disk layout (the override emptyDir)

```
<root>/releases/rel-<ts>-<rand>/dist/      extracted bundle (served)
<root>/releases/rel-<ts>-<rand>/meta.json  build id + pushed_at (NOT served)
<root>/current                             symlink → releases/rel-...  (atomic flip)
```

`current` is flipped by writing a temp symlink and `rename(2)`-ing it over the
old one — an atomic, zero-window swap. `meta.json` is a sibling of `dist/`, so
the build marker travels with the flip yet is never web-served.

## Control endpoints (the contract Stage 2/4 depend on)

All under the reserved, never-proxied prefix `/__live-preview/`.

### `PUT /__live-preview/push`

Activate an override. Body: a **gzipped tar of the built frontend `dist/`**.
Headers:

- `Authorization: Bearer <auth.romaine.life service JWT>` (required).
- `X-Live-Preview-Build: <build id>` (**required**) — a content hash or git SHA
  of the built frontend. Persisted with the release and reported by `status`.

Guards (reject → the prior good bundle stays live):

- 64 MiB compressed cap (`http.MaxBytesReader`), 256 MiB uncompressed cap,
  20000-entry cap;
- reject absolute paths, `..` escapes, and symlink/hardlink/device members;
- require a root `index.html` (else reject, so the edge never flips to a dead
  frontend).

Response `200`:

```json
{ "status": "ok", "build": "<build id>", "release": "rel-...", "files": 12,
  "bytes": 345678, "pushed_at": "2026-06-21T...Z", "by": "<actor_email>" }
```

Error classes → HTTP status / push outcome: oversize body → `413` / `too_large`;
bad tar, missing `index.html`, or missing build header → `400` / `bad_archive`;
unauthorized → `401`/`403` / `unauthorized`; filesystem fault → `500` / `error`.

### `DELETE /__live-preview/push`

Remove the active override and drop the releases → revert to backend passthrough.
Same auth as push. Response `200`: `{ "status": "reverted", "was_active": <bool> }`.
Push outcome `reverted`.

### `GET /__live-preview/status` — served-build read-back (load-bearing)

Reports the **LIVE** bundle — the one `current` resolves to, not the last push
attempt — so a failed push (which never flips `current`) leaves status reporting
the prior good build. The next-stage observed-read-back verifier calls this to
confirm the edge serves exactly the build that was pushed.

```json
{ "override_active": true, "build": "<build id>", "release": "rel-...",
  "pushed_at": "2026-06-21T...Z" }
```

When no override is active: `{ "override_active": false, "build": "", "release": "", "pushed_at": "" }`.

Requires a valid auth.romaine.life token of any accepted role (admin/user/
service) but is **not** owner-scoped: the verifier (a service principal) and the
owning developer both read it, and it exposes only non-sensitive liveness
metadata, so it is token-gated (not open on the public preview URL) yet not
confined to the push subject.

### Ops routes

`GET /__live-preview/healthz` and `/readyz` (200, unauthenticated — k8s probes)
and `/metrics` (Prometheus, unauthenticated — in-cluster scrape).

## Auth model

- **Push / DELETE:** `role=service` + `actor_email` (enforced by the reused
  `internal/auth` verifier) **and** the verified JWT `sub` must equal the edge's
  configured `authorizedSubject`, matched exactly. That subject is the
  IdP-signed, unforgeable per-owner subject of the session/lease that owns this
  preview env — "a pod may only write its own preview". Any other subject is
  rejected with push outcome `unauthorized`.
- **Status:** any valid token; not owner-scoped (above).
- The edge validates against auth.romaine.life JWKS using glimmung's existing
  `internal/auth/romaine_jwt.go` verifier — no new auth code, no K8s-SA or
  cookie path (an edge data plane has only the bearer service-principal caller).

## Configuration (`cmd/live-preview-edge`)

| Env | Meaning |
| --- | --- |
| `LIVE_PREVIEW_EDGE_LISTEN` | listen address (default `:8080`) |
| `LIVE_PREVIEW_EDGE_UPSTREAM` | app backend base URL (required, absolute) |
| `LIVE_PREVIEW_EDGE_BACKEND_PREFIXES` | comma-separated prefixes proxied to the backend |
| `LIVE_PREVIEW_EDGE_OVERRIDE_ROOT` | override emptyDir mount (required) |
| `LIVE_PREVIEW_EDGE_AUTHORIZED_SUBJECT` | JWT `sub` permitted to push (required) |

## Helm partial

`k8s/live-preview-edge` is a reusable library chart (named templates) an app's
slot chart includes to run the edge in front of its backend, gated by the
consumer's `livePreview.enabled`. It renders the edge container (config from the
table above, override emptyDir mount), the override volume, the single-replica
guard, and the served-port helper (the edge becomes the served port / HTTPRoute
target; the backend listens internally). It is **off by default** and
**parameterized** for any app chart. `k8s/live-preview-edge-harness` proves it
renders. See `k8s/live-preview-edge/README.md`.

**Single serving pod** is a hard invariant: `live-preview-edge.replicas` returns
1 when enabled and fails the render on `replicas > 1` (v1 #1419 — a per-pod
emptyDir override behind multiple replicas load-balances reads 50/50 and flickers
the co-watched frontend).

## Observability

`internal/metrics`, namespace `glimmung_live_preview_edge_*`:

- `..._push_total{outcome}` — `ok | too_large | bad_archive | unauthorized |
  reverted | error`.
- `..._serve_total{disposition}` — `override_file | override_spa | backend_proxy
  | fresh_passthrough`.
- `..._proxy_errors_total{disposition}` — upstream proxy failures.
- `..._served_build_info{build}` — info gauge held at **one** active series
  (reset-then-set on each flip, cleared on revert) so the per-push build id never
  churns the label space; per-push history lives in `status` + structured logs.

## Image and CI

`live-preview-edge/Dockerfile` is a minimal single-binary image built from this
Go module. The image-build CI wiring (a `live-preview-edge` matrix entry in
`.github/workflows/docker-build-check.yaml`) and a CI guard job that runs
`scripts/check-deleted-test-slot-hot-swap.mjs` (`.github/workflows/tests.yaml`)
are staged in `live-preview-ci.patch` (`git apply` from the repo root) for the
integrator to land: a restricted-git session's GitHub App lacks the `workflows`
permission, so the spoke cannot push `.github/workflows/` edits directly.

## Staging to the complete feature

Stage 1 (this PR) builds the generic edge component only and is
inactive-until-wired. Remaining stages for the complete architecture:

1. **Edge component** — the standalone edge, image, reusable Helm partial, tests
   (this PR).
2. **Preview env provisioning + observed read-back** — Glimmung provisions a
   preview test env (stable main-image backend + edge on a wildcard URL) and a
   verifier that pushes a bundle and confirms `status` reports the pushed build.
3. **In-pod push path** — the session-side build+push of `dist/` to the edge.
4. **Cross-repo consumption + retire the in-app path** — app charts (ambience,
   kill-me, …) depend on/vendor the partial; tank-operator's in-app
   static-override path is deleted end-to-end.

Each stage leaves the system coherent; the edge does nothing to any app until a
chart wires it in.

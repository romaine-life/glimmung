# Live frontend preview: a generic Glimmung preview lane

**Status:** Accepted plan / build spec. Owner-approved 2026-06-21. Governed by
`docs/migration-policy.md`, `docs/quality-timeframes.md`,
`docs/product-inspirations.md`, and the test-slot deploy invariant in
`docs/test-slot-deploy-plan.md`. This is a long-horizon, heavy-default build —
durable state, observed outcomes, observability, and the migration deletion are
in-scope, not follow-ups.

This work is **hub-orchestrated**: a single tank session drives it, delegating
each stage to a fresh spoke session and integrating the result. It is **not**
routed through Glimmung's issue/run loop — the canonical spec is this document.

## Goal (north-star)

Let a developer push **any** onboarded romaine-life app's freshly-built
frontend to a real, co-watchable test-environment URL — served override-first
over a **stable** backend — so they iterate on UI in seconds instead of the
minutes a full CI image build + deploy costs. It must be **generic platform
infrastructure** owned by Glimmung (which owns ephemeral environments), usable
by ambience, glimmung, chess-tactics, kill-me, tank-operator, and any future UI
app — never baked into one app.

## Three lanes — two live, one dead

| Lane | Method | Purpose | This work |
| --- | --- | --- | --- |
| **image-deploy** | The slot runs **exactly** the CI-built, fingerprinted image (`docs/test-slot-deploy-plan.md`). | Validation / review / merge — faithful. | **Untouched.** |
| **live-preview** | Push your built frontend over a stable backend; serve it override-first. | Fast UI iteration — scratch. | **New — this doc.** Never a validation input. |
| **hot-swap** | Artifact streaming + a fail-open fidelity classifier. | (retired) | **Deleted, guarded, stays dead.** |

`live-preview` is a **separate mechanism** from the retired hot-swap and must
never revive its vocabulary. The guards
`glimmung/scripts/check-deleted-test-slot-hot-swap.mjs` and
`tank-operator/scripts/check-session-pod-hot-swap-migration.mjs` forbid
`apply_test_slot_hot_swap`, `artifact_kind`, `fidelity_classifier`,
`test_slot_hot_swap`, `GLIMMUNG_HOT_SWAP_*`, `resolveArtifact`, `DispatchHotSwap`.
Use **`live_preview` / `static_override`** vocabulary only.

## Locked architectural calls

1. **Edge home = glimmung.** A generic `live-preview-edge` (a standalone
   reverse-proxy + receiver + status read-back) lives in glimmung
   (`cmd/live-preview-edge`). Glimmung owns ephemeral environments; the edge is
   not baked into any app.
2. **Wiring = a shared Helm library partial, not Glimmung manifest-injection.**
   Each app's slot chart `include`s a Glimmung-published partial, activated by a
   value Glimmung sets on preview leases. This reuses each chart's knowledge of
   how to run its own backend (tank-operator is multi-container — injection
   would have to re-learn that). "Borrow primitives, not boundaries."
3. **Preview backend = main's stable CI image.** The preview lease runs a stable
   app backend (the fingerprinted CI image for `main`); only the frontend is
   scratch (the dev's uncommitted build). The frontend is **not CI-gated** —
   it is explicitly scratch and never merge evidence.
4. **Trigger = session-initiated.** The dev session starts the preview and
   pushes. Glimmung never builds — building would be hot-swap revival.
5. **Build = sender-side.** The dev session builds its own repo's frontend by
   convention and pushes the gzipped tar to the edge. (Glimmung runs
   Glimmung-managed Jobs only; a Glimmung-side build is out.)
6. **Two lease types; edge only on preview; preview is single-replica.** The
   validation slot is the CI image with no edge — pure (deploy-plan, untouched).
   The **preview lease** is a separate ephemeral env, durably typed `preview`,
   structurally **not** a validation target, served by a **single** pod. (v1
   lesson, tank-operator#1419: a per-pod emptyDir override with replicas > 1
   load-balances reads 50/50 and the co-watched frontend flickers.)

Per-app Glimmung config is a **new `live_preview` metadata key**
(`enabled`, `backend_prefixes`, `backend_port`) — never `test_slot_hot_swap.*`.

## The generic `live-preview-edge`

A standalone HTTP service that fronts the app backend in a preview env. Per
request, in order:

1. `/__live-preview/*` — the edge's own control surface, handled locally:
   - `PUT /__live-preview/push` — gzipped tar of a built `dist/`; extract with
     guards, atomically activate. Carries a build id (`X-Live-Preview-Build`)
     persisted with the release. Service-principal auth.
   - `DELETE /__live-preview/push` — drop the override, revert to passthrough.
     Service-principal auth.
   - `GET /__live-preview/status` — JSON: override active?, the **live build
     id** (the build `current` points at), release, pushed_at. **Load-bearing:**
     Glimmung's observed verifier reads this to confirm the edge is serving
     exactly the pushed build.
2. Configured **backend prefixes** (`/api`, `/healthz`, …) — reverse-proxy to
   the app backend upstream.
3. Otherwise (frontend/asset paths): serve the override file if present;
   SPA-fallback to the override `index.html`; **if no override is active
   (fresh preview), proxy to the backend** so a fresh preview shows the stable
   app until the first push.

**Receiver core is ported from the v1 prototype** (tank-operator
`backend-go/cmd/tank-operator/handlers_static_override.go`): streamed
gzip-tar extraction with containment + entry-count + uncompressed-size guards,
link rejection, a required root `index.html`, extraction into
`releases/rel-…` and an atomic `current` symlink flip (`rename(2)`), prune to a
few releases, DELETE reverts. **Added:** the pushed build id persists with the
release so `/__live-preview/status` can report it.

**Auth** reuses glimmung's `internal/auth` (`romaine_jwt.go`): push/DELETE
require an `auth.romaine.life` service-principal JWT (`role=service`,
`actor_email`), and the edge only accepts its **owner's** pushes (an expected
subject is configured at provision time — "a pod may only write its own
preview").

## Durable model & observed verification

- A durable, Glimmung-owned **`preview_environment`** record: project, lease,
  enabled, `live_build_id`, `pushed_at`, `observed_build_id`, `observed_at`.
- **Observed, not claimed.** A push is "live" only when the edge is read back
  (via `/__live-preview/status`) serving that exact build id. The dashboard and
  state reflect the observed build, never local optimism
  (`product-inspirations.md`: observed outcomes beat claimed intent).
- **Live transport wakes; it does not own state.** Control + status flow over a
  wake/SSE channel; the durable record is the truth, resync on reconnect.
- **Metrics:** preview provisioned, push received, observed-confirm,
  stale-detected (pushed but the edge is not serving it).

## Per-app readiness (Stage 0 audit, 2026-06-21)

| App | CI stable image (P1) | Slot chart can host edge (P2) | Auth token on slot pod (P3) |
| --- | --- | --- | --- |
| tank-operator | Y (`docker-build-check` → `app-<fp>`+`sha-<commit>`) | Y — multi-container; **v1 source** | **Y** (projected unconditionally) |
| kill-me | Y | Y — single container `http:3000`, cleanest | **N — add it** |
| chess-tactics | Y | Y — single container `http:3000` (+ ephemeral PG object) | **N — add it** |
| glimmung | Y | Y — slot chart `k8s/issue` (single container); see note | **N on the slot chart** — prod projects it, `k8s/issue` does not |
| ambience | Y | Y *with caveat* — native `edge`/`authority` two-tier split | **N — add it** |
| spirelens | — | — | **Out of scope** (game mod, no webapp/chart/ACR image) |

**P1 is satisfied uniformly** — every webapp's `docker-build-check.yaml` pushes
a runnable `app-<fp>` image + `sha-<commit>` alias to ACR on trusted PRs. **The
consistent onboarding task is P3**: add a projected
`serviceAccountToken{audience: https://auth.romaine.life, path: token}` to the
slot pod (paths in the Stage 4 task). **Onboarding order:** kill-me →
chess-tactics → glimmung → ambience.

**Design notes from the audit:**
- **ambience** runs its own `edge`/`authority` split (its slot Service targets
  the native edge pod, not the backend). The `edge.webOverride.enabled:"true"`
  in its Glimmung metadata is a **dormant stub that lands on nothing** (zero
  code references). Onboarding ambience requires deciding how the generic edge
  coexists with (or replaces) ambience's native edge tier — the hardest case.
- **glimmung** already has **dormant override scaffolding** in its slot chart:
  override-first serving (`server.go` `roots{override, base}`), an emptyDir, and
  a trivial `static-writer` container — but **no push receiver and no daemon**.
  It is not a second v1 instance. When the generic edge fronts glimmung's
  backend, this in-backend override serving + writer-container become redundant
  and are cleaned up during glimmung's onboarding. The adjacent
  `hotSwapBackend` value belongs to the **image-deploy** lane and is left alone.

## Stages (sequential; each leaves the system coherent)

0. **Precondition audit** — *done* (table above).
1. **`live-preview-edge` component** — the standalone reverse-proxy + receiver +
   status read-back; image; the shared Helm partial. Inactive until wired.
   Unit + integration tests.
2. **Glimmung preview-lane** — the durable `preview_environment` model + a
   provision op (deploy the stable image + the edge partial, route URL → edge →
   app; not CI-gated) + wake/SSE control + the observed read-back verifier +
   dashboard control/status (design-system) + metrics.
3. **Generalize the sender** — lift `push-frontend.sh` + the in-pod daemon out
   of tank-operator specifics into a repo-agnostic builder (per-repo build
   command by convention) that pushes to a preview edge.
4. **Per-app onboarding + cross-app smoke (cutover gate)** — opt every app in
   via the `live_preview` key + the edge partial (adding the P3 token where
   missing); an automated + retained slot-smoke green for **every** app:
   observed-serve, replace-not-install, fresh-preview passthrough, clear-revert,
   negative path. Modeled on the deploy-plan's Stage 6, for the preview lane.
5. **Cutover (required)** — delete the tank-operator-specific
   receiver/serving/chart-wiring/daemon/toggle/state end to end (no parallel
   path); tank-operator becomes a normal preview consumer; remove the vestigial
   `test_slot_hot_swap` project metadata; add migration guards; sweep doc-rot
   (the dangling `docs/test-slot-hot-swap.md` reference in
   `docs/features/test-slots/contract.md`). Gated on Stage 4 green per app.

## Stage 2a landed contracts (preview-lane backend)

The Stage 2 backend (durable model, provision, verifier, control API, metadata,
metrics) is built. The dashboard UI (Stage 2b) and the sender (Stage 3) consume
these contracts:

- **Durable model — `preview_environment`** (Postgres `preview_environments`,
  PK `(project, name)`). It is the SOURCE OF TRUTH; state is never inferred from
  process memory or a session's claim. Fields: `project`, `name` (the preview env
  / helm release / DNS label), `lease_ref`, `session_id`, `authorized_subject`,
  `enabled`, `state`, `url`, `upstream_url`, `backend_prefixes`, `image_tag`,
  `edge_image`, the CLAIMED `live_build_id` / `pushed_at`, the OBSERVED
  `observed_build_id` / `observed_at`, `detail`, timestamps. States:
  `provisioning → ready → pushed → live`, with `stale` (pushed ≠ observed),
  `disabled`, `error`. Store CRUD is CAS-guarded on `updated_at`.
- **Observed-not-claimed verifier.** A control-plane reconciler reads each
  pending env's edge `GET /__live-preview/status` as a service principal
  (projected SA token → `POST {auth}/api/auth/exchange/k8s`) and folds it in: a
  push is marked `live` ONLY when `override_active && served build == pushed
  build`; a pushed build the edge is not serving is the durable, counted `stale`
  state. Metrics on transition (no per-poll double-count). Cost: only envs with
  an unconfirmed push are read back; a push receipt wakes the loop, so steady
  state is ~one read-back per push. Self-gates on `ControlPlaneLoopsEnabled`
  (slot processes never run it).
- **Control/status API** (admin-or-service auth): `POST /v1/previews` (provision
  → 202 + durable `provisioning` row, control-plane only), `GET /v1/previews`,
  `GET|DELETE /v1/previews/{project}/{name}`,
  `POST /v1/previews/{project}/{name}/push-receipt` (records the CLAIM + wakes
  the verifier), `.../enable`, `.../disable`. Preview state is also carried in
  the `GET /v1/events` SSE snapshot (`preview_environments`), so a watcher
  resyncs from the durable cursor on reconnect.
- **Provision / typing.** A preview lease is durably typed `preview`
  (`LeaseKindPreview`) and structurally NOT a validation target: it never carries
  `runner_k8s` / `test_slot_checkout`, is never acquired through the runner-slot
  checkout path, and is excluded from `testEnvironmentsFromSnapshot`. The
  provision reuses the slot Helm install machinery with a PREVIEW config — the
  app's own `test_slot_helm` install settings (chart path, installer image) plus
  layered `livePreview.*` `--set` values — reconciled in the SAME warm→hot
  sequence the faithful validation activation uses: WARM renders the
  renderWarm-gated route (HTTPRoute + testenv prereqs) for the preview's wildcard
  host, then HOT renders the workload (Deployment + Service) with the vendored
  edge in front of the backend. A hot-only render materializes the workload but
  no HTTPRoute, so the preview URL is unreachable — the warm pass is what makes it
  routable. The backend image is the chart's pinned main-lockstep default
  (stable); only the pushed frontend is scratch (NOT CI-gated). `k8s/issue` is the first/dogfood consumer of the
  `live-preview-edge` partial; its dormant in-backend static-override serving was
  removed (the edge owns serving).
- **Per-app `live_preview` metadata** `{enabled: bool, backend_prefixes:
  []string, backend_port: int}`, validated at `register_project`. `backend_port`
  is the in-pod port the app's OWN backend listens on (glimmung :8000, kill-me /
  chess-tactics :3000, ambience :8080); the provision sets the edge `UPSTREAM` /
  `livePreview.upstream.url` from it (loopback `http://127.0.0.1:<backend_port>`)
  so the edge fronts THIS app's backend rather than a hardcoded :8000. Unset
  falls back to `defaultPreviewBackendPort` (8000). Glimmung dogfoods it (register
  payload below).
- **Metrics** (`internal/metrics`): `glimmung_live_preview_provisioned_total`,
  `..._push_received_total`, `..._observed_confirmed_total`,
  `..._stale_detected_total`.

**Glimmung dogfood registration** (apply at rollout, after the chart with the
edge partial deploys):

```json
{ "name": "glimmung", "github_repo": "romaine-life/glimmung",
  "metadata": { "live_preview": { "enabled": true,
    "backend_prefixes": ["/v1", "/healthz", "/readyz", "/metrics", "/og"],
    "backend_port": 8000 } } }
```

## Stage 4a landed contracts (per-app provisionable foundation)

Stage 4a makes the live-preview lane provisionable for ANY onboarded app (Stage
2a only worked for glimmung's own dogfood). Stage 4b (per-app onboarding) and 4c
(cross-app smoke) build on these.

- **Edge image lockstep.** The generic `glimmung-live-preview-edge` image now has
  a stable, fingerprint-pinned tag in BOTH chart values files
  (`k8s/values.yaml` and `k8s/issue/values.yaml`, key `livePreview.image.tag`),
  bumped on every push to main by `.github/workflows/live-preview-edge-build.yaml`
  (which rebuilds + pushes the image to ACR, reusing the same fingerprint
  `docker-build-check.yaml` proves on PRs) and held in lockstep by
  `scripts/check-chart-image-tag-sync.mjs`. The prod deployment passes the prod
  value to the control plane as `LIVE_PREVIEW_EDGE_IMAGE_{REPOSITORY,TAG}` →
  `Settings.LivePreviewEdgeImage{Repository,Tag}`, and the provision `--set`s it +
  records it in the durable `edge_image`. There is **no floating `:edge`
  default**: an empty tag is a hard provision error (`previewHelmSettings`), so a
  missing edge image surfaces as an explicit provision failure, never a silent
  ImagePull.

- **Per-app backend port.** `live_preview.backend_port` (see the 2a metadata
  contract, now `{enabled, backend_prefixes, backend_port}`) drives the edge
  upstream so the edge fronts THIS app's backend port, not a hardcoded :8000.

- **Cross-repo partial consumption — Glimmung vendors via a ConfigMap.** An app
  chart in another repo cannot `file://` glimmung's `k8s/live-preview-edge`
  library partial, and an `oci://` Helm dependency from ACR is **not viable**: the
  registry (`romainecr`) is ACR **Basic SKU** — no anonymous pull, no scoped
  tokens — and the slot-install Job pod carries no Azure workload identity, so it
  cannot authenticate a Helm registry pull. Instead **Glimmung publishes the
  partial as a cluster ConfigMap** (`glimmung-live-preview-edge-chart` in the
  runner namespace, rendered from the sibling chart dir via `.Files` in
  `k8s/templates/live-preview-edge-chart-configmap.yaml`, ArgoCD-maintained in
  lockstep with glimmung main). A **preview** install Job mounts it and vendors
  the partial into the app chart's `charts/live-preview-edge/` **only if the chart
  did not already supply it** (`internal/server` `livePreviewEdgeVendorScript`).
  Glimmung's own `k8s/issue` keeps its in-repo `file://` dependency, so the vendor
  step no-ops for it; the faithful validation path is byte-for-byte unchanged
  (the vendor step is emitted only for preview installs).

  **App-chart onboarding snippet (4b copies this into each app's slot chart).**
  The app chart `include`s the partial but **declares NO Helm dependency** for it;
  every reference is guarded on `livePreview.enabled` so the chart renders
  standalone (app CI / prod / validation, where livePreview is off) WITHOUT the
  library, and only needs Glimmung's vendored partial on a preview lease:

  ```yaml
  # values.yaml — off by default; Glimmung sets these on a preview lease.
  livePreview:
    enabled: false

  # Deployment
  spec:
    # single serving pod when enabled (helper fails the render on replicas>1);
    # the app's own replica count otherwise.
    replicas: {{ if .Values.livePreview.enabled }}{{ include "live-preview-edge.replicas" (dict "spec" .Values.livePreview "requested" .Values.replicas) }}{{ else }}{{ .Values.replicas }}{{ end }}
    template:
      spec:
        containers:
          - name: app
            # the app backend listens INTERNALLY (its own port); the edge fronts it
            ports:
              - name: app-internal
                containerPort: 3000   # this app's backend port == live_preview.backend_port
          {{- if .Values.livePreview.enabled }}
          {{- include "live-preview-edge.container" .Values.livePreview | nindent 8 }}
          {{- end }}
        {{- if .Values.livePreview.enabled }}
        volumes:
          {{- include "live-preview-edge.volume" .Values.livePreview | nindent 8 }}
        {{- end }}
  ---
  # Service — the edge becomes the served port when enabled, the app port otherwise
  spec:
    ports:
      - name: http
        port: 80
        targetPort: {{ if .Values.livePreview.enabled }}{{ include "live-preview-edge.servedPortName" (dict "spec" .Values.livePreview "appPortName" "app-internal") }}{{ else }}app-internal{{ end }}
  ```

  Glimmung sets `livePreview.enabled/image.tag/authorizedSubject/upstream.url/
  backendPrefixes` per preview lease; the app sets its `live_preview` project
  metadata (`enabled`, `backend_prefixes`, `backend_port`) and adds the P3
  projected `auth.romaine.life` SA token to its slot pod (Stage 0 audit). No
  cross-repo Helm registry, no committed copy of the partial.

## Definition of done

All five stages. **Stage 5 deletion is required** — no parallel path survives.
Observability, durable state, observed-not-claimed, and a cost story are
in-scope, not follow-ups.

## What this is NOT

- Not hot-swap, and never uses its vocabulary (guards stay green).
- Not a validation input — a preview lease is structurally not a validation
  target; the faithful image-deploy lane is untouched.
- Not resurrectable — a preview env is ephemeral; pod death is a real boundary
  (`product-inspirations.md`, Coder/Gitpod).

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
(`enabled`, `backend_prefixes`) — never `test_slot_hot_swap.*`.

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

# Live-preview cutover smoke (Stage 4c gate)

**Status:** Retained gate. Owner of the live-preview lane cutover (Stage 5) must
keep this green. Governed by `docs/live-preview-plan.md` (Stage 4c) and modeled
on `docs/test-slot-deploy-plan.md` Stage 6 ("per-app deploy smoke"), per
`docs/quality-timeframes.md` ("observed outcomes beat claimed intent").

This is the **cutover gate** for Glimmung's live frontend preview lane: an
automated, retained, re-runnable smoke that provisions a REAL preview for every
onboarded app and proves the lane works end-to-end **observed from outside**
(observed, never mocked). The gate is GREEN for an app only when all five
properties below are observed green. It is the live-preview lane (scratch, for
seeing) — it never touches the faithful image-deploy validation lane and shares
no vocabulary with the retired hot-swap path.

## The five properties (per app)

A unique per-run sentinel/build-id makes a stale image or stale serve unable to
false-pass.

1. **fresh-preview passthrough** — before any push, the preview URL serves the
   STABLE app (the edge fresh-passthroughs to the backend): `override_active`
   is false and the edge reaches the backend (a 2xx/3xx response, not the edge's
   own `502 upstream backend unavailable`).
2. **observed-serve** (load-bearing) — push a `dist/` carrying a unique build id;
   then, read back FROM OUTSIDE: the edge `GET /__live-preview/status` reports
   that exact build AND the served page contains the sentinel AND Glimmung's
   durable `preview_environment.state` becomes `live` with `observed_build_id ==
   pushed build` (the observed-not-claimed verifier confirmed it).
3. **replace-not-install** — push build A, then build B; the preview moves A→B
   (the realistic re-push, not just a clean first install).
4. **clear-revert** — DELETE the override; `override_active` returns to false and
   the URL reverts to the stable backend passthrough.
5. **negative path** — an UNAUTHORIZED push (no / wrong auth.romaine.life subject)
   is REJECTED by the edge (401/403) and Glimmung is not falsely marked live.

Plus two invariants checked alongside: **backend prefixes stay backend-proxied**
through the edge regardless of override (the app's own `live_preview.backend_prefixes`),
and **clean terminal** (deprovision removes the durable row and the runtime; no
leak).

## The harness — `scripts/live-preview-smoke.sh`

The retained, re-runnable smoke. It speaks the same contracts as the Stage 3
sender (`tank-operator` `k8s/session-config/live-preview-push.sh`):

- exchanges the projected `auth.romaine.life` token for a service-principal JWT
  (`POST {auth}/api/auth/exchange/k8s`),
- provisions via Glimmung `POST /v1/previews` and polls the durable row to
  `ready`,
- waits for the preview host to be *stably* externally reachable (ExternalDNS +
  TLS propagation can flap right after `ready`),
- PUTs `gzip(tar(dist/))` to the edge `/__live-preview/push` with
  `X-Live-Preview-Build`, POSTs the Glimmung push receipt, reads back the edge
  status + served page + durable row,
- exercises all five properties, then deprovisions via `DELETE /v1/previews/...`.

```
# one app:
scripts/live-preview-smoke.sh --project chess-tactics --name smoke-chess
# every in-scope app (kill-me, chess-tactics, ambience, glimmung):
scripts/live-preview-smoke.sh --all
# reuse an already-ready preview / leave it up for inspection:
scripts/live-preview-smoke.sh --project glimmung --name smoke-glimmung --no-provision --keep
```

Evidence (status JSON, served pages, durable rows, push responses) is written
under `--evidence-dir` (default `/tmp/live-preview-smoke/<project>-<name>/`), and
a per-property PASS/FAIL table is printed and written to `results.tsv`. Exit 0
iff every observed property is green.

## How it is RETAINED / re-run

- **Version-controlled** in this repo (`scripts/live-preview-smoke.sh`), so every
  future change to the preview lane re-proves every app by re-running it.
- **Runs from any token-bearing context**: a Tank session pod (which projects
  `/var/run/secrets/auth.romaine.life/token`) — exactly how a developer uses the
  lane — or a **Glimmung-managed k8s Job**.
- **Glimmung-managed Job wrapper (Stage 4c follow-up):** to run as a first-class
  Glimmung slot-smoke under the verify loop + evidence infra, register a workflow
  whose verify-phase Job checks out this repo and runs `--all`, writing the
  per-property table into `artifacts/verification.json`. One prerequisite the
  control plane must add for that Job: project the `auth.romaine.life` SA token
  into the smoke Job pod (runner Jobs do NOT get it today — `run_launcher.go` only
  uses the control plane's own token). Until that projection lands, the retained
  smoke runs from a session pod; the logic is identical.

## Observed gate results — 2026-06-21 (first real end-to-end exercise)

This was the FIRST real end-to-end exercise of the whole lane (Stages 1–4b were
unit/render-tested). It surfaced two integration blockers, exactly as the gate
exists to do. **Gate status: RED** (chess-tactics green; two blockers below).

| Property | kill-me | chess-tactics | ambience | glimmung |
| --- | --- | --- | --- | --- |
| provision → ready | ❌ error (Bug 1) | ✅ ready | ❌ 422 (Bug 2) | ❌ error (Bug 1) |
| 1 fresh-passthrough | ❌ 502 backend down (Bug 1) | ✅ | — (Bug 2) | ⚠️ edge reaches backend (301) but store unready (Bug 1) |
| 2 observed-serve | ✅ status+sentinel+durable `live` | ✅ | — (Bug 2) | ✅ status+sentinel+durable `live` |
| 3 replace A→B | ✅ | ✅ | — (Bug 2) | ✅ |
| 4 clear-revert | ⚠️ edge reverts (override off); backend down (Bug 1) | ✅ | — (Bug 2) | ⚠️ edge reverts (override off); store unready (Bug 1) |
| 5 negative path | ✅ 401 + not-live | ✅ | — (Bug 2) | ✅ 401 + not-live |
| backend-proxy / deprovision | proxy blocked (backend down) / ✅ | ✅ / ✅ | — / — | proxy reachable / ✅ |

**Key finding: the live-preview lane's own machinery is sound.** Edge image pull,
edge container wiring, the warm→hot route fix (preview URL routable: DNS+TLS+
gateway), fresh-passthrough proxying, override push + atomic activate, override
serving read back from outside, A→B replace, the observed-not-claimed verifier
(durable `live` + `observed_build_id`), owner-only push auth, and override revert
are ALL observed-working. chess-tactics (no Azure workload identity) passes all
five end-to-end. The two RED apps are blocked by environment/config, not lane
logic.

### Blocker 1 — preview namespaces have no Azure workload-identity federation (kill-me, glimmung)

Apps whose stable backend authenticates to Azure at boot via workload identity
crash/again-unready in a preview because no Azure **federated identity credential**
exists for the preview namespace's service-account subject.

- **kill-me**: backend `CrashLoopBackOff`, exit 1 —
  `AADSTS700213: No matching federated identity record found for presented
  assertion subject 'system:serviceaccount:smoke-killme:infra-shared'` (it reads
  Azure App Config at boot).
- **glimmung**: backend Running but `Ready=false`, `/readyz` 503
  `read_store_not_ready` — same root cause (Postgres Azure-AD auth via workload
  identity for `system:serviceaccount:smoke-glimmung:infra-shared`).
- **Effect**: the HOT installer Job waits for the workload to become healthy and
  times out → `preview_environment.state=error`
  (`...installer job ... did not complete before timeout`). Properties 1 & 4
  ("serves the STABLE app") cannot be green.
- **Root cause**: `internal/server/runner_workload_identities.go`
  `desiredWorkloadIdentityCredentials` mints federated credentials ONLY for the
  fixed standby slot pool (`<slot_prefix>-<1..count>`), never for ad-hoc preview
  namespaces. The preview provisioner (`KubernetesRunLauncher`,
  `preview_provision.go`) holds no `FederatedIdentityCredentialClient` and does
  no per-preview federation. chess-tactics and ambience have no
  `runner_standby_workload_identity` and are unaffected.
- **Fix (landed on this branch; pending control-plane deploy to observe-verify)**:
  per-preview Azure workload-identity federation, built to the lifecycle bar (a
  federated credential is a per-environment preliminary resource,
  `docs/test-slot-lifecycle.md`). `ProvisionPreview` upserts a federated credential
  for the preview namespace subject (reusing the existing
  `FederatedIdentityCredentialClient` + the project's credential templates with
  `{namespace}`/`{slot_name}` = preview name) before the HOT render;
  `DeprovisionPreview` and every provision error path delete it (best-effort,
  detached context) so a failed/torn-down preview never leaks. The Azure
  per-identity federated-credential cap is bounded with a **surfaced** error
  (durable `state=error` naming the identity + in-use count, never a silent
  failure); metrics `glimmung_live_preview_workload_identity_total{operation,
  outcome}` + `..._orphans_reclaimed_total`; and a one-shot startup sweep
  (`ReclaimOrphanedPreviewWorkloadIdentities`, mirroring `RecoverInFlightTestSlots`)
  self-heals a missed teardown. Unit-tested (templating, upsert/teardown lifecycle,
  cap-exceeded, orphan reclaim, standby-credential preservation). It **cannot** be
  observed-verified from a session pod or a Glimmung test slot (the provision is
  control-plane-only; slots run `ControlPlaneLoopsEnabled=false`) — it must deploy
  to the control plane and be re-proven by re-running this smoke for kill-me +
  glimmung. See `docs/live-preview-plan.md` "Stage 4c landed contracts".

### Blocker 2 — ambience cannot provision a preview (no wildcard base; native edge tier)

- `POST /v1/previews` for ambience returns **422**: `project has no preview
  wildcard base (metadata.runner_standby_dns.record_base)`. `testSlotURL`
  (`test_slot_api.go`) requires `runner_standby_dns.record_base`/`recordBase`;
  ambience's metadata has neither.
- This is the "hardest case" flagged in `docs/live-preview-plan.md` (ambience
  runs its own `edge`/`authority` two-tier split; the generic edge must coexist
  with or replace it). Needs an owner/hub decision: add a `record_base` (+ DNS/
  TLS wildcard + how the generic edge fronts ambience's native edge) or define
  ambience's preview topology. Project-metadata + cross-repo design — routed to
  the hub.

## What this means for Stage 5 (cutover)

Stage 5 deletes the tank-operator-specific receiver path and makes tank-operator
a normal preview consumer. It is gated on this Stage 4c gate being green for
every in-scope app. Today the gate is RED on kill-me, glimmung (Blocker 1) and
ambience (Blocker 2). The lane machinery is proven (chess-tactics green); the
remaining work is the two blockers above plus, if the Glimmung-managed-Job
wrapper is desired, projecting the `auth.romaine.life` token into the smoke Job.

# Test Slots Capabilities

This ledger names user-facing behavior under the test-slots contract. It is
not a backlog. Entries land here when the behavior needs a stable handle for
planning, review, tests, incident follow-up, or retirement.

## slot-control-plane-isolation

Status: shipped

Intent:
A slot process (the binary running inside any `k8s/issue/` release, hot or
warm) serves the HTTP handler surface against the shared Postgres database
and the shared Kubernetes apiserver, and nothing else. It must not start any
background reconciler or recovery sweep that mutates run state, lease state,
signal state, or `glimmung-runs` Kubernetes Jobs. Those belong to the prod
glimmung Deployment, which is the single writer for the control plane.

This is the boundary that lets a hot-swapped binary exercise new code paths
against the real database and the real apiserver without racing the prod
control plane on the same rows and Jobs.

Affected contracts:
- Test Slots (primary — the slot is the isolation boundary)
- Workflow Execution (run-queue, dispatch-timeout, and any future workflow
  reconciler must honor the same gate)

Contract impact:
- `Settings.ControlPlaneLoopsEnabled` (env `CONTROL_PLANE_LOOPS_ENABLED`,
  default `true`) is the canonical gate. The prod Deployment leaves it at
  the default; `k8s/issue/templates/deployment.yaml` sets it to `false` on
  every per-issue release.
- `cmd/glimmung-go/main.go` is the single enforcement point. The
  `switch` that starts `StartSignalDrainReconciler`,
  `StartRunQueueReconciler`, `StartRunDispatchTimeoutReconciler`, and
  `RecoverInFlightTestSlots` is gated on `settings.ControlPlaneLoopsEnabled`
  and emits a startup log line when the gate is closed. Any new reconciler
  or recovery sweep that touches shared runtime state must be added inside
  the same `switch`.
- The slot Deployment in `k8s/issue/templates/deployment.yaml` keeps an
  inline comment naming the gate so a future reader does not strip the
  env var without understanding what it now controls.

Evidence:
- `internal/server/settings_test.go` — `TestSettingsFromEnv_ControlPlaneLoopsEnabled`
  pins default-true, accepted truthy/falsy values, and garbage-falls-back-to-default.
- `cmd/glimmung-go/main.go` — the gated `switch` that wraps every
  background reconciler and the test-slot recovery sweep.
- `internal/server/server.go` — `Settings.ControlPlaneLoopsEnabled` field
  doc explaining the prod-vs-slot invariant.
- `k8s/issue/templates/deployment.yaml` — env-var stanza with an inline
  comment pointing at `Settings.ControlPlaneLoopsEnabled`.

History:
- Before this capability was named, `CONTROL_PLANE_LOOPS_ENABLED` was set on
  the per-issue chart but unread by the Go binary. Slot binaries ran every
  control-plane reconciler against shared Postgres; the omission became visible
  when a slot-local reconciler called the apiserver for Jobs in
  `glimmung-runs` and hit 403 against the slot's narrowly-scoped
  ServiceAccount. The fix made the env var real rather than expanding slot
  RBAC.

## live-preview-edge

Status: in progress (Stage 1 of the live frontend preview feature)

Intent:
The live-preview edge is the generic data plane of the **live frontend preview**
feature: a standalone reverse-proxy + override receiver
(`cmd/live-preview-edge`) that an app's slot chart runs in front of its stable
backend inside a preview test environment. A developer pushes their freshly-built
frontend `dist/` to the edge; the edge serves it override-first over the stable
backend on the preview env's URL, so UI iterates in seconds instead of waiting
for a full CI image build+deploy. Fresh previews (before the first push) and all
configured backend prefixes proxy straight through to the stable backend.

This is the **live-preview lane** — scratch, for *seeing* — and is never a
validation input. It is a separate lane from the faithful image-deploy lane
(`deploy-image-to-slot`, which runs the exact CI image) and shares no vocabulary
with the retired hot-swap path. Building it is inactive-until-wired: no app chart
in this stage activates it, so no current slot/deploy behavior changes.

Affected contracts:
- Test Slots (primary — the preview env is a leased slot shape)
- Observability And Evidence (push outcomes, served-build, serve/proxy counters)
- Auth And API Surface (service-principal push auth + owner-subject scoping)

Contract impact:
- The edge owns a reserved control surface, never proxied: `PUT/DELETE
  /__live-preview/push` and `GET /__live-preview/status`, plus ops routes
  `/__live-preview/{healthz,readyz,metrics}`. App backend prefixes and frontend
  paths can never collide with it.
- Served-build read-back contract: `GET /__live-preview/status` reports
  `override_active`, the LIVE `build` id (the build of the bundle `current`
  resolves to, not the last push attempt), `release`, and `pushed_at`. The
  next-stage observed-read-back verifier depends on this to confirm the edge
  serves exactly the build that was pushed. The build id is supplied on push via
  the `X-Live-Preview-Build` header and persisted in a release marker that
  travels with the atomic flip.
- Push/DELETE require an auth.romaine.life service-principal JWT whose verified
  `sub` equals the configured authorized subject (the lease owner's IdP-signed
  subject) — "a pod may only write its own preview". `status` requires a valid
  token of any accepted role but is not owner-scoped.
- The receiver core (extract guards, atomic `current` symlink flip,
  prune-to-N-releases) is ported from tank-operator's static-override receiver;
  tank-operator's serving/toggle/SSE/daemon surface is not ported (it is the
  retired in-app path).
- Single serving pod is a hard invariant: the Helm partial's
  `live-preview-edge.replicas` returns 1 when enabled and fails the render on
  `replicas > 1` (v1 #1419: a per-pod emptyDir override behind multiple replicas
  load-balances reads and flickers the co-watched frontend).

Evidence:
- `internal/livepreview/release_test.go` — extract guards (missing index,
  path-escape, abs path, symlink/hardlink/device, entry cap, uncompressed bomb,
  bad gzip), atomic flip/replace, failed-push-keeps-previous, prune, status
  read-back, served-path resolution.
- `internal/livepreview/edge_test.go` — routing precedence, override serve + SPA
  fallback, fresh-preview passthrough, backend-prefix proxy, push/status/delete
  lifecycle with build flip, the push auth model (no-token/user/wrong-subject/
  owner), status auth posture, and bad pushes (missing build header, missing
  index, non-gzip, oversize, unauthorized) not changing the served bundle.
- `internal/metrics/metrics_test.go` — `glimmung_live_preview_edge_*` families
  wired and the served-build info gauge held at one active series.
- `k8s/live-preview-edge/` (library partial) + `k8s/live-preview-edge-harness/`
  rendered with `helm template`: edge fronts the app when enabled, inert when
  disabled, render fails on `replicas > 1`.
- `live-preview-edge/Dockerfile` (validated locally with the exact CGO-off
  build) plus the CI wiring: a `live-preview-edge` matrix entry in
  `docker-build-check.yaml` and a `check-deleted-test-slot-hot-swap` guard job in
  `tests.yaml` (the retired-hot-swap guard previously ran in no workflow, so
  wiring it in is what makes it a real gate).
- `docs/features/test-slots/live-preview.md` — the feature design + the staged
  plan to the complete architecture.

History:
- Stage 1 (this entry) builds the generic edge component only. The provisioning
  of a preview test env (stable backend + edge on a wildcard URL), the
  observed-read-back verifier, the in-pod push tool, and the cross-repo chart
  consumption mechanism are later stages named in
  `docs/features/test-slots/live-preview.md`. The complete feature replaces
  tank-operator's retired in-app static-override path.

## deploy-image-to-slot

Status: shipped

Intent:
`deploy_image_to_test_slot` resolves a verified pushed ref to the CI-built
image, dispatches the slot deploy, records durable job history, and polls the
neutral job-status route until terminal. The slot runs the same image that PR
CI proved and main will promote. Resolution is a direct registry lookup of the
commit-addressed `sha-<commit>` alias that `docker-build-check` publishes (a
pointer at the content-fingerprinted manifest) — the verified commit SHA is the
key, so there is no GitHub Actions run/PR/attempt reconstruction. Dispatch also refreshes a short active lease
to the configured hot-swap minimum remaining TTL before slow ref/image
resolution and Kubernetes deploy work begin, so active validation does not race
the original checkout deadline.

Affected contracts:
- Test Slots (primary — deploy image + job history)

Contract impact:
- Legitimacy (published + CI-green + mergeable + current-with-main) is enforced
  upstream by Tank's deterministic readiness gate — the sole caller,
  `provisionTestSlotForSession`, validates readiness before calling this
  endpoint. Glimmung's deploy is a pure provisioner over a verified `git_ref`; it
  does not re-derive or own that gate. (This corrects the older "the caller's
  responsibility" framing, which predated Tank's server-side gate.)
- The deploy input is only project, slot, and `git_ref`.
- Durable job history is keyed by the deploy operation and projected through
  `GET /v1/test-slots/jobs/{project}/{job}`.
- A deploy against an expired or cleanup-started lease fails instead of
  resurrecting the slot.
- The CI image is keyed by commit SHA, not CI-run identity. `docker-build-check`
  publishes a `sha-<commit>` alias of the fingerprinted manifest; the resolver
  looks that up directly. When the alias is absent it reads the commit's run to
  report build readiness: in-progress → retryable `409`; failed, succeeded-
  without-alias, or no-run → `422`. There is no `ci-pr`/`ci-ref` lookup tag and
  no dispatch-run probe.
- Build-stream project metadata and kind selection are retired and rejected on
  project registration.

Evidence:
- `internal/server/test_slot_deploy_image_resolver_test.go` —
  `TestResolvesCommitShaImage` (registry-only resolution, no GitHub call),
  `TestResolvePendingWhenAliasAbsentAndBuildRunning`,
  `TestResolveFailedBuildWhenAliasAbsent`,
  `TestResolveSucceededButNoAliasWhenAbsent`, `TestResolveNoBuildWhenAliasAbsent`,
  `TestResolveSurfacesNonNotFoundValidationError`.
- `internal/server/test_slot_deploy_image_api_test.go` —
  `TestDeployImageToTestSlotExtendsShortLeaseToHotSwapMinimum`,
  `TestDeployImageToTestSlotDoesNotShortenSufficientLease`, and
  `TestDeployImageToTestSlotReturns409WhileCIImagePending`.
- `internal/server/test_lease_defaults_api_test.go` —
  hot-swap minimum TTL route coverage for global and project settings.
- `.github/workflows/docker-build-check.yaml` ("Tag app image by commit") and
  `.github/workflows/build.yaml` (dead lookup step removed) — the producer side
  of the commit-addressed alias.

History:
- 2026-06-20: a test slot requested in the ~90s between PR open and the PR's
  `docker-build-check` run finishing failed with a misleading `422 … image tag
  …:ci-ref-<hash>-run-<id>-attempt-1 not found in registry`. Resolution had
  reconstructed a run-scoped lookup tag from GitHub Actions metadata; the
  primary `status=success&head_sha` lookup was empty during the build, so it fell
  into a `workflow_dispatch` probe that paired *this commit's* ref-hash with
  *every recent run's* id — combinations a PR commit (only ever `ci-pr-<n>`
  tagged) never published. The fix re-keys the lookup by commit SHA: CI publishes
  a `sha-<commit>` alias, the resolver looks it up directly, and GitHub is read
  only to explain a missing alias. This deleted the entire run/PR/attempt
  reconstruction (`ci-pr`/`ci-ref` tags, PR-number resolution, ref-hash, dispatch
  fallback) and the bug class it carried, including the earlier 2026-06-18
  empty-`pull_requests` regression.

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

## deploy-image-to-slot

Status: shipped

Intent:
`deploy_image_to_test_slot` resolves a verified pushed ref to the CI-built
image, dispatches the slot deploy, records durable job history, and polls the
neutral job-status route until terminal. The slot runs the same image that PR
CI proved and main will promote. Dispatch also refreshes a short active lease
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
- SHA→image resolution reports build readiness, not just success or failure. A
  commit whose `docker-build-check` run is still queued or in progress resolves
  to a retryable `409` ("CI image … is not ready yet … retry once the build
  completes"); a failed build, or a commit no build targets, resolves to `422`.
  The resolver only falls back to the `workflow_dispatch` ci-ref probe when no
  run targets the commit by head_sha, and never surfaces that probe's expected
  registry misses as the resolution error.
- Build-stream project metadata and kind selection are retired and rejected on
  project registration.

Evidence:
- `internal/server/test_slot_deploy_image_api_test.go` —
  `TestDeployImageToTestSlotExtendsShortLeaseToHotSwapMinimum`,
  `TestDeployImageToTestSlotDoesNotShortenSufficientLease`, and
  `TestDeployImageToTestSlotReturns409WhileCIImagePending`.
- `internal/server/test_slot_deploy_image_resolver_test.go` —
  `TestGitHubActionsTestSlotImageResolverReturnsPendingWhileBuildRunning`,
  `…ReportsFailedBuild`, `…NoBuildIsClearNotRegistryMiss`, and
  `…PrefersSuccessfulRerunOverFailedAttempt`.
- `internal/server/test_lease_defaults_api_test.go` —
  hot-swap minimum TTL route coverage for global and project settings.

History:
- 2026-06-20: a test slot requested in the ~90s between PR open and the PR's
  `docker-build-check` run finishing failed with `422 … image tag
  romainecr.azurecr.io/<repo>:ci-ref-<hash>-run-<id>-attempt-1 not found in
  registry`. The resolver's primary lookup filtered to `status=success` on the
  commit's head_sha, returned empty while the build was still running, and fell
  into the `workflow_dispatch` recovery probe — which pairs *this commit's*
  ref-hash with *every recent run's* id. PR commits only ever carry `ci-pr-<n>`
  tags, so every fabricated `ci-ref` tag 404'd and the last miss was surfaced as
  the failure. The fix classifies the commit's runs (success / in-progress /
  failed) before the probe and returns a typed, retryable pending error, so the
  race is reported as "not ready, retry" rather than a missing image.

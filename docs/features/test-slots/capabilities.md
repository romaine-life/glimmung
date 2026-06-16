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
  control-plane reconciler against shared Postgres; the omission only became
  visible when a hot-swapped reconciler began calling the apiserver for
  Jobs in `glimmung-runs` and hit 403 against the slot's narrowly-scoped
  ServiceAccount. The fix made the env var real rather than expanding slot
  RBAC.

## apply-hot-swap-async-finalize

Status: shipped

Intent:
`apply_test_slot_hot_swap` is asynchronous-with-poll. The POST dispatches the
build-and-swap Kubernetes Job and returns immediately with a `running` handle
plus an initial history entry; the build-and-swap deadline is enforced on the
Job (`activeDeadlineSeconds`); a gated finalizer records the terminal outcome
when the Job completes; the caller polls the status route until terminal. No
single HTTP request is held open for the build, so `timeout_seconds` is honored
by the poll loop and the durable outcome survives client disconnects, proxy
deadlines, and orchestrator rollouts.

This replaced a synchronous design that blocked one HTTP request for the whole
build. Because that request's context fed the Job wait *and* the history write,
a ~30s client/proxy deadline aborted the request and corrupted/dropped the
durable record while the Job itself ran on to completion — the caller saw only a
timeout. The durable lease history — never the response body — is now the source
of truth.

Affected contracts:
- Test Slots (primary — hot-swap apply + history)
- Observability And Evidence (`glimmung_hot_swap_outcomes_total` is incremented
  by the finalizer, once per terminal Job)

Contract impact:
- The dispatch (`DispatchHotSwap`) renders + submits the Job and returns
  `running`; it never calls a blocking `WaitForJob`. The build-and-swap timeout
  lives on `spec.activeDeadlineSeconds` (reason `DeadlineExceeded` → outcome
  `timeout`).
- The finalizer (`StartApplyHotSwapJobWatcher` → `dispatchHotSwapTerminal`)
  reuses the cluster-wide `k8sJobWatcher` and is gated by
  `Settings.ControlPlaneLoopsEnabled` (see `slot-control-plane-isolation`), so a
  slot process never finalizes prod Jobs. It is idempotent: a terminal entry is
  keyed by the job handle, so duplicate apiserver events and post-restart
  re-lists never double-record.
- The status route `GET /v1/test-slots/apply-hot-swap/{project}/{job}` is a
  read-only projection of the durable lease history (the poll surface).
- Every durable write in the dispatch path runs on `context.WithoutCancel` of
  the request, so a client disconnect cannot abort it.
- The fidelity classifier's diff context is resolved server-side (GitHub Compare
  API, `base...git_ref`) and plumbed into the build container as
  `GLIMMUNG_CHANGED_FILES`, because the shallow single-SHA build checkout cannot
  compute a real diff.

Evidence:
- `internal/server/apply_hot_swap_watcher_test.go` —
  `TestShouldStartApplyHotSwapJobWatcherGate` (unreachable when the control
  plane is off), `TestDispatchHotSwapTerminalRecordsOutcome`,
  `TestDispatchHotSwapTerminalIsIdempotent`,
  `TestDispatchHotSwapTerminalTimeoutOutcome`,
  `TestGetApplyHotSwapStatusReturnsLatestEntry`.
- `internal/server/test_slot_apply_hot_swap_ops_test.go` —
  `TestDispatchHotSwap*` (renders the Job + `activeDeadlineSeconds`, returns
  `running`, no delete on dispatch), `TestFinalizeHotSwapClassifiesOutcome`.
- `internal/server/test_slot_apply_hot_swap_api_test.go` —
  `TestApplyTestSlotHotSwapDispatchReturnsRunningAndPlumbsDiff`.
- `internal/server/hot_swap_diff_test.go` —
  `TestResolveHotSwapDiffComputesChangedFiles`.
- `scripts/check-apply-test-slot-hot-swap-migration.mjs` — the completion
  manifest, updated to pin the async invariants and forbid the synchronous
  `WaitForJob` block from returning.

History:
- The synchronous endpoint shipped first (the original
  `check-apply-test-slot-hot-swap-migration.mjs`). A developer hot-swapping a
  static frontend change observed the `apply_test_slot_hot_swap` MCP call return
  `timed out` at ~30-35s while the Kubernetes Job completed successfully ~87s
  later, and the classifier printed an empty `changed_files: []` because the
  shallow checkout had no base ref. Both were fixed together: the call path went
  async-with-poll (durable finalize, request-detached writes, per-request poll
  timeouts in `mcp-glimmung`) and glimmung began resolving the diff context
  server-side.

# Workflow Execution Contract

This contract applies to workflow registration, schema snapshots, phase/job
shape, native Kubernetes job launch, managed evidence gates, callback tokens,
and workflow sync helpers.

## Product Model

A Workflow is the registered automation shape for a project. It gives agents a
precise lane: prepare capacity, do work, verify the result, and clean up. The
workflow graph should be legible before dispatch and should remain reconstructable
after registration changes.

## Sources Of Truth

- Postgres `workflows` owns logical workflow registrations and current schema
  pointers. Its `control_pins` column is operator-owned: only the dedicated
  pin/unpin endpoints write it, never registration.
- Postgres `workflow_control_events` owns the append-only attribution ledger
  for control-plane writes (register, patch, pin, unpin, delete) — schema
  rows are content-addressed and cannot carry who-moved-the-pointer history.
- Historical workflow schemas referenced by runs own projection for past
  cycles.
- `docs/workflow-shape.md` owns required phases, linear topology, job
  concurrency, evidence gate semantics, and path-typed identity.
- Native Kubernetes Jobs own execution process state while running.
- Native job event rows own hot execution telemetry.
- Workflow import/sync inputs are admin conveniences only. They are not
  required in consumer repositories and are never the runtime source of truth.

## Migration Rules

- Do not make repo workflow files the dispatch source of truth.
- Do not register executor kinds other than `k8s_job`. Review gates are
  `k8s_job` phases with `purpose: review_gate`; a review-gate phase must
  declare exactly one `pr_merge` primitive job and no other jobs. The
  `pr_merge` primitive must live inside a `purpose: review_gate` phase and
  nowhere else.
- Do not add phase fan-in, fan-out, job-level dependencies, or non-linear DAG
  behavior without replacing this contract.
- Do not allow project-owned arbitrary gate jobs to stand in for the managed
  evidence gate.
- Do not reintroduce `skip_when_preserve_test_env`; registration rejects the
  retired field with a pointer at the
  `when: "${{ run.preserve_test_env }} == 'false'"` replacement.
- Do not reintroduce the retired issue-contract entry-phase mandate. The
  platform validates the generic prepare/verify/teardown skeleton only;
  project stage names and outputs inside `prepare` are project-owned.
- Do not delete historical schemas still referenced by run history.
- Do not start a workflow-execution background reconciler (run queue,
  dispatch timeout, completion sweep, native Job inspection, etc.) outside
  the `settings.ControlPlaneLoopsEnabled` gate in `cmd/glimmung-go/main.go`.
  The control-plane isolation boundary belongs to the
  [Test Slots contract](../test-slots/contract.md); a workflow-execution
  reconciler that ignores it lets a hot-swapped slot binary race the prod
  glimmung Deployment on the same runs, Postgres rows, and `glimmung-runs`
  Kubernetes Jobs.

## Live Behavior

- Registration rejects missing entry, verify, or teardown cleanup phases.
- Registration rejects invalid dependencies, duplicate phases, duplicate job
  IDs, invalid inputs, and unsupported executor kinds before they become a
  runtime contract.
- A verification phase must declare `recycle_policy` explicitly; registration
  rejects a nil policy naming the phase. There is no implicit platform
  attempt count for silence to inherit.
- Every workflow write flows through one choke point that enforces operator
  control pins: a pinned target's incoming value is discarded in favor of the
  pinned value before the schema hash is computed, the override is reported
  on the response and ledger, and a pin whose target phase is absent from the
  incoming registration rejects the registration. Patches naming a pinned
  target are rejected with the pinner, reason, and unpin remediation. No
  write path may mutate the workflow payload without minting a schema and a
  ledger event.
- A `verify=true` phase is a bounded verification phase whose concrete shape is
  selected by the persisted workflow constraint
  `constraints.verification.shape`. Supported shapes are `single_job`,
  `bounded_case_jobs`, and `dynamic_step_group`; code validates the selected
  primitive, but the workflow row owns which one applies. `dynamic_step_group`
  declares exactly one sequential verification job with one dynamic test-case
  block whose `dynamic_group.max_items` is at or below 10.
  `bounded_case_jobs` declares ten jobs named `verify-case-01` through
  `verify-case-10`; every case job sets `timeout_seconds` at or below 600
  seconds, and unused slots complete successfully with no-op case results.
- Jobs inside one phase launch in parallel and complete independently.
- Phase- and job-level `when` conditions (closed grammar over registration
  `vars`, declared dispatch inputs, and the closed run-fact set) are
  evaluated server-side at dispatch. A false condition creates no Kubernetes
  Job: the platform synthesizes durable skipped attempt/job/step records
  attributed to the resolved condition. Skipped jobs pre-satisfy the
  expected-job completion set, are verdict-neutral in phase aggregation
  (never degrading success, never masking a sibling failure), and their
  unpublished declared outputs resolve to empty strings in downstream input
  substitution; phases with no skips keep fail-closed substitution.
  Registration rejects `when` on verification and review-gate phases/jobs,
  on entry phases, and on a phase whose every job is conditional.
- A step-scoped fail-closed abort is represented by a typed `step_aborted`
  native event and a durable aborted step state. A failed or aborted job whose
  cause is step-scoped must not project with every step succeeded or
  not-started.
- A phase/job dispatch failure before a Kubernetes Job exists is represented as
  a failed workflow-owned `dispatch` step. Declared workflow steps remain
  `not_started`; the synthetic dispatch step owns the terminal failure instead
  of leaving the human UI without a failed node.
- `touchpoint_gate` is a gated native phase name, not an executor kind:
  reaching the `purpose: review_gate` phase creates a durable parked `k8s_job`
  attempt at the human decision boundary, and approve later releases that same
  attempt's managed `pr_merge` job through the ordinary native event,
  completion, watcher, and recovery paths.
- Phase advancement happens only after all registered jobs in the phase reach
  terminal callback state.
- Verification phases preserve verification statuses, reasons, evidence refs,
  and typed evidence artifacts in the phase completion. Glimmung synthesizes
  the phase output `verification` so the managed evidence gate has a stable
  JSON verdict.
  Multi-job verification phases aggregate per-job verification data before
  synthesizing that phase output.
- Dynamic verification groups are runtime-expanded inside one managed job from
  bounded plan outputs (`test_cases_json` or `test_cases_count`, plus
  group-specific aliases). Expanded case steps are emitted as durable native
  step events with concrete slugs and group metadata; template steps must not
  remain the only visible execution record after expansion.
- Evidence verification gates are canonicalized into managed Glimmung runner
  jobs.
- Dispatch may include bounded string `inputs`. These are durable run facts,
  persisted on the Run as `run_inputs`, copied into every native lease's
  metadata, exposed to native pods as `GLIMMUNG_RUN_INPUT_*`, and preserved
  across recycle attempts. Native `checkout.ref` and `extra_checkouts[].ref`
  may use exact run-input templates such as `${{ inputs.git_ref }}`; the
  launcher resolves them before emitting the native runner job spec so the
  runner receives a concrete ref.
- Runs use the workflow schema snapshot captured at run/cycle creation, not a
  later logical workflow update.

## Failure And Recovery

- A failed native Job produces durable job/phase failure state through the
  completion callback path, not a retired failure route.
- Teardown phases (`purpose: teardown`) are verdict-neutral. A teardown job's
  outcome never sets the run verdict: a failed teardown must not abort an
  otherwise-passing run, and must not override or mask the primary phase's
  verdict. The decision engine advances the cleanup chain regardless of a
  teardown conclusion, and terminal-cause attribution skips teardown attempts.
  The failed teardown remains visible on its own job/step state. Teardown jobs
  carry a bounded `backoffLimit` so a transient pod-start failure self-heals;
  producer/verify jobs keep `backoffLimit=0` (fail fast).
- A failed dispatch produces durable phase/job/step failure state through run
  terminal finalization and run graph projection, including historical rows
  whose child jobs or steps were previously stamped `skipped`.
- Callback-token validation failure must not mutate unrelated runs or phases.
- Workflow update failure should leave the previous logical workflow pointer
  intact.
- Service restart must preserve the ability to project active and historical
  cycles from schema refs and run ledgers.

## Observability

- Native event streams should identify project, issue, run, cycle, phase, job,
  step, conclusion, and relevant log tail or archive link.
- A pod that dies for a pod-level reason (OOMKilled, Evicted) must surface that
  reason, not just the Job condition. The Kubernetes Job condition collapses to
  `BackoffLimitExceeded` for any `backoffLimit=0` pod failure, so Glimmung reads
  the pod's `containerStatuses[].state.terminated.reason` / `status.reason` on
  terminal failure and refines the terminal reason to `oom_killed` / `evicted`
  (and keeps the raw reason in the human summary). This applies to outer Jobs
  and inner per-case Jobs alike.
- Registration failures should name the exact invalid phase, dependency, input,
  job, or unsupported kind.
- Run graph projection should make schema mismatch or missing schema failures
  explicit.

## Acceptance Checks

- Workflow shape changes include registration validation tests.
- `when`/`vars` changes include grammar-validation tests, dispatch-behavior
  tests for both phase-level and job-level skips, skip-neutral phase
  aggregation tests, and skipped-output substitution tests.
- Control-pin changes include tests for pin enforcement on re-registration,
  pinned-patch rejection, the closed pin-target grammar, and ledger/actor
  attribution on the write surface.
- Native launcher/callback changes include multi-job phase behavior when the
  change can affect phase completion.
- Dispatch-input changes include tests for run persistence, native lease/env
  propagation, checkout-ref resolution, missing-input failure, and recycle
  preservation when applicable.
- Verification-case shape changes include tests for required case IDs, bounded
  timeouts, and synthesized aggregate verification output.
- Gate changes prove managed gate canonicalization and terminal behavior.
- Teardown-routing changes prove a failed teardown stays verdict-neutral: it
  does not abort a passing run and does not become the run's terminal cause
  over a failed primary phase.
- Sync-helper changes prove they do not bypass registration validation.
- Historical run projection still works when the logical workflow changes.

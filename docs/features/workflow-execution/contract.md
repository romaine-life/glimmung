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
  pointers.
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
- Evidence verification gates are canonicalized into managed Glimmung runner
  jobs.
- Runs use the workflow schema snapshot captured at run/cycle creation, not a
  later logical workflow update.

## Failure And Recovery

- A failed native Job produces durable job/phase failure state through the
  completion callback path, not a retired failure route.
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
- Registration failures should name the exact invalid phase, dependency, input,
  job, or unsupported kind.
- Run graph projection should make schema mismatch or missing schema failures
  explicit.

## Acceptance Checks

- Workflow shape changes include registration validation tests.
- Native launcher/callback changes include multi-job phase behavior when the
  change can affect phase completion.
- Verification-case shape changes include tests for required case IDs, bounded
  timeouts, and synthesized aggregate verification output.
- Gate changes prove managed gate canonicalization and terminal behavior.
- Sync-helper changes prove they do not bypass registration validation.
- Historical run projection still works when the logical workflow changes.

# Issues And Runs Contract

This contract applies to Glimmung issues, run dispatch, run/cycle numbering,
locks, verify-loop retry, runner callbacks, abort/failure state, and run
history projection.

## Product Model

An Issue is the canonical work target. A Run is one invocation of a workflow for
that Issue. A Cycle is the durable execution ledger created by retry/recycle
inside a Run. The product must never make a user infer whether work is queued,
running, retried, failed, or complete from GitHub Actions side effects or
browser memory.

## Sources Of Truth

- Postgres `issues` owns the canonical issue row and project-scoped issue number.
- Postgres `runs` owns run state, cycle numbering, phase/job/step ledgers,
  callback metadata, cost, validation URL, abort reason, typed terminal
  observation, terminal state, and the resolved agent runtime snapshot.
- Durable global settings, project metadata, and issue metadata own agent
  runtime defaults before dispatch. Each layer must say `inherit` or
  `override`; executor images and CLI defaults are not sources of truth.
- Postgres `locks` owns issue and PR mutual exclusion.
- Runner job callbacks to `/v1/run-callbacks/{callback_token}/run/completed`
  own job completion input.
- `docs/workflow-shape.md` owns run/cycle identity, workflow schema snapshots,
  and verify/recycle model.
- GitHub Issues are not the live run-loop source of truth.

## Migration Rules

- Do not reintroduce GitHub Issues as the canonical run trigger.
- Do not add GitHub Actions workflow-run state as a canonical run-state source.
- Do not add manual mid-run restart as a product route; user-driven rerun
  creates a new Run, and recycle policy creates a new Cycle.
- Do not store path strings as canonical IDs when they can be computed from
  typed entity identity.
- Do not keep retired callback routes, route aliases, or tests for runner
  failure endpoints.

## Live Behavior

- Dispatch resolves project, workflow, and issue from durable records before
  creating run state.
- Dispatch resolves agent runtime from global defaults, project config, and
  issue policy before creating run state, then snapshots the resolved default
  and slot profiles onto the Run and initial lease.
- Dispatch serializes active work per issue on durable run state and the issue
  lock: a second dispatch is refused (`already_running`) while the issue has a
  non-terminal run, so serialization holds even if the lock's TTL lapses under a
  long wait.
- Dispatch creates the run before claiming a slot. When no test slot is free the
  run is created `queued` (holding the issue lock) and the run-queue reconciler
  admits it when capacity appears; dispatch does not hard-fail on transient
  no-capacity, and the queued/waiting state is visible in run state and the
  dashboard rather than left for the user to infer.
- No runner work starts without a claimed lease or the configured admission
  state for queued runs.
- Job completion callbacks include `job_id`; phase completion waits for every
  registered job in the phase.
- Any terminal failure of any class is a typed, owned failure, not an absence
  of evidence. When a run settles into a terminal failure state — for any class
  in the canonical set (`producer_phase_failed`, `verifier_contract_missing`,
  `verifier_failed`, `gate_failed`, `dispatch_failed`, `phase_requested_abort`,
  `manual_abort`, `malformed_terminal`) — run history must expose the failed
  phase/job, a failed owner step, and a specific typed `terminal_observation`
  cause, so no aborted run can show every workflow step as succeeded, skipped,
  or not-started. A phase dispatch failure is one example: it owns a failed
  `dispatch` step because no workload step has started yet. The canonical class
  list is the single source of truth, so a future failure class cannot ship
  without an owned, attributed terminal projection.
- Recycle policy creates a new Cycle under the same Run. Manual rerun after a
  terminal state creates a new Run.
- Run display numbering remains stable across reloads and schema changes.

## Failure And Recovery

- Concurrent dispatch returns a clear already-running/admission state rather
  than creating duplicate active runs.
- Callback replay is idempotent at the job/cycle boundary.
- Missing capacity does not launch executor work under an unclaimed lease.
- A service restart must project existing run state from Postgres rather than
  losing queued, running, terminal, or callback-waiting state.
- Terminal paths release issue/PR locks through durable store operations.

## Observability

- Run state, current phase, attempts/cycles, abort reason, cost, validation
  URL, typed terminal observation, and callback status must be inspectable
  through API/UI surfaces.
- Typed terminal observations for any terminal failure must carry owner
  identity — phase identity, job identity when known, and `step_slug` where
  determinable — plus a specific, non-generic reason naming the actual cause.
  A dispatch failure is one example: it carries `step_slug=dispatch` with a
  normalized reason (`dispatch_failed` or `dispatch_timeout`) because no
  workload step has started yet. When attribution cannot be resolved, the run
  must settle as the loud `malformed_terminal` signal that names what was
  missing — never a silent generic or an empty/`unknown` class.
- Runner event inspection should let an operator map hot job events back to
  run, cycle, phase, job, and step.
- Lock contention and duplicate dispatch attempts should be logged or surfaced
  clearly enough to distinguish user contention from a stuck run.

## Acceptance Checks

- Dispatch and callback changes include unit/integration tests for lock,
  run-state, and callback idempotency paths.
- Workflow schema or numbering changes preserve historical run projection.
- Verify-loop changes prove retry, terminal, and cleanup behavior.
- Any issue/run UI change reloads from durable state and does not depend on
  browser-local ordering.
- Agent runtime changes prove inheritance, override, and run snapshot behavior;
  historical runs must keep showing the profile selected at dispatch time.
- Retired GitHub Issue or GitHub Actions run-loop paths are deleted end to end.

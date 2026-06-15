# Review Surfaces Contract

This contract applies to Reviews, RunReports, PR syndication, signals,
Playbooks, evidence links, screenshots, and reviewer decision state.

## Product Model

Glimmung exists to make agent work reviewable. Review surfaces should answer
two different questions without mixing them: what should the human decide now,
and what happened in a specific run. Reviews own the current decision;
RunReports own factual per-run audit state.

## Sources Of Truth

- Postgres `issues` owns the work item state.
- Postgres `runs` and runner events own per-run facts.
- Postgres `reviews` physically stores Review state.
- Postgres `playbooks` owns ordered multi-issue planning and execution state.
- Postgres `signals` owns reviewer feedback and re-entry requests.
- GitHub PRs are syndication/review targets, not the canonical Glimmung issue
  or run record.
- `docs/reviews-runreports-playbooks.md` owns object boundaries and
  integration strategy vocabulary.

## Migration Rules

- Do not add multiple Reviews per Issue.
- Do not store per-run historical facts primarily on the Review.
- Do not reintroduce Report-named routes, UI controls, aliases, or tests for
  migrated Review concepts.
- Do not model GitHub PR merge/reject events as hidden side effects when the
  signal bus is the canonical feedback input.
- Do not create full human review Reviews for automatic Playbook entries
  unless that entry is a human decision boundary.

## Live Behavior

- A Review summarizes the current decision surface for exactly one Issue.
- A RunReport reports facts for exactly one Run: attempts, cost, validation
  URL, typed evidence, abort reason, terminal status, and evidence
  requirements.
- Reviewer feedback enters as signals and re-enters the run loop through
  durable issue/run state. Both `payload.kind: "approve"` and
  `payload.kind: "reject"` are first-class glimmung_ui signal kinds; approve
  releases a workflow's `review_gate` to merge, reject recycles via the
  configured `pr.recycle_policy`.
- Reviewer decisions are attributed. The authenticated principal that submits
  an approve / reject / cancel signal is captured on the signal at enqueue time
  (derived from the session, never the request body, so it cannot be forged)
  and stamped onto the reviewed run as durable per-run attribution
  (`reviewed_by`, `reviewed_at`, `review_decision`). A gate never advances while
  its authorship is dropped: the attribution is written before the gate
  releases or the recycle dispatches.
- A run sitting at a `review_gate` has Run state `review_required`. That
  state is non-terminal: locks stay held, the slot may be alive, projections
  treat the run as active, and the run advances forward when approve fires.
- Merged Reviews close their Issue in the normal isolated-PR case. When
  a workflow declares a `review_gate` phase, the run reaches terminal
  `passed` only by going through `approve → pr_merge → cleanup_final`, and
  the originating Issue transitions to state `closed` on that terminal
  transition.
- Playbook integration strategy controls where entries land and when a final
  review surface is required.

## Failure And Recovery

- Missing or delayed PR syndication must not erase the canonical Glimmung run
  and issue state.
- A failed signal drain leaves durable queued signal state or clear failure
  evidence.
- RunReport derivation failure should surface as missing/invalid report state,
  not as a misleading successful review.
- Playbook advance failures should preserve prior entry state and gates.

## Observability

- Review, RunReport, signal, and Playbook APIs should expose enough state
  for an operator to distinguish missing evidence, failed syndication, queued
  feedback, and failed rerun.
- PR body generation should name issue/run refs and the Glimmung Review.
  Review evidence, including WebM videos and screenshots, is stored and
  rendered by Glimmung rather than copied into the GitHub PR body.
- Signal drain logs should identify target repo/ref, source, kind, and outcome.
- Reviewer attribution is durable and queryable: the RunReport exposes
  `reviewed_by` / `reviewed_at` / `review_decision`, and the run event ledger
  records each decision as a `review_approve` / `review_reject` /
  `review_cancel` event carrying the acting principal and origin signal id.

## Acceptance Checks

- Review changes preserve one-to-one Issue cardinality.
- RunReport changes preserve one-Run scope and include factual evidence fields.
- Signal changes include tests or evidence for durable enqueue and drain
  behavior.
- Reviewer-decision changes preserve attribution: the approving, rejecting, or
  cancelling principal is recorded on the reviewed run and surfaced on the
  RunReport. A gate release or recycle cannot regress to an anonymous decision.
- Playbook changes prove dependency/gate/integration behavior for the changed
  path.
- PR syndication changes show that GitHub remains a projection of Glimmung
  state rather than the canonical source.

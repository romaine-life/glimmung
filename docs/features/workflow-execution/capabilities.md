# Workflow Execution Capabilities

## workflow-tombstone-delete

- **Status:** active
- **Intent:** Removing a workflow from ordinary operator surfaces must not
  strand historical runs, parked review gates, replay, or repair. Observed
  failure this exists to prevent: `ambience#164/runs/12.1` reached
  `review_required` on workflow `sidecartest`; after the logical workflow row
  was physically deleted on 2026-06-15, review-gate approval/replay could no
  longer resolve the workflow normally.
- **Mechanism:** Workflow delete is a durable lifecycle mutation on the
  existing `workflows` row: `metadata.deleted_at`, `metadata.deleted_by`,
  `metadata.usable=false`, and `metadata.visible=false`. Normal list surfaces
  hide hidden/deleted rows; new explicit dispatch rejects unusable/deleted
  rows; backend history, signal, replay, and repair paths retain access to the
  row and historical schemas.
- **Affected contracts:** Workflow Execution (logical registrations, schema
  snapshots, control ledger), Issues And Runs (historical run projection and
  abortability), Review Surfaces (`review_required` remains non-terminal and
  parked gates remain resolvable).
- **Evidence:** `TestTombstoneWorkflowPayloadPreservesRowAsUnusableHiddenWorkflow`,
  `TestWorkflowDeleteDoesNotPhysicallyDeleteWorkflowRows`,
  `TestWorkflowHiddenFromLists`,
  `TestDispatchRunRejectsExplicitTombstonedWorkflow`,
  `TestWorkflowTombstoned`, and
  `TestAdminAbortAlreadyTerminalState`.

## operator-control-pins

- **Status:** active
- **Intent:** Operator-chosen control values (recycle policies, budget) must
  survive agent re-registrations. Observed failure this exists to prevent:
  `ambience.default` `llm-verify` `max_attempts` was deliberately set to 1 by
  the operator on 2026-06-11 and flipped back to 3 by a session
  re-registration the same afternoon (schema `wfs_bb87d34023e3bb4c`); earlier,
  an explicit 3 first materialized as a side effect of an unrelated migration
  re-registration (2026-06-08, `wfs_1ae90a036a8066b7`) with no record of
  anyone deciding it. Workflow documents are replaced wholesale on register,
  so every re-registration silently re-decides every control value unless the
  control plane defends them.
- **Mechanism:** operator-owned `workflows.control_pins` column enforced at
  the single registration choke point; append-only `workflow_control_events`
  attribution ledger (actor + control diff per write); explicit
  `recycle_policy` required on verification phases; `X-Glimmung-Actor`
  forwarded caller detail on service writes.
- **Affected contracts:** Workflow Execution (pins, ledger, validation);
  Auth And API Surface (three new routes, MCP rollout in `mcp-glimmung`);
  Dashboard And Styleguide (pin state + control history on the workflow
  page).
- **Evidence:** `TestEnforceControlPins*`, `TestControlChanges*`,
  `TestPinWorkflowControl*`, `TestValidateWorkflowRegisterRejectsVerificationPhaseWithoutRecyclePolicy`,
  route inventory update, `RecyclePolicyPanel` pin-flow tests, and the live
  post-deploy check: a re-registration attempting `max_attempts: 3` against
  the pinned `ambience.default` verify lane is held at 1 with a
  `pin_enforced` ledger entry.

## run-harness-sdk

- **Status:** active
- **Intent:** A workflow step body must be physically unable to emit an untyped
  error, skip the verification verdict, or mislabel a harness crash as a model
  failure. Observed failure class this exists to prevent: a `throw` in a
  hand-rolled `scripts/glimmung-native/lib.sh` step body becomes `exit 1` with
  the real reason stranded in stderr, and a harness crash before the model ran
  is terminal-attributed against the model at $0 cost — filling
  `suspected_cause` wrong. Each consumer app forked ~600 lines of near-identical
  shell that could not carry the contract.
- **Mechanism:** the public `harness/...` Go SDK holds the run contract as types
  — `harness/step` (Handler/Registry/Main spine, fail-closed typed `Context`,
  honest exit contract, `LayeredError{harness|host|model}`), `harness/agent`
  (the only origin of a model-layer error; line-unbuffered usage streaming so
  `agentcost` prices it), `harness/verification` (typed finalizable +
  deterministic gate), `harness/evidence` (boundary-exact matchers + TRX),
  `harness/remotehost` (typed venue). The shared `{layer,code,message}` wire
  shape lives in `internal/domain/steperr`. Additively, the runner reads an
  optional typed `error` block from `GLIMMUNG_COMPLETION_FILE` on step failure
  and the store promotes it into the producer-phase terminal cause; completions
  without the block are byte-for-byte unchanged.
- **Affected contracts:** Workflow Execution (step-producer surface, step-body
  failure projection), Observability And Evidence (typed terminal cause for a
  producer step crash; verification.json finalizable shape).
- **Evidence:** `harness/step` dispatch/abort/fail-closed/layer-translation +
  emitter-parity tests; `harness/agent` `$0`-bug streaming guard;
  `harness/verification` finalizer-shape + gate tests; `harness/evidence` ported
  Pester cases; `harness/remotehost` mint/arg/host-layer tests; the no-drift
  contract test `TestSDKEmissionsRoundTripThroughRunnerParsers`; the runner
  attribution tests (`TestRunnerPromotesTypedStepErrorOnFailure`,
  `TestRunnerFailureWithoutBlockIsUnchanged`); the store cause-promotion tests
  (`TestTerminalObservationPromotesTypedStepError`,
  `TestTerminalObservationWithoutStepErrorIsUnchanged`); and the migration guard
  `TestSDKIsTheOnlySanctionedStepProducerSurface`.

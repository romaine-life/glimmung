# Observability And Evidence Capabilities

This ledger names user-facing behavior under the observability and evidence
contract. It is not a backlog. Entries land here when the behavior needs a
stable handle for planning, review, tests, incident follow-up, or retirement.

## no-invisible-terminal-failure

Status: shipped

Intent:
No run may reach a terminal failed/aborted state that is invisible or
unattributed. Observed failure class this exists to prevent (spirelens#147): a
terminal failure could leave the run-graph inspector showing every workflow
step as succeeded, skipped, or not-started, with no failed owner step and no
specific cause — the run "failed" but every visible signal said it had not.
The invariant generalizes the original dispatch-only guarantee to a total one:
for EVERY terminal failure class, the run must carry (1) a typed terminal
observation naming a known class with owner identity and a SPECIFIC cause,
(2) a failed owner step in the run-graph projection, and (3) an inspector that
renders that cause in place — backed by a runtime metric/alert and an
enum-driven regression guard so a future class cannot regress into silence.

Mechanism:
- A single canonical class set, `server.AllTerminalObservationClasses`
  (`producer_phase_failed`, `verifier_contract_missing`, `verifier_failed`,
  `gate_failed`, `dispatch_failed`, `phase_requested_abort`, `manual_abort`,
  `malformed_terminal`), is the one source of truth tying attribution,
  projection, render, metric, and guard together.
- Fail-closed attribution at the terminal-write choke point:
  `server.GuardTerminalFailureObservation` rejects an absent observation, an
  empty/`unknown` class, or an empty message and rewrites it into a loud
  `malformed_terminal` observation naming what was missing — never a silent
  generic.
- Run-graph owner-step projection: `server.ensureFailedJobOwnerStep` guarantees
  a failed job always owns a failed step, so a failed job never renders
  all-green.
- Inspector render: the class-agnostic `.run-failure-cause` block in
  `IssueDetailView.tsx` (`VerificationFailureDetail`) renders the typed
  `terminal_observation`, `abort_reason`, and the deciding verification's
  expected/observed/reasons for any class.
- Runtime backstop: `metrics.RecordRunTerminal` increments
  `glimmung_run_terminal_total{class,state}` once per terminal settle at the
  guarded choke point (bounded labels, no double-count), and the
  `GlimmungRunTerminalUnattributed` Prometheus alert pages when
  `class=malformed_terminal|unknown`.

Affected contracts:
- Issues And Runs (terminal failure is a typed owned failure across all classes;
  run history exposes the failed phase/job, owner step, and typed cause).
- Dashboard And Styleguide (run-graph inspector renders the specific cause and a
  failed job always owns a failed step).
- Observability And Evidence (primary — typed terminal observations MUST
  distinguish every class; the metric and alert are required invariants with the
  fail-closed no-unattributed guard and no double-count).
- Workflow Execution (terminal failure classes originate from workflow phase /
  verify / gate / dispatch outcomes; the canonical class set bounds them).

Evidence:
- Canonical class set + enum-driven regression guard:
  `internal/server/terminal_observation.go::AllTerminalObservationClasses`,
  `internal/server/terminal_observation_inventory_test.go::TestAllTerminalObservationClassesInventoryIsExact`
  and `::TestEveryTerminalClassProjectsAFailedOwnerStep`.
- Fail-closed attribution: `server.GuardTerminalFailureObservation` with
  `internal/server/terminal_observation_test.go` (guard cases) and store-side
  `internal/store/store/terminal_observation_test.go::TestTerminalObservationAttributesEveryClass`,
  `::TestTerminalObservationFixtureCoversEveryCanonicalClass`,
  `::TestTerminalObservationMalformedIsLoud`,
  `::TestTerminalObservationNamesDispatchFailureAsDispatchStep`.
- Run-graph owner-step projection: `server.ensureFailedJobOwnerStep` with
  `internal/server/graph_api_test.go::assertFailedJobsOwnAFailedStep`,
  `::TestEnsureFailedJobOwnerStepShapes`,
  `::TestRunCycleGraphProjectionOwnsVerifierVerdictFailure`,
  `::TestRunCycleGraphProjectionShowsForwardDispatchFailureWithDispatchStepOwnership`.
- Inspector render: `frontend/src/IssueDetailView.tsx` `VerificationFailureDetail`
  with `frontend/src/IssueDetailView.test.tsx` "renders the terminal verify
  cause in the inspector when a failed verify job is selected", the per-class
  inventory test "renders a non-blank inspector cause for terminal class %s"
  over `ALL_TERMINAL_OBSERVATION_CLASSES`, and "the terminal class list matches
  the canonical set".
- Metric backstop: `metrics.RecordRunTerminal` →
  `glimmung_run_terminal_total{class,state}` with
  `internal/metrics/metrics_test.go::TestRecordRunTerminalLabelsAndSingleIncrement`,
  `::TestRecordRunTerminalEmptyClassCoercesToUnknown`, and store-side
  no-double-count `internal/store/store/terminal_settle_metric_test.go::TestRecordTerminalSettleDoesNotDoubleCount`,
  `::TestSetRunTerminalSettleDoesNotRecountAlreadyTerminalRun`;
  class derivation `internal/server/terminal_observation_metric_test.go::TestTerminalObservationMetricClass`,
  `::TestTerminalObservationClassUnattributed`.
- Alert backstop: `GlimmungRunTerminalUnattributed` in
  `k8s/templates/prometheusrule.yaml` (fires on
  `class=~"malformed_terminal|unknown"`).

## durable-inspections

Status: active

Intent:
Make every `inspect_browser_url` invocation a durable artifact record so the
screenshot and inspection report survive the calling MCP tool response,
stay referenceable by `/v1/artifacts/...`, and compose with the existing
`pr_review` finalize machinery rather than burning agent context as
inline base64.

Affected contracts:
- Observability And Evidence (primary)
- Auth And API Surface (new MCP-used route + tool schema reshape)
- Test Slots (lease-cleanup goroutine gains an artifact sweep step)
- Review Surfaces (Run-bound inspections flow into Review evidence
  through the existing `pr_review` primitive — no new caller-facing
  promotion API)

Contract impact:
- Glimmung becomes the first writer into the artifact store. Existing run
  evidence is still uploaded by agent runners via the stdout base64-tar
  side-channel; convergence onto a single write surface is a documented
  follow-up.
- `slot_inspections` is the durable Postgres ledger for every
  inspection record. Lease-scoped (`scope='lease'`, `lease_id` set) and
  run-scoped (`scope='run'`, `run_id` set) rows live in the same table;
  the `scope` column distinguishes them at query time.
- Run binding is **caller-declared** at POST time, not derived from
  lease metadata. Test-slot leases intentionally live across multiple
  runs (a slot is a session-owned reservation, not a run-owned
  reservation), so the test-slot lease has no stable `run_id`. The
  POST `/v1/inspections` handler accepts an optional `run_id` form
  field; when supplied, glimmung validates the run exists under the
  lease's project and writes the bytes under
  `runs/<project>/<run_id>/inspections/<id>/...`. When absent, bytes
  land under `inspections/<lease_id>/<id>/...` (the default).
- Lease-cleanup is the retention boundary for **lease-scoped**
  inspections: every `scope='lease'` row matching the lease + its
  `report.json` + `screenshot.png` is deleted as part of
  `cleanupTestSlotRuntime`. **Run-scoped rows survive lease cleanup**
  and follow Run evidence retention semantics (the same as Run videos
  and screenshots): no per-row sweep, governed by whatever global
  retention policy the artifact store implements.
- Artifact-path whitelist grows by one prefix (`inspections/`).
  `review_evidence` resolver canonicalizes `inspections/` refs into
  the standard `blob://artifacts/...` shape so a testing job that emits
  an inspection ref in `verification.evidence` is normalized into
  Review evidence at finalize, exactly like screenshots / videos
  /evidence / refs.
- New metric family `glimmung_inspections_*` with closed-enum labels
  (`scope`, `phase`, `piece`, `outcome`). No project/lease/session
  identifiers in labels.

Evidence:
- `internal/server/inspection_api_test.go` — write-contract, idempotent
  retry, ledger rollback, missing-part rejection, invalid-JSON, missing
  lease, GET-by-id detail shape, GET-by-id 404 for missing, list with
  no filter / lease filter / project filter / invalid-limit rejection.
- `internal/server/inspection_sweep_test.go` — lease-cleanup deletes
  rows and blobs; no-rows no-op; nil-store no-crash.
- `internal/server/artifact_api_test.go` — `inspections/` prefix served;
  `inspections/../escape` rejected.
- `internal/server/route_inventory_test.go` — `POST /v1/inspections`,
  `GET /v1/inspections`, and `GET /v1/inspections/{inspection_id}`
  routes registered in the expected order.
- Runner-evidence convergence: the original glimmung#143 plan named a
  "convergence of the agent-runner stdout-base64-tar evidence path onto
  `POST /v1/inspections`" follow-up. Investigation while doing the
  follow-up showed the framing was wrong: the stdout-base64-tar
  emission in `internal/ops/agentops/job.go` (the developer-CLI
  `glimmung-agent apply-agent-job` script) had **no consumer anywhere
  in the repo**. The production runner (`glimmung-runner`)
  uses a different completion-file ref path and does not currently
  upload evidence bytes. The honest action per migration-policy
  ("vestigial code is a deletion target") was deletion of the dead
  emission, not migration. A real evidence-upload pipeline (runner
  picks up evidence files → POSTs to glimmung with run context →
  glimmung stores under `runs/<project>/<run>/<kind>/<name>`) is a
  separately-scoped feature, not a follow-up — explicitly out of
  scope here.
- Run-scoped path evidence:
  `internal/server/inspection_api_test.go::TestCreateInspectionRunScopedWritesUnderRunPrefix`
  asserts run-bound bytes land under `runs/<project>/<run>/inspections/...`
  and the ledger row carries `scope='run'` + `run_id`.
  `TestCreateInspectionRunScopedRejectsMissingRun` and
  `TestCreateInspectionRunScopedRejectsCrossProject` pin the
  validation contract.
  `internal/server/inspection_sweep_test.go::TestSweepLeaseInspectionsLeavesRunScopedRows`
  pins the retention boundary: lease cleanup deletes lease-scoped rows
  and blobs while run-scoped rows persist.

## typed-step-error-attribution

- **Status:** active
- **Intent:** A non-verification producer step crash must surface its real
  reason, not a content-free "exited with code N". Before this, a producer step
  body's `throw`/host failure terminal-attributed as the generic step exit
  reason (`exit_nonzero`), so the operator-facing terminal observation lost the
  actual cause — the producer-step half of the no-generic-terminal contract that
  #846/#756 established for verifier/gate/dispatch failures.
- **Mechanism:** the run-harness SDK writes a typed `error{layer,code,message}`
  block (shape `internal/domain/steperr.Block`) to `GLIMMUNG_COMPLETION_FILE` on
  a step failure. The runner rides it on the `step_failed` event metadata
  (`error_layer`/`error_code`/`error_message`) and the `/completed` request; the
  completion API threads it onto the failing job completion; the store's
  `terminalJobFailureCause` promotes the typed message as
  `RunTerminalObservation.Reason` (with the layer folded into the message) when
  no verification verdict supplied a cause. A malformed block (no message) is
  dropped so a producer can never launder a hollow attribution. Completions
  without a block behave byte-for-byte as before, so the existing terminal
  attribution, metric, and alert invariants are unchanged.
- **Affected contracts:** Observability And Evidence (terminal-cause
  attribution, run report), Workflow Execution (step-body failure projection).
- **Evidence:** `TestTerminalObservationPromotesTypedStepError`,
  `TestTerminalObservationWithoutStepErrorIsUnchanged`,
  `TestMalformedStepErrorIsDropped`, `TestRunnerPromotesTypedStepErrorOnFailure`,
  `TestRunnerFailureWithoutBlockIsUnchanged`,
  `TestCompletionPayloadThreadsTypedStepError`,
  `TestCompletionPayloadDropsMalformedStepError`.

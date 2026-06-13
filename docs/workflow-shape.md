# Workflow shape

The opinionated structure every glimmung-managed workflow follows,
the data model that enforces it, and the conventions for naming /
identifying entities.

## The shape

Every workflow is a left-to-right pipeline of phases:

```
prepare       →  work        →  testing  →  cleanup
┌──────────┐     ┌─────┐
│ env      │     │ plan│
├──────────┤     ├─────┤
│ contract │     │ impl│
└──────────┘     └─────┘
```

- **Phases** flow horizontally. Each phase is a stage of the
  pipeline. The first phase is the only entry phase and declares
  `depends_on: []`. Every later phase declares exactly one
  `depends_on` entry, and that entry must be the immediately
  previous phase.
- **Jobs** stack vertically inside a phase. Multiple jobs in one
  phase always run in parallel — there is no job-level
  `depends_on` and the system does not support gitlab-style
  inter-job-within-a-phase dependencies. Pipeline composition
  happens at the phase boundary.
- **A phase is one job wide and any number of jobs deep.** "Wide"
  meaning horizontal — phase boundaries are the only place a
  pipeline advances as one left-to-right chain. "Deep" meaning vertical
  parallel jobs within a single phase.

This rule keeps pipeline design legible: anyone reading a
workflow definition can see the order of work by scanning
phases left-to-right, and see what runs in parallel by reading
jobs top-to-bottom in each column.

## Required phases

Glimmung-managed workflows must declare:

1. **prepare** — exactly one phase named `prepare` with `depends_on=[]`
   (the entry phase). Project owns what goes here; common shape is
   "build a container image and deploy it to a per-run validation
   namespace." The platform mandates no project stage names inside it —
   the retired issue-contract entry mandate is deleted; registration
   rejects nothing about a prepare phase's job ids or outputs beyond the
   generic rules.
2. **testing** — exactly one bounded verification phase with `verify=True`.
   The workflow row declares `constraints.verification.shape`, which selects
   the verification phase shape for that workflow. Supported profiles are
   `single_job`, `bounded_case_jobs`, and `dynamic_step_group`. Runtime
   evidence requirements decide what each verification run actually attempts,
   but the persisted constraint profile owns whether the phase is a legacy
   single verifier, a fixed set of bounded case jobs, or a sequential dynamic
   test-case block. The phase owns and produces one verification verdict.
3. **cleanup** — at least one phase with `purpose: teardown` and
   `run_on: always` or `run_on: failure`. Runs on terminal cleanup paths and
   tears down the validation environment.

Any number of `work` phases between prepare and testing — that's
where the actual implementation happens.

The mandatory-phase and linear-topology enforcement is active in the Go workflow
writer, sync path, and Postgres upsert path. Registrations whose entry phase is
not named `prepare`, that miss a `verify: true` testing phase, or that miss a
teardown cleanup phase are rejected before they can become the project runtime
contract. Registrations with multiple entry phases, fan-in/fan-out phase
dependencies, invalid cross-phase input refs, duplicate phase names, or
duplicate job IDs are rejected too.

Verification shape is intentionally data-owned. Glimmung code owns the primitive
validator implementations, but the workflow payload owns which primitive applies
through `constraints.verification.shape`. When that field is absent, registration
infers a profile from the verify phase and persists the inferred constraint on
the next write. This keeps existing `single_job` workflows editable while
allowing newer workflows to opt into `dynamic_step_group` without making every
project wait on a Glimmung deploy for the shape decision.

Evidence requirements are snapshotted onto the Run at dispatch time. Workflow
`default_requirements.required_evidence` and operator labels such as
`evidence:video` or `evidence:animation` are inputs to that snapshot, but the
stored Run requirements are the source of truth after dispatch. Later label
edits do not change an in-flight evidence contract.

Blank phase `kind` values default to `k8s_job`. Registered workflow phases must
use `k8s_job`; any other executor kind is rejected before dispatch.

## Job-level concurrency within a phase

In a phase with N jobs, all N dispatch simultaneously. No
dependencies between them. Each job is its own k8s Job; each
emits its own completion callback; the phase is "complete"
when all jobs have completed.

The native completion contract is enforced at
`POST /v1/run-callbacks/{callback_token}/native/completed`: the payload must
include `job_id`. Managed runner payloads include positive `cost_usd` and
`agent_usage` when the runner observes provider usage events such as Codex
`turn.completed.usage`; the runner prices those tokens from the Run's
snapshotted agent runtime profile. Missing pricing for an observed usage event
is a runner error, not a silent zero-cost completion. Glimmung records each job
completion independently, returns a `wait_jobs` response while sibling jobs are
still pending, and runs the phase decision path only on the transition where
the final registered job completes. This is the only native terminal callback.
Failed jobs report through the same endpoint with a non-`success`
`conclusion`; the retired `/native/failed` callback must not be reintroduced or
required by runner images.

Because jobs in a phase are strictly parallel, **a job can never
depend on the output of another job in the same phase**. If
verifier needs implementation's output, verifier goes in a
*later* phase, not as a sibling job in the work phase.

This rules out gitlab-style `needs:` graphs at the job level, by
design — pipeline shape is determined by phases, not by job DAGs.

## Managed agent steps

Managed native jobs can declare `type: agent` steps. The workflow owns where an
agent is needed and which logical slot it occupies; Glimmung owns selection of
the concrete provider/model at dispatch time.

```yaml
jobs:
  - id: implement
    managed: true
    checkout:
      ref: main
    working_directory: /workspace/ambience
    steps:
      - slug: implement
        type: agent
        agent:
          slot: implementation
          prompt_file: .glimmung/prompts/implement.md
```

Agent runtime policy resolves in this order:

1. Global config chooses the fleet default and profile catalog.
2. Project config (the durable `projects` row, written via `register_project`)
   may inherit or override the default and named slots.
3. Issue metadata may inherit or override the default and named slots.

Each decision is explicit: `mode: inherit` keeps the current value from the
previous layer, while `mode: override` names a profile. Dispatch snapshots the
resolved runtime onto the Run before any native work starts. The runner consumes
only that snapshot through `GLIMMUNG_AGENT_RUNTIME_JSON`; changing global,
project, or issue defaults later does not mutate an in-flight or historical
run. This keeps agent selection containerized: a workflow inserts an agent step
without forking the workflow per model/provider.

Every runtime profile includes an explicit pricing catalog snapshot. Cost
telemetry is derived from token usage observed during native execution and that
snapshotted pricing, then persisted on job completions, attempts, and run
reports. Provider-emitted `total_cost_usd` result lines are not a live
contract.

## Conditional Phases And Jobs (`when` / `vars`)

Phases and jobs may declare a `when` condition, evaluated **server-side at
phase dispatch, before any Kubernetes Job exists**. A false condition spends
zero compute (GitHub Actions `if:` / GitLab `rules:` parity): the platform
synthesizes durable skipped records instead of launching, and the dashboard
renders the declared-but-skipped leg. The workflow shape stays total — the
skip *is* the documentation of the toggle.

```yaml
vars:
  feature_type: effect
phases:
  - name: llm-work
    jobs:
      - id: llm-test-plan
        when: "${{ vars.feature_type }} != 'effect'"   # skipped for effect runs
      - id: llm-implement                               # unconditional
  - name: cleanup_early
    purpose: teardown
    when: "${{ run.preserve_test_env }} == 'false'"
```

The grammar is closed — a routing condition, not a scripting surface:
`true` | `false` | `<term> ==|!= <term>`, where a term is `${{ vars.<key> }}`,
`${{ inputs.<key> }}` (a declared dispatch input), `${{ run.preserve_test_env }}`,
or a literal. Comparison is exact string equality after resolution.
Registration validates every ref against the declared `vars` map and
`dispatch_inputs`, and rejects:

- `when` on a verification or review-gate phase or its jobs — gates run;
- `when` on an entry phase (no `depends_on`) — fresh cycles and recycle
  landings need a defined start;
- a phase whose every job is conditional — at least one job stays
  unconditional so a launched phase always launches something (whole-phase
  conditions belong on the phase-level `when`).

Semantics:

- **Phase-level skip**: a false phase condition synthesizes a `skipped`
  attempt with the resolved condition as the reason, and the workflow
  advances past it like a success. Every job whose condition is false when
  its siblings all skip collapses to the same phase-level skip.
- **Job-level skip**: a false job condition writes a synthesized `skipped`
  job completion (pre-satisfying the expected-job set, so the phase
  completes on launched siblings alone) and skipped job/step execution
  records. No Job object, secret, or pod is ever created.
- **Skipped outputs are empty**: downstream
  `${{ phases.X.outputs.Y }}` refs against a phase with skipped legs resolve
  to the empty string when the output was never published — consumers handle
  absence, exactly like skipped-job outputs in GitHub Actions. Phases with
  no skips keep the strict fail-closed behavior.
- **Verdict-neutral**: a skipped job neither degrades a succeeding phase nor
  masks a failing sibling.
- `vars` is registration-owned workflow identity (part of the content-hashed
  schema), not a per-dispatch knob; per-dispatch routing uses
  `${{ inputs.* }}`.

## Verification Case Shapes

The verification shape is selected by `constraints.verification.shape`.
`single_job` preserves legacy workflows such as Ambience's monolithic
`llm-verify` runner. `bounded_case_jobs` declares fixed case slots and keeps
each slot independently bounded. `dynamic_step_group` declares a sequential
runtime-defined block inside one job. Jobs in one phase are parallel, so choose
`bounded_case_jobs` only when the workflow owns enough isolated runtime
capacity for sibling jobs.

```yaml
constraints:
  verification:
    shape: dynamic_step_group
- name: testing
  kind: k8s_job
  purpose: verification
  verify: true
  outputs: [verification]
  jobs:
    - id: verify
      timeout_seconds: 1800
      managed: true
      steps:
        - slug: author-test-plan
          type: agent
        - slug: gather-evidence
          type: agent
          group: test-cases
          group_title: Test cases generated at runtime
          dynamic_group:
            max_items: 10
            item_label: test case
        - slug: judge-evidence
          type: agent
          group: test-cases
          group_title: Test cases generated at runtime
          dynamic_group:
            max_items: 10
            item_label: test case
        - slug: aggregate-verification
          type: run
```

At runtime the verification runner reads the test/evidence plan and expands the
dynamic block into zero to ten sequential cases. The step immediately before the
dynamic block usually authors the plan and must publish either:

- `test_cases_json` / `<group>_json` / `<group>_cases_json`: a JSON array, or an
  object with a `cases` array. Each item can include `label`, `title`, or `name`.
- `test_cases_count` / `<group>_count` / `<group>_cases_count`: an integer when
  the cases are intentionally positional and do not need per-case JSON.

`<group>` is the dynamic step group normalized to output-key form; for
`group: test-cases`, `test_cases_json` is the canonical key. The runner fails
closed if no plan key is present or the count exceeds `dynamic_group.max_items`.
Each expanded case runs the dynamic block's template steps sequentially with
concrete slugs such as `gather-evidence-case-01` and `judge-evidence-case-01`.
The concrete steps receive:

- `GLIMMUNG_DYNAMIC_CASE_INDEX`
- `GLIMMUNG_DYNAMIC_CASE_COUNT`
- `GLIMMUNG_DYNAMIC_CASE_LABEL`
- `GLIMMUNG_DYNAMIC_CASE_JSON`
- `GLIMMUNG_DYNAMIC_TEMPLATE_STEP`

Each expanded case does one bounded task:

- no item at that index: write a no-op completion and exit success
- video/screenshot/browser item: capture and judge that one artifact
- command item: run that one command and report the result
- agentic external item: use only the tools needed for that one item

A case must not debug unrelated tooling, recapture other cases, or consume a
suite-level retry budget. A Playwright timeout, trigger failure, MCP/tool
failure, missing artifact, or command timeout fails that case and should stop
the sequential verifier promptly. The verification job writes the completion
payload with `verification.status`, `reasons`, and evidence refs/artifacts.
Glimmung stores the phase output `verification` for reports, terminal
observations, and any later phase that needs to read the verdict.

On a non-pass verdict the verification payload should also carry a
structured `failure` block — the why, not just the enum:

```json
"failure": {
  "expected": "the claim being verified, quoted",
  "observed": "the literal contradicting observation",
  "where": "event log | decoded frame | snapshot | http response",
  "suspected_cause": "code_bug | test_expectation_mismatch | environment_config | harness_flake",
  "cause_detail": "1-3 sentences of causal analysis"
}
```

Glimmung persists the block on the attempt and per-job completions, renders
expected/observed/suspected-cause on the dashboard attempt card, folds it
into abort explanations, and — when a recycle policy retries the run — passes
the deciding cycle's verification (status, reasons, failure) to every pod of
the new cycle as `GLIMMUNG_PRIOR_VERIFICATION_JSON`
(`{"phase": "...", "verification": {"status", "reasons", "failure", ...}}`),
so retry attempts plan against the previous failure instead of rediscovering
it. For dynamic verification groups, the runner promotes the first failing
case's `failure` block to the aggregate verdict and keeps each case's block
on `verification.cases[]`.

For `bounded_case_jobs`, every case job is named `verify-case-01` through
`verify-case-10` and sets `timeout_seconds` no higher than 600 seconds. Each
case runner selects the plan item at its 1-based index; unused slots write no-op
case results. When the final registered case job completes, Glimmung aggregates
case completions into the phase verdict and synthesizes the phase output
`verification`.

The test-plan LLM owns what cases should prove. Glimmung owns the selected
constraint profile, maximum case count, timeout bounds, aggregation, and
visibility. If a plan needs more than ten required items, it is too broad for
one run and must fail or narrow the plan.

## Verification Boundary

The valid shape for emitting a verdict at the testing boundary is a
verification phase whose cases own their evidence verdicts directly:

```yaml
- name: testing
  kind: k8s_job
  purpose: verification
  verify: true
  outputs: [verification]
  jobs:
    - id: verify
      timeout_seconds: 1800
      steps:
        - slug: gather-evidence
          group: test-cases
          dynamic_group: {max_items: 10, item_label: test case}
        - slug: judge-evidence
          group: test-cases
          dynamic_group: {max_items: 10, item_label: test case}
```

If a dynamic case emits `verification.status=fail` or `error`, Glimmung fails
the owning case and the verification job; downstream phases depend on
verification success rather than on a separate evidence-verification gate. The
old `evidence_verification_gate` primitive remains readable for historical
runs, but new workflow registrations reject it.

## PR touchpoint primitive

Every Glimmung workflow ends in a human-reviewed PR — there is no opt-out.
Workflows must declare exactly one native job with `primitive: pr_touchpoint`,
and that job must live in a `purpose: review_touchpoint`, `run_on: success`
phase. Review touchpoints are not teardown; when verification aborts the run,
Glimmung runs only teardown phases and then terminates the run as aborted.

```yaml
phases:
  - name: touchpoint
    kind: k8s_job
    run_on: success
    purpose: review_touchpoint
    depends_on: [testing]
    jobs:
      - id: pr-touchpoint
        primitive: pr_touchpoint
```

The job is Glimmung-supplied. Registration canonicalizes the declared job into
the managed native runner step that calls Glimmung's PR/touchpoint finalizer.
The workflow owns the placement and job id; Glimmung owns the implementation.
The historical PR opt-out toggle was deleted: there was no documented product
scenario for PR-less workflows and per migration-policy unused toggles are
deletion targets, not design options. The `pr.recycle_policy` setting remains
and configures the reject-signal recycle target.

The same finalizer is also exposed as an admin repair/control endpoint:
`POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/finalize`.
For recycled runs, use the cycle-addressable form that matches the UI URL:
`POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/touchpoint/finalize`.
It is idempotent and uses the durable Run state as source of truth: it creates
or reuses the GitHub PR, records `run.pr_number`, and ensures the Touchpoint
linked to the Issue and Run. During that same call, Glimmung promotes review
facts such as `validation_url` into canonical Run fields, normalizes run
artifact evidence into Touchpoint evidence, and validates required typed
evidence artifacts before the Touchpoint is ready. For browser-visible changes,
WebM video is the baseline evidence kind; screenshots are supplemental
final-state or thumbnail evidence. GitHub PR bodies stay a syndicated
pointer into Glimmung; video, screenshots, and other review evidence belong on
the Glimmung Touchpoint. Operators should use this endpoint when a Run already
passed verification but an older or interrupted workflow did not materialize
the review surface.

## Human review gate (touchpoint_gate)

Workflows that want a reviewer to confirm a touchpoint before merging declare a
`touchpoint_gate` phase between testing and cleanup. The gate has exactly one
managed job with `primitive: pr_merge`; Glimmung canonicalizes that job into
the runner step that performs the idempotent merge.

```yaml
phases:
  - name: prepare
    kind: k8s_job
    jobs:
      - id: env-prep

  - name: work
    kind: k8s_job
    depends_on: [prepare]
    jobs:
      - id: impl

  - name: testing
    kind: k8s_job
    verify: true
    depends_on: [work]
    jobs:
      - id: testing

  - name: cleanup_early
    kind: k8s_job
    run_on: always
    purpose: teardown
    depends_on: [testing]
    jobs:
      - id: env-destroy

  - name: touchpoint
    kind: k8s_job
    run_on: success
    purpose: review_touchpoint
    depends_on: [cleanup_early]
    jobs:
      - id: pr-touchpoint
        primitive: pr_touchpoint

  - name: touchpoint_gate
    kind: k8s_job
    run_on: success
    purpose: review_gate
    depends_on: [touchpoint]
    jobs:
      - id: pr-merge
        primitive: pr_merge

  - name: cleanup_final
    kind: k8s_job
    run_on: always
    purpose: teardown
    depends_on: [touchpoint_gate]
    jobs:
      - id: env-destroy-final
```

Runtime behavior:

- When the workflow advances into the gate, Glimmung appends the durable
  `touchpoint_gate` attempt, sets the Run state to `review_required`, and does
  NOT launch the gate's job. `review_required` is an in-progress sub-state —
  locks stay held and the slot may still be alive if the issue had
  `preserve_test_env=true`. Projections treat it as active.
- The signal bus carries the reviewer's decision:
  - `payload.kind: "approve"` releases the existing gate attempt. Glimmung
    flips the Run back to `in_progress` and launches the managed `pr_merge`
    job through the normal `k8s_job` path. The job calls back through the normal
    completion callback, the workflow advances to `cleanup_final`, and the Run
    terminates `passed`. The Issue is closed on that terminal transition.
  - `payload.kind: "reject"` follows today's PR-feedback recycle path: a new
    cycle is created landing at the workflow's configured `pr.recycle_policy`
    target.
- The `pr_merge` primitive is idempotent. A second approve when the PR is
  already merged returns `status: already_merged` and is a benign no-op.
- If any primary phase aborts before the review surface, Glimmung dispatches
  only remaining teardown phases. It does not run `touchpoint` or
  `touchpoint_gate`, so an aborted run cannot park in `review_required`.

The cleanup-execution split:

- `cleanup_early` runs as a normal teardown phase, conditioned with
  `when: "${{ run.preserve_test_env }} == 'false'"`. If the issue has
  `preserve_test_env=true`, the condition evaluates false at dispatch and the
  server synthesizes a `"skipped"` attempt — no Kubernetes Job is created;
  the phase still advances and the run history shows the deliberate skip
  attributed to the resolved condition. With `preserve_test_env=false` (the
  default) the env is torn down here, before the reviewer sees the
  touchpoint. The retired `skip_when_preserve_test_env` field is rejected at
  registration with a pointer at this `when` form.
- `cleanup_final` is the catch-all teardown phase after merge or abort. It
  always actually runs on those paths. When `cleanup_early` already destroyed
  the validation environment, `cleanup_final` is a no-op success that still
  records the cleanup decision in the run history.

Teardown phases are **verdict-neutral**. A `purpose: teardown` phase runs as
post-verdict cleanup, so its job outcome never sets the run verdict: a failed
teardown — e.g. a transient pod-start `BackoffLimitExceeded` — does not abort
an otherwise-passing run and never overrides or masks the primary phase's
failure. The decision engine advances the cleanup chain regardless of a
teardown job's conclusion, and terminal-cause attribution skips teardown
attempts, so the run terminates reflecting its primary verdict. The failed
teardown stays visible on its own job/step state; a bounded `backoffLimit`
lets the idempotent env-destroy absorb a transient blip, and the env-prep slot
reap (ambience#224) covers any resources a still-failed teardown did not
remove.

The `pr_merge` primitive is also exposed as an admin repair/control endpoint:
`POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/merge`
(and the cycle-addressable form). Idempotent; uses the durable Run state as
source of truth. Useful for triggering an approve from the API or repairing
a stuck gate.

## Naming convention

The reference names for the four mandatory phases are:

- **prepare** — entry phase, environment setup and shared pre-work contracts
- **work** — implementation labor (1+ phases between prepare and
  testing)
- **testing** — the verdict-rendering phase
- **cleanup** — teardown

The MCP `scaffold_workflow` tool (TODO) emits a starter template
with these names pre-filled.

## Remote-host execution

Phase shell scripts that need to drive a host outside the Glimmung cluster
(e.g. a desktop-game mod whose verify loop requires the warm game install)
shell out to the host over SSH, with credentials minted per-run by two
callback-token-gated endpoints on Glimmung. The phase shape stays
`k8s_job`; only what the script does inside the pod changes. See
[`remote-host-execution.md`](remote-host-execution.md) for the endpoint
contracts, threat model, and orchestrator-side flow.

## Dispatch inputs

Workflows can declare dispatch-time inputs that the dispatcher fills in before
the run is created. Every `${{ inputs.X }}` reference inside a native
`checkout.ref`, `extra_checkouts[].ref`, or phase `workflow_ref` must be backed
by a declared input — registration is rejected at the `ValidateWorkflowRegister`
boundary otherwise. The contract is symmetric: a dispatch payload that sends an
input the workflow does not declare is also rejected, so caller-supplied values
cannot silently flow into `Run.RunInputs`.

```yaml
dispatch_inputs:
  - name: git_ref
    description: branch or sha for native checkouts
    required: true
    default: main
```

Rules:

- `name` follows the run-input identifier pattern (letters, numbers,
  underscores, hyphens; starts with a letter or underscore). Duplicate names
  are rejected.
- `required: true` makes the dispatcher's omission a 422. There is no
  server-side guess. A required input may set a `default` so a no-input
  dispatch succeeds against the declared default; a no-input dispatch against
  a required input with no default fails.
- `required: false` requires a non-empty `default`. A non-required input
  without a default has nothing to substitute and is rejected at register
  time.
- The dashboard renders one form row per declared input, pre-filled with the
  declared default. The MCP `dispatch_run` tool keeps its existing optional
  `inputs` parameter; per-workflow declarations are inspected by reading the
  workflow registration.

This makes the dispatch contract a durable, inspectable part of the workflow
registration rather than an implicit promise scattered across templated
checkout refs. The same `ValidateWorkflowRegister` walk catches both register
and dispatch paths, so the rule cannot drift between them.

## Runtime source of truth

Postgres workflow registrations are the runtime source of truth. The
workflow upstream endpoints have been retired; dispatch reads the registered
workflow document, not a consumer repository file.
The native runner direction is documented in
[`project-native-runner-architecture.md`](project-native-runner-architecture.md):
Glimmung owns the runner contract and project workflows use inline step
commands rather than repo-owned callback plumbing.

Workflow registrations are logical pointers. Updating a registration creates a
new immutable workflow schema and moves the logical pointer forward. Existing
runs and cycles keep referencing the schema they were created with. Historical
schemas are retained; this rollout does not garbage-collect them. Deleting or
deactivating a logical workflow must not delete schemas referenced by run
history.

Each cycle stores a durable execution ledger for the schema snapshot it was
created with: phase records contain job records, and job records contain step
records. The graph UI projects from this ledger first, then uses raw native
events as live detail. This keeps state names and colors stable even when a
native job has not emitted logs yet.

## Operator control pins and the control ledger

Control values — recycle policies and the cost budget — are operator-owned
dials. Registration replaces the workflow document wholesale, which
historically meant any session re-registering for an unrelated reason could
silently "normalize" a control back to a value the operator had deliberately
changed (observed live: `llm-verify` `max_attempts` flipped 1→3 by a
re-registration). Two mechanisms close this:

**Control pins.** `workflows.control_pins` is an operator-owned column,
parallel to `projects.status`: registration never writes it. A pin freezes
the current value at one of a closed set of targets:

```text
budget
pr.recycle_policy
phases.<phase>.recycle_policy
```

- `PUT /v1/workflows/{project}/{name}/control-pins/{target}` (admin) pins the
  CURRENT value, with an optional `{"reason": "..."}` body. Pins freeze what
  is — value changes go through the ordinary patch surface first.
- `DELETE /v1/workflows/{project}/{name}/control-pins/{target}` (admin)
  releases it.
- **Registration enforces pins**: a pinned target's incoming value is
  discarded and the pinned value is written into the authored payload before
  the schema hash is computed, so the stored document and every run snapshot
  reflect operator intent. The override is loud — reported on the register
  response (`pins_enforced`, `control_changes` with `action: pin_enforced`)
  and recorded in the ledger. A pin whose target phase no longer exists in
  the incoming registration rejects the registration (unpin first); pins
  never silently evaporate.
- **Patches against a pinned target are rejected** with the pinner, reason,
  and the unpin remediation in the error. A patch states control intent
  explicitly, so silently overriding it would make the call "appear to work".

**The control ledger.** Every control-plane write — `register`, `patch`,
`pin`, `unpin`, `delete` — appends one row to `workflow_control_events`:
actor (the authenticated principal, refined by the `X-Glimmung-Actor` header
trusted service callers forward, e.g. `svc:mcp-glimmung via tank-session:815`),
resulting `schema_ref`, and a detail document carrying the control-field diff
(`budget`, `pr.recycle_policy`, every phase recycle policy) for registrations
or the target/reason for pin events. The ledger exists because
`workflow_schemas` rows are content-addressed: re-registering a
previously-seen shape (a revert) reuses the existing schema row and would
otherwise leave no trace of who moved the pointer.
`GET /v1/workflows/{project}/{name}/control-events` is a public read, and the
workflow detail page renders the recent tail plus per-lane pin state.

**Explicit verification recycle policy.** A `verify=true` phase must declare
`recycle_policy` at registration; a missing policy is rejected naming the
phase. Silence is not a policy: `max_attempts: 1` is the explicit way to run
the verification gate with recycling off, and there is no platform-default
attempt count for a registration to fall back to.

## Path-typed identity

Entities are addressed by URL-shaped paths that match the HTTP
API surface:

```
projects/<project>
projects/<project>/workflows/<workflow>
projects/<project>/workflow-schemas/<schema_ref>
projects/<project>/workflows/<workflow>/phases/<phase>
projects/<project>/issues/<issue_number>/runs/<run_number>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>/jobs/<job_id>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>/jobs/<job_id>/steps/<slug>
```

Logs, MCP tool outputs, error messages, and notification surfaces
all emit these. Inside a known scope (e.g. inside one run's logs),
the trailing path can be elided: `attempts/0/jobs/agent` is enough when the
run is implicit.

Runs are user/reviewer intent records. Cycles are the durable execution
ledger records. The issue history keeps a flat, monotonically increasing
cycle number (`1`, `2`, `3`, ...), but each cycle also belongs to a run and
has a run-local cycle ordinal. The compact display form is
`<run>.<run_cycle>` such as `1.1`, `1.2`, `2.1`.

Recycle policy creates a new cycle under the same run. Reviewer feedback,
touchpoint changes, and a user pressing Run after terminal state create a
new run with its first cycle. Manual mid-run restart is not part of the
product HTTP surface; emergency surgery belongs outside the normal run
workflow model.

Within one run, only one cycle can be active at a time. Within one issue, only
one run can be active at a time. A cycle stores the workflow schema ref it was
created with; phase/job/step projection and retry/cleanup decisions use that
schema ref, not whatever logical workflow registration is current later.

Use **attempt** as an execution-scoped display counter for a concrete phase
launch. It is not a first-class product entity. Recycle policy is represented
by a new cycle, not by appending another product-level attempt to the prior
cycle.

Manual run dispatch is admission-gated: if no test slot is available, the API
returns `no_capacity` and does not create a run or cycle. Queueing remains an
issue-level product workflow; queued cycles that already exist are admitted by
the run-queue reconciler when capacity appears.

Synthetic dispatch is deliberately stricter and less helpful than normal
dispatch. It is an operator escape hatch for creating a new Run at an explicit
`start_at_phase` with caller-supplied completed outputs for earlier phases. It
must not scrape prior runs, decide which outputs matter, or allocate a test
slot. Supplied phases render as `supplied`, not `passed`; the caller owns the
truthfulness and completeness of the provided outputs.

Never store paths as canonical identifiers — compute at render
time from the entity's slug + parent context. This avoids
renumbering churn when phases are added/removed and naturally
handles DAGs (parent path encodes type structurally; depth
doesn't matter for naming).

Helpers live in `internal/domain/paths`: `RunPath`, `PhasePath`, `JobPath`,
and `StepPath`.

## Why this shape

The constraints are deliberate:

- **Strict left-to-right** removes the gitlab-style "wonky
  semantics" where `needs:` DAGs at the job level make pipelines
  hard to read. Jobs can fan out inside a phase; phases themselves
  stay a single chain.
- **Mandatory testing** means glimmung-managed workflows are
  self-validating; an agent's PR doesn't ship without a verdict
  step, even if the verdict is just `npm build`.
- **Mandatory cleanup** means orphaned environments don't
  accumulate. Glimmung enforces what every project would have
  built awkwardly on its own.
- **Path-typed identity** makes references uniform across logs,
  UI URLs, MCP, slack — one canonical form, parent-encoded by
  structure, no decoration.

These are the four levers from `CLAUDE.md`: precise lanes,
heavy automation around the agent, guard rails, and token-spend
protection. Workflow shape is a guard rail.

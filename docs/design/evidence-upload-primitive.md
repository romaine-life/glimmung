# Evidence Upload Primitive

Status: Stages 1–2 implemented (the glimmung-owned step + registration
invariant). Stages 3–4 (re-registering live workflows, removing project bash)
are tracked separately.

This document records the product boundary for evidence/screenshot upload to
the shared `romaineglimmungartifacts` blob store. It follows the repository
policies in [`../migration-policy.md`](../migration-policy.md),
[`../project-native-runner-architecture.md`](../project-native-runner-architecture.md),
and the feature contracts `workflow-execution` and `observability-and-evidence`.

## Problem

Evidence upload to the shared artifacts blob store is the last cross-cutting,
infra-authenticated operation still hand-rolled as project bash. Each project's
`scripts/glimmung-native/verify.sh` runs its own `upload_screenshots` doing
`az storage blob upload-batch --auth-mode login`, which needs an explicit
`az login --service-principal --federated-token "$(cat "$AZURE_FEDERATED_TOKEN_FILE")"`
first — the `az` CLI does not consume the projected workload-identity token the
way the Azure SDK credential chain does. spirelens's migrated script dropped
that login and runs failed.

Managed cross-cutting ops in Glimmung are owned primitives that registration
REQUIRES: see `pr_touchpoint`/`pr_merge` in
[`internal/server/evidence_gate.go`](../../internal/server/evidence_gate.go)
and `ValidateWorkflowRegister` in
[`internal/server/workflow_write_api.go`](../../internal/server/workflow_write_api.go)
(it rejects a `review_gate` phase without exactly one `pr_merge` job). Evidence
upload must become a managed primitive too, so the auth + storage contract lives
in Glimmung instead of being re-implemented (and silently broken) per project.

## Decision: a managed STEP on the verification job — NOT a job/phase

`pr_touchpoint`/`pr_merge` are managed **jobs** in their own phases doing
NETWORK work (curl a Glimmung URL). Evidence upload is different, and the
difference is dispositive:

- **Evidence upload must read the verification pod's LOCAL filesystem** — the
  screenshots/videos/observations the project just collected on disk during the
  verification run.
- **Each native Kubernetes Job is its own pod with ephemeral, pod-local
  storage. There is NO shared cross-job / cross-phase volume.** A separate
  upload job, or a dedicated upload phase, runs in a *different* pod and
  therefore CANNOT see the evidence the verification job produced.

Therefore the primitive is a Glimmung-owned **STEP appended to the verification
job itself** (the same pod as the evidence), auto-injected during
canonicalization. It is not a job and not a phase. This is the corrected
framing: an earlier draft mirrored `pr_merge` as a job/phase; that shape cannot
work because of the pod-locality constraint above.

## Mechanics

### Canonicalization auto-append (Stage 2)

`CanonicalNativePhase` (in `internal/server/evidence_gate.go`) appends a managed
`evidence_upload` step to every job of a verification-purpose phase (`verify:
true`, or `purpose: verification`), after the project's own evidence-producing
steps, so the upload runs last in the same pod once the evidence exists.

- The step is identified by the stable managed slug `upload-evidence`, so
  re-canonicalization is idempotent (the read path runs `CanonicalWorkflow` on
  every workflow load; the append guards on the existing slug and never
  double-appends).
- A verification phase is the phase-local signal. Every Glimmung verification
  phase exists to produce and judge evidence — that is its defining purpose — so
  keying on the verification purpose is the conservative choice. Statically
  scanning each job's steps for an evidence emitter would miss the real shapes
  (spirelens/ambience verification jobs emit screenshots at runtime, and bounded
  case jobs declare no steps at registration), silently dropping the managed
  upload exactly where it is needed. Non-verification phases are never touched.

### Rendered step → runner subcommand (Stage 1)

The managed step's command invokes the native runner's own `upload-evidence`
subcommand (exactly as the managed `pr_merge`/`pr_touchpoint` steps render bash
that curls the runner/Glimmung). The pod ENTRYPOINT is
`/app/glimmung-native-runner`; the step runs:

```bash
exec "${GLIMMUNG_NATIVE_RUNNER_BIN:-/app/glimmung-native-runner}" upload-evidence
```

`main()` in `cmd/glimmung-native-runner` dispatches on `os.Args[1]`: with no
args it is the managed-job entrypoint and runs the job/step loop; with
`upload-evidence` it runs the primitive in the SAME pod as the evidence.

### Upload behavior

`cmd/glimmung-native-runner/upload_evidence.go`:

- **Auth: Azure SDK credential chain only.** `azidentity.NewDefaultAzureCredential`
  consumes the projected workload-identity federated token
  (`AZURE_FEDERATED_TOKEN_FILE` / `AZURE_CLIENT_ID` / `AZURE_TENANT_ID`). No `az`
  CLI, no `az login`. The verification pod is already labeled
  `azure.workload.identity/use: "true"` (set by the launcher), so the federated
  token file is projected.
- **Source:** every regular file under `GLIMMUNG_EVIDENCE_DIR`. An empty or
  absent directory is a success no-op (logs "no evidence to upload"). The
  launcher injects `GLIMMUNG_EVIDENCE_DIR` only into verification-phase pods, at
  a fixed path under the per-run working dir
  (`/tmp/glimmung-<run-ref>/evidence`, consistent with the project default
  `GLIMMUNG_WORKING_DIR`).
- **Destination:** the `romaineglimmungartifacts` account / evidence container,
  taken from `AGENT_SCREENSHOT_STORAGE_ACCOUNT` / `AGENT_SCREENSHOT_CONTAINER` —
  the SAME env the current upload bash consumes (injected via the project's job
  env), not hardcoded.
- **Layout:** each file lands at `<project>/<run-ref>/<relpath>` (relpath is the
  file's path relative to the evidence dir, POSIX-separated), uploaded with
  overwrite=true (idempotent — `UploadBuffer` overwrites by default, matching the
  bash `--overwrite true`).
- **Surfacing refs:** when the parent runner set `GLIMMUNG_COMPLETION_FILE`, the
  subcommand writes the uploaded artifact refs (`blob://<container>/<key>`) plus
  a screenshots-markdown block into that file. The parent runner's
  `collectCompletionMetadata` folds them into the job's `/completed` callback, so
  the refs surface in the run report's `evidence_refs` / `screenshots_markdown`
  channels — the same completion-metadata path agent/verification steps use.
- The blob client is abstracted behind a tiny interface so the walk + prefix +
  empty-dir logic is unit-tested without real Azure.

### Registration invariant (Stage 2)

`ValidateWorkflowRegister` asserts that every job of an evidence-producing
verification phase carries the managed `evidence_upload` step, mirroring the
`pr_merge`/`pr_touchpoint` validation pattern. Because canonicalization
auto-appends the step BEFORE validation runs (and every production validate path
canonicalizes — the register path via `normalizeWorkflowRegister`, the
dispatch/queue re-validation paths via `CanonicalWorkflow`), conforming
workflows pass. The assertion locks the invariant so a hand-built workflow that
strips the managed step is rejected.

## Rollout safety

Because canonicalization injects the step before validation, existing
evidence-producing workflows auto-conform when later re-registered (Stage 3).
The Stage 1–2 change is additive: it does not re-register live workflows or
remove project bash (Stage 4). During the Stage 3→4 window both the managed step
and the project bash may upload; that is harmless because the upload is an
idempotent overwrite to the same run-scoped prefix.

## Non-goals

- Do not make evidence upload a job or a phase (pod-locality forbids it).
- Do not use the `az` CLI or `az login` for auth.
- Do not hardcode the storage account/container; read the project-supplied env.
- Do not remove project upload bash or re-register live workflows in this slice.

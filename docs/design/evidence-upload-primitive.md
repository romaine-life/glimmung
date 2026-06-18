# Design: `evidence_upload` managed primitive

Status: proposed. Plan-first per [docs/quality-timeframes.md](../quality-timeframes.md);
implemented in coherent stages below.

## Problem

Evidence/screenshot upload to the shared `romaineglimmungartifacts` blob store
is the **one cross-cutting, infra-authenticated operation still hand-rolled as
project bash**. Each project's `scripts/glimmung-native/verify.sh` runs its own
`upload_screenshots`:

```bash
az storage blob upload-batch --account-name "$AGENT_SCREENSHOT_STORAGE_ACCOUNT" \
  --destination "$AGENT_SCREENSHOT_CONTAINER" --destination-path "$prefix" \
  --source "$shots" --auth-mode login --overwrite true
```

`--auth-mode login` requires the `az` CLI to be authenticated. The runner pod is
labeled `azure.workload.identity/use: "true"` (`native_launcher.go`), so the
federated token is projected — but the **`az` CLI does not consume it**; only an
explicit `az login --service-principal --federated-token …` does (ambience's
`native_azure_login`). spirelens's migrated `verify.sh` dropped that login, so
the first run ever to reach the step (`spirelens#151/runs/21.1`) failed with
`ERROR: Please run 'az login' to setup account.` after a fully-passing
verification.

The defect class: **a load-bearing precondition (authenticated upload) living in
project code that nothing requires to be correct.** It is droppable per project
and silently degrades. This is the same shape as the synthetic-dispatch input
gap (PR #770) — an invariant not enforced at a chokepoint.

## Principle

Every other cross-cutting managed operation is a **glimmung-owned managed
primitive that registration requires**: `pr_touchpoint` and `pr_merge`
(`evidence_gate.go`) are implemented by glimmung, and `ValidateWorkflowRegister`
rejects a `review_gate` phase that does not declare exactly one `pr_merge` job
(`workflow_write_api.go`). Evidence upload must become one too. Projects should
not hand-roll managed-tier, credentialed operations.

## Design

### 1. The primitive

- New job primitive `evidence_upload` (constant in `evidence_gate.go` alongside
  `JobPrimitivePRTouchpoint` / `JobPrimitivePRMerge`).
- **Glimmung-owned implementation in `glimmung-native-runner`.** It uploads every
  file under the run's evidence directory to `romaineglimmungartifacts` under a
  run-scoped prefix (`<project>/<run-ref>/…`), then emits the artifact refs into
  the phase output the report/UI already consume.
- **Auth via the Azure SDK credential chain, not the CLI.**
  `azidentity.NewDefaultAzureCredential` consumes the projected
  `AZURE_FEDERATED_TOKEN_FILE` / `AZURE_CLIENT_ID` / `AZURE_TENANT_ID` (workload
  identity) directly; upload via `azblob`. **No `az` binary, no `az login`** — the
  entire failure class is deleted, not guarded. The identity is the native-runner
  UAMI that already holds `Storage Blob Data Contributor` on the store (proven —
  ambience uploads with it today), so no new RBAC.
- Idempotent (`overwrite=true`), and a no-op (success) when the evidence dir is
  empty.

### 2. Evidence directory convention

- Glimmung injects `GLIMMUNG_EVIDENCE_DIR` into verification phase pods (a fixed
  path under the per-run working dir).
- Projects' verification writes evidence there (screenshots + `verification.json`).
  Collecting evidence off the project's own host (e.g. STS2 over SSH) stays
  project logic; **producing into `GLIMMUNG_EVIDENCE_DIR` is the only contract.**
- The managed `evidence_upload` job uploads the directory; the project never
  references the storage account, container, or any credential.

### 3. Registration enforcement (the invariant)

- `ValidateWorkflowRegister`: a `purpose: verification` phase that declares
  screenshot/`required_evidence` artifacts must declare exactly one
  `evidence_upload` job (auto-injected by registration is acceptable, mirroring
  how managed jobs are normalized). A conforming-evidence workflow that omits it
  is **rejected at registration** — it cannot exist in the violating state, the
  same way a `review_gate` without `pr_merge` is rejected.
- A registration test locks the rejection (the analog of the `pr_merge` /
  `pr_touchpoint` validation tests).

### 4. Retire the project bash (migration policy: no compatibility)

Per [migration-policy.md](../migration-policy.md), the old surface is deleted,
not preserved behind a flag:

- Delete `upload_screenshots` and the `az storage blob upload-batch` call from
  `spirelens/scripts/glimmung-native/verify.sh` **and**
  `ambience/scripts/glimmung-native/verify.sh`.
- Delete `native_azure_login` from `ambience/scripts/glimmung-native/lib.sh`
  (now unreferenced).
- Both projects' `glimmung-native-quality` contract checks gain an assertion that
  **no `az` / `--auth-mode login` appears in the native scripts** (so the
  hand-rolled path cannot return), and the workflow rows re-register to declare
  the managed `evidence_upload` job.

## Stages (each coherent on its own)

1. **Primitive + runner upload + convention.** Add `evidence_upload` kind, the
   `glimmung-native-runner` implementation (SDK-credential `azblob` upload from
   `GLIMMUNG_EVIDENCE_DIR`), inject the env, runner unit test. No workflow yet
   requires it — additive.
2. **Registration requirement + tests.** `ValidateWorkflowRegister` requires the
   job on evidence-producing verification phases; reject test. Update the
   feature contract (`workflow-execution`, `observability-and-evidence`).
3. **Re-register spirelens + ambience** `default` workflows to declare the
   managed job; verify a run uploads via the primitive.
4. **Delete project bash** (`upload_screenshots`, `native_azure_login`) + add the
   "no raw `az` in native scripts" contract lint in each project's
   `glimmung-native-quality.yml`.

Stage 1 is shippable alone; stages 2–4 land the enforcement and the cleanup. The
spirelens `verify.sh` `az login` stopgap (if used to unblock #151 sooner) is
explicitly throwaway scaffolding removed in stage 4.

## Feature Contracts

- **Workflow Execution** — adds a third managed primitive; registration requires
  it on evidence-producing verification phases (same enforcement shape as
  `pr_merge`).
- **Observability And Evidence** — evidence upload becomes a guaranteed,
  glimmung-owned step; artifact refs are produced by the platform, not by
  best-effort project bash.

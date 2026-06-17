# Test-slot validation: deploy the CI-built image, not hand-streamed artifacts

**Status:** Accepted plan / migration spec. Supersedes the artifact-streaming
hot-swap model — `docs/test-slot-hot-swap.md`, the `retired apply tool`
build-and-stream path, the per-project `retired build-stream metadata` build
contract, and tank-operator's `scripts/retired classifier script`
classifier. Those are deletion targets here, not designs to extend
(see `docs/migration-policy.md`).

## Decision

A test slot is validated by **deploying the exact image CI already built and
pushed to ACR for the verified commit** — never by building artifacts in an
ephemeral Job and streaming them into running pods. "Hot swap" — artifact
streaming, per-kind selector selection, and change detection — is removed end
to end. The replacement is "deploy what CI built," which is simultaneously the
*correct* thing and the *fast* thing.

## Why this, and why now

- **Hot-swap only ever existed to dodge a slow image build.** Streaming a
  hand-built binary into a running pod was a workaround for not having a ready
  artifact. CI already builds the image — so the problem hot-swap solved is
  solved upstream, canonically, by the Dockerfile build itself.
- **CI-green ⟺ the fingerprinted image is in ACR.** They are the same event.
  PR CI builds and pushes the fingerprinted proof image; merge promotes that
  same image (it does not rebuild). So the moment a commit is legitimate, the
  exact artifact that will ship is already in the registry, keyed to that SHA.
  The thing the gate verifies and the thing the slot deploys become one object.
- **Accuracy goes up.** The old hot-swap Job rebuilt artifacts with its own
  command (`go build` with ad-hoc flags, a from-scratch Go toolchain install),
  which is not guaranteed byte-identical to the Dockerfile build. Deploying the
  CI image means the slot runs the *exact* artifact CI built and main will
  deploy — the test target stops being an approximation.
- **The detection was the wrong shape and it failed open.** The accuracy
  classifier's impact-allowlist + rebuild-trigger-denylist let any unrecognized
  file fall through as "assumed swappable," silently testing stale code — a
  classifier guard that fails open is worse than none. It was enabled on exactly
  one project (tank-operator) and absent/disabled on every other, which is the
  tell that it was a one-app wart mistaken for a platform feature.
- **The loop is CI-bound anyway.** Because validation requires CI-green code
  (below), the wall-clock is dominated by CI, not by the final deploy step. A
  ~30–60s image redeploy versus a ~90s artifact stream is noise against minutes
  of CI — so the streaming machinery bought almost nothing while costing the
  whole detection/config/maintenance surface.

## Invariant

A slot runs **exactly the CI-built, fingerprinted image** for a branch head that
is **published, CI-green, mergeable, and current with main** — verified after
deploy. No partial state, no hand-built artifact, no agent judgement in the
path. The only thing that can land on a slot is the real, verified,
going-to-ship build.

## Mechanism

1. **One governed tool, agent says one thing.** "Validate `<branch>` on a slot"
   (or fire automatically on publish). The agent supplies a ref and nothing
   else — no kind selector, no cluster access, no token. Every step runs in a
   glimmung-owned Job/operation with its own scoped identity, so it stays
   observable and hookable (the reason the platform moved off `kubectl cp` in
   the first place — that property is preserved).
2. **Legitimacy gate (reuse the existing verify gate + control-action ledger).**
   The tool refuses to deploy unless the head is published, CI-green, mergeable,
   and **not behind main**. These facts come from the durable
   `control_action_events` ledger that the hot-swap verify gate already reads
   (the ledger un-frozen by `romaine-life/tank-operator#1253`). Because the tool
   only ever operates on a *published git ref* — never an agent working tree —
   uncommitted scratch code is structurally unable to reach a slot. The gate is
   what stops broken/stale code; the ref-only input is what stops un-pushed
   code.
3. **Resolve SHA → CI image.** Map the verified commit to the CI run that built
   or reused the app image, then deploy that run's lookup alias. The deploy path
   must not assume the raw commit SHA is the image tag. The canonical image
   identity is still `app-<fingerprint>`; trusted app-image CI creates a
   run-scoped ACR alias pointing at the same manifest after the fingerprinted
   image exists:
   - pull request: `ci-pr-<pr_number>-run-<run_id>-attempt-<attempt>`
   - dispatch/non-PR: `ci-ref-<source_sha_hash>-run-<run_id>-attempt-<attempt>`

   Glimmung resolves the verified SHA through GitHub Actions workflow-run
   metadata, constructs the expected lookup tag, and validates the tag in ACR
   before any slot mutation. A missing alias is a deploy-image error, not an
   ImagePullBackOff. Hand-maintained project metadata maps such as
   `tags_by_sha` and `images_by_sha` are retired; future durability work should
   project the CI run/image facts into the same ledger the legitimacy gate reads.
4. **Deploy the image — two levels, both "deploy the verified image":**
   - **App-level** (backend, static, any app): repoint the slot's app
     Deployment at the validated CI lookup tag that points to
     `app-<fingerprint>` and roll. Fast — the slot already ran the prior image,
     so only the changed layer pulls.
   - **Runner / session-level**: start a fresh session pod on the slot from the
     CI-built session image, which boots the new runner natively. Same
     principle, different tag.
5. **Verify the end state.** Confirm the slot's running image equals the
   resolved fingerprint (health gate + image assertion) and record the terminal
   outcome durably. "Reached the right destination" is verified, never assumed.

## What gets deleted (end to end — no compat layer, no parallel path)

Per `docs/migration-policy.md`, the old path is removed, not fenced off:

- `retired apply tool` build-and-stream path: the in-Job artifact build,
  the tar-over-exec streaming into running pods, the SIGHUP-on-streamed-artifact
  restart, and the artifact-kind dispatch (`resolveArtifact`,
  `renderApplyHotSwapJobSpec`, the per-kind switch).
- The per-project `retired build-stream metadata` build contract:
  `build_command`, `builder_image`, `source`/`target`, every per-artifact block
  (`static`, `backend`, `agent_runner`, `codex_runner`, `antigravity_runner`),
  and `restart`.
- tank-operator `scripts/retired classifier script`, the
  `accuracy_classifier` contract block, the `GLIMMUNG_HOT_SWAP_*` env plumbing,
  and the `--enforce` gate.
- mcp-glimmung's `kind selector` parameter and the multi-kind surface (never
  shipped; do not build it).
- Old behaviour tests and docs (`docs/test-slot-hot-swap.md`) — replaced by this
  doc and the new deploy-from-image contract.

A migration guard must fail if any artifact-build/stream path, per-artifact
contract block, or classifier is reintroduced into live code.

## What gets built

- **glimmung:** a `deploy_image_to_slot` operation — resolve SHA → fingerprint,
  set the slot Deployment's image, roll, and verify the running image; the
  durable SHA → image resolution; the **behind-main** check in the verify gate
  (today's mergeability check treats a *behind* branch as mergeable, which is
  the exact "tested something that got merge-conflict-fixed later" waste this is
  meant to kill).
- **mcp-glimmung:** the tool becomes ref-in / deploy-and-verify-out, with no
  `kind selector`.
- **tank-operator:** delete the classifier and its CI guard/wiring. Keep the
  verify gate and the control-action ledger (already corrected in #1253).

## Per-app implications

- **Per-app config collapses.** It drops from the per-artifact hot-swap
  contract to nothing beyond the existing `test_slot_helm` (how an app deploys
  to a slot) plus the SHA → image lookup. There is nothing to allowlist —
  whole images are deployed, so "which files are swappable" stops being a
  question. This obsoletes the swappable-surface allowlist that was being
  designed.
- **Precondition.** Every in-scope project must have CI that builds and pushes a
  runnable image to ACR *before* main. tank-operator does. ambience,
  chess-tactics, glimmung, spirelens, and kill-me must be confirmed and adopted
  where missing (the project owner has been standardizing on this path).

## What we deliberately give up

The single lost capability is **patching a change into a live, mid-conversation
session without restarting it.** That is a debugging convenience, never a
validation need (validation runs on a fresh session), and it was the most
complex, most app-specific corner of the old design. Accepted.

## Staging (each stage leaves the system coherent)

1. **Done — `tank-operator#1253`:** un-freeze the `control_action_events`
   ledger the verify gate reads (authorize control-action writes off the
   verified per-session subject).
2. **glimmung — add the deploy path:** `deploy_image_to_slot` + SHA→image
   resolution + the behind-main check, landing *alongside* the existing
   `retired apply tool` so slots keep working during rollout.
3. **mcp-glimmung — switch the tool:** ref-in / deploy-out; stop accepting
   `kind selector`.
4. **Cutover + deletion:** remove the artifact-stream path, the
   `retired build-stream metadata` build contract, and the tank-operator classifier end to
   end; land the migration guards. No parallel path survives. **Gated on stage 6
   green for every in-scope app** — the old path is not deleted until the new
   one is proven per app.
5. **Per-app CI images:** confirm/adopt pre-merge CI image builds for every
   in-scope project (the precondition stage 6 deploys from).
6. **Per-app deploy smoke (gates cutover):** prove the new path end to end on
   every in-scope app — see below. This is the gate that authorizes stage 4; the
   last rollout skipped it and shipped a false pass.

## Stage 6 — per-app deploy smoke (the cutover gate)

The last rollout's failure mode was a **false pass**: it validated narrowly,
declared done, and broke things the validation never exercised. So cutover is
gated on a per-app, end-to-end smoke that **observes the change in the running
slot**, not on a Job reporting success ("observed outcomes beat claimed intent"
— `docs/quality-timeframes.md`, `product-inspirations`). Per in-scope project,
the smoke proves:

1. **The deploy lands a known build (process check).** Deploy a specific
   verified ref and assert the slot's pod is running the *exact* resolved image
   fingerprint (k8s image read). Catches "the deploy mechanism didn't apply."
2. **The app actually serves that build, observed from outside (content
   check — the load-bearing one).** Read a unique, build-derived value back from
   the slot's real surface: the commit SHA via a `/version`/health field where
   one exists, otherwise the build-stamped frontend asset (the bundle hash
   changes per build) or a deliberately injected sentinel. The value must be
   unique per run so a stale image cannot false-pass. This is "it showed up in
   the slot." Catches "image deployed but the app serves stale."
3. **Replace, not first-install.** Deploy version A, then B, and assert the slot
   moved A → B. The replace onto an already-running slot is the realistic
   condition; validating only a clean slot is the exact gap that bit the last
   rollout.
4. **The gate refuses illegitimate code (negative path).** Push a CI-red ref and
   a behind-main ref and assert the slot is **not** updated and the tool reports
   why. The gate's whole value is blocking bad code — prove the block, not just
   the pass.
5. **Clean terminal, no trip.** The deploy/verify recorded a durable success,
   the slot is healthy afterward, and there is no error surface.

Coverage spans the projects whose deploy shapes differ — ambience (WASM edge),
chess-tactics (node), glimmung, tank-operator (multi-pod + session pods), and
spirelens/kill-me if in scope. The smoke is **automated and retained** as a
glimmung slot-smoke playbook (glimmung already owns the verify loop, evidence,
and `slot-playwright`), not a one-time manual pass — so every future change to
the deploy path re-proves every app. **Stage 4 does not proceed until this is
green for every in-scope project.**

## Open items to verify before stage 2

- Confirm each in-scope project's CI pushes a *runnable* image (not a
  build-only check) to ACR pre-merge, and the tag is resolvable from the SHA.
- Confirm the runner/session path: runner code ships in the *session* images
  (`session-agent-claude`, `session-agent-codex`), resolved by a different tag
  than the app image.
- Confirm GitHub `mergeable_state` exposes `behind` distinctly so the
  behind-main check is precise (vs. inferring from ahead/behind counts).
- Replace the stage-2 `test_slot_deploy.ci_image` project metadata mapping with
  a CI-owned ledger/projection (extend the control-action ledger vs. a
  glimmung-side projection) so SHA → fingerprint resolution is durable,
  auditable, and not hand-maintained per project.

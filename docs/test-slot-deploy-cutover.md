# Cutover: delete the artifact hot-swap path

**Status:** ready to execute. This is the deletion stage (Stage 4) of the
deploy-image-to-slot migration — see `test-slot-deploy-plan.md`. The replacement
(`deploy-image-to-slot`) is **live and proven** end-to-end on tank-operator
(deploy lands the exact image; a frontend+backend change is served from outside;
replace A→B works; the gate/verify fail-safe fires). So the old path can go.

## Goal

Delete the artifact **build-and-stream** hot-swap path end to end. Per
`docs/migration-policy.md` (binding): no compat alias, no parallel path, no
`legacy`/`fallback` shim. When done, `apply_test_slot_hot_swap`, the
`test_slot_hot_swap` build contract, the fidelity classifier, and the
`artifact_kind` surface do not exist in live code, and a guard prevents their
return.

## ⚠️ The one trap: delete vs. keep-and-rename

`deploy-image-to-slot` deliberately **reuses** part of the `hot_swap`-named
plumbing. Do **not** blind-delete everything named `hot_swap`.

**KEEP (deploy depends on these) — and RENAME to neutral, non-`hot_swap` names**
(the `hot_swap` naming is itself retired-path naming the migration should clear):

| Surface | File / location | Why kept |
|---|---|---|
| Status/poll route `GET /v1/test-slots/apply-hot-swap/{project}/{job}` → `getApplyHotSwapStatus` | `glimmung server.go:496`, `test_slot_apply_hot_swap_status.go` | the deploy tool polls this for `running → deployed/deploy_failed` |
| History reader `latestHotSwapEntryForJob` / `hotSwapEntryJobName` / `isTerminalHotSwapStatus` | `glimmung hot_swap_history_read.go` | reads the durable `image_deploy` history entries |
| Lease metadata `test_slot_hot_swap_history` + `AppendTestSlotHotSwapHistory` | glimmung store + lease metadata | deploy writes its `running`/terminal entries here |
| mcp poll path | `mcp-glimmung tools.py` `/v1/test-slots/apply-hot-swap/{project}/{job}` | must move in lockstep with the route rename |

Suggested neutral naming: route `GET /v1/test-slots/jobs/{project}/{job}` (or
`…/slot-ops/…`), history → `slotOpHistory` / `latestSlotOpEntryForJob`, lease
metadata `test_slot_op_history`, statuses keep `deployed`/`deploy_failed` (the
op string is already `image_deploy`). Pick one neutral name and apply it
consistently across glimmung + mcp-glimmung. **Re-run the deploy smoke after the
rename** (below) — the rename is the highest-risk part because it sits under the
live deploy path.

## DELETE — the artifact build/stream path

**glimmung** (`internal/server/`):
- `test_slot_apply_hot_swap_api.go` + `_test.go` — the `applyTestSlotHotSwap`
  endpoint, `artifact_kind`/`validation_target` request, the per-kind dispatch.
- `test_slot_apply_hot_swap_ops.go` + `_test.go` — `resolveArtifact`, the
  build-in-Job + tar-over-exec stream + SIGHUP-on-artifact, `FidelityCommand`.
- `test_slot_hot_swap_api.go` — the `test_slot_hot_swap` **contract** read API
  (`get_test_slot_hot_swap_contract` backing) and the per-artifact block parse
  (`static`/`backend`/`agent_runner`/`codex_runner`/`antigravity_runner`,
  `restart`, `fidelity_classifier`).
- `hot_swap_diff.go` + `_test.go` — `base_ref` diff resolution (apply-only).
- `apply_hot_swap_watcher.go` + `_test.go` — the apply finalizer watcher.
  ⚠️ verify nothing in the deploy path depends on it before deleting; if the
  finalizer is shared, split out only the deploy-relevant piece.
- `server.go` — remove `POST /v1/test-slots/apply-hot-swap`, the
  `applyPerformer`/`applyDiffResolver` wiring; **keep** the status GET (renamed).
- the route-inventory contract test entry for the deleted POST.

**mcp-glimmung** (`src/mcp_glimmung/`):
- `tools.py` — delete `apply_test_slot_hot_swap`, `record_test_slot_hot_swap`
  (if apply-only), `get_test_slot_hot_swap_contract`, and the `artifact_kind`
  parameter/`_DISPATCH` machinery. Keep `deploy_image_to_test_slot`. Update the
  poll path for the route rename. `tests/test_tools.py` accordingly.

**tank-operator**:
- `scripts/classify-tank-test-fidelity.mjs` — delete.
- `claude-container/mcp-auth-proxy/src/mcp_auth_proxy/server.py` —
  `_GLIMMUNG_HOT_SWAP_TOOL`, `_augment_glimmung_hot_swap_tool_schema`, and the
  `GLIMMUNG_HOT_SWAP_*` env plumbing.
- `README.md`, `k8s/session-config/skills/common/test/references/repos/tank-operator.md`,
  and any other doc/skill that documents the hot-swap apply flow → rewrite to
  deploy-image-to-slot.
- `scripts/check-session-pod-hot-swap-migration.mjs` — this is a **guard**;
  extend it (don't delete) to forbid the deleted apply/classifier surface
  (below).

**per-project metadata** (glimmung project store, via `register_project`):
- remove the `metadata.test_slot_hot_swap` block from **ambience,
  chess-tactics, glimmung, tank-operator**. (kill-me already has none.)
- ⚠️ **Determine `register_project`/`UpsertProject` merge-vs-replace semantics
  first.** If it REPLACES metadata, fetch each project's full current metadata
  (`list_projects`), drop only the `test_slot_hot_swap` key, and re-register the
  whole object — do **not** clobber `runner_standby_dns`,
  `runner_standby_workload_identity`, `managed_auth_origins`, `test_slot_helm`,
  etc. Verify each project round-trips unchanged except the dropped key.
- Also add a write-surface guard (mirror `validateTestSlotHelmMetadata`) that
  rejects a `test_slot_hot_swap` block on register, so it can't be re-added.

## Staging (each stage leaves the system coherent)

The apply path is being retired, deploy is the replacement — but the apply POST
may still have live callers, so drain/announce before deletion if needed.

1. **Rename the shared surface** (glimmung + mcp-glimmung in lockstep) to neutral
   names, keeping behavior identical. Deploy smoke must stay green. (Lowest-risk
   to do first, isolates the rename from the deletes.)
2. **mcp-glimmung**: delete the apply tool + `artifact_kind` + contract tool.
3. **tank-operator**: delete the classifier, the proxy augmentation, the env, the
   docs; extend the migration guard.
4. **glimmung**: delete the apply endpoint/ops/contract/diff/watcher + tests;
   remove the apply route; add a guard.
5. **per-project metadata**: drop `test_slot_hot_swap` from the 4 projects
   (merge-safe re-register) + the write-surface reject guard.

## Migration guards (completion requirement, not follow-up)

- glimmung: a `scripts/check-*.mjs` (or Go test) that fails if
  `applyTestSlotHotSwap`, `artifact_kind`, `resolveArtifact`, `FidelityCommand`,
  or a `test_slot_hot_swap` contract parse reappears in live (non-test) code.
- tank-operator: extend `scripts/check-session-pod-hot-swap-migration.mjs` to
  forbid `classify-tank-test-fidelity`, `_GLIMMUNG_HOT_SWAP_TOOL`,
  `GLIMMUNG_HOT_SWAP_*`, and `apply_test_slot_hot_swap`.
- glimmung project write: reject a `test_slot_hot_swap` metadata block.

## Validation (observed outcomes, not claimed)

1. **Deploy still works after the rename**: reserve a tank-operator slot, deploy
   a SHA-tagged commit via `deploy_image_to_test_slot` (or
   `POST /v1/test-slots/deploy-image`), poll the **renamed** status route to
   `deployed`, confirm the slot serves that image. (The repeatable smoke pattern
   is in this session's history — deploy A then B, read a build marker back from
   the slot's real surface.)
2. **Grep is clean**: no `apply_test_slot_hot_swap`, `artifact_kind`,
   `resolveArtifact`, `fidelity`, `classify-tank-test-fidelity`,
   `_GLIMMUNG_HOT_SWAP_TOOL`, `test_slot_hot_swap` (contract) in live code across
   all four repos.
3. **All repos' CI green**; the new guards fail on a re-introduction attempt.
4. No `legacy`/`compat`/`fallback`/`deprecated` shim was added (migration-policy).

## Do NOT

- Touch the `deploy-image-to-slot` path itself — it is the replacement.
- Break the shared status/history surface — rename it, keep it working, re-smoke.
- Leave any `hot_swap` naming on the *kept* surface (the rename is part of done).
- Blind-`git rm` files by name match — verify each against the deploy path first.

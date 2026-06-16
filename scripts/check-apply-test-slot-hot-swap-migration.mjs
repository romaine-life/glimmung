#!/usr/bin/env node

// Completion manifest for the apply_test_slot_hot_swap migration.
//
// This script is the spec. "Done" = exit 0. Same workflow as
// tank-operator's scripts/check-stop-request-migration.mjs and
// scripts/check-session-pod-hot-swap-migration.mjs: committed as commit 1
// of the branch, before any feature code, so the contract is auditable
// independently of whatever the agent later writes.
//
// CONTEXT
//
// PR #494 in tank-operator landed the *mechanism* for session-pod
// agent-runner hot-swap (supervisor + writable target + SIGHUP re-exec).
// It surfaced that test-slot hot-swap as a whole is half-finished: glimmung
// exposes get_test_slot_hot_swap_contract and record_test_slot_hot_swap MCP
// tools, but no apply tool — every developer (or AI agent) ends up running
// kubectl-fu by hand. The /test skill literally documents the manual
// pattern. Per docs/quality-timeframes.md "prefer complete architecture
// over quick relief", this is the missing half.
//
// This PR closes the gap: a single HTTP endpoint that takes a git ref +
// a slot identifier and does end-to-end build-and-swap. The endpoint
// dispatches a one-off Kubernetes Job (init container for build, main
// container for kubectl-stream + signal), watches it to completion, and
// returns a structured result. Synchronous by default per the ArgoCD
// `app sync` pattern (researched against Google AIP-151 for async-only
// APIs; ArgoCD's developer-driven shape is the closer analog for our
// use case).
//
// THE CONTRACT — four user-named guarantees:
//
//   1. "Place new code, step back" works end-to-end. The new MCP tool
//      (apply_test_slot_hot_swap) takes a project, slot, artifact_kind,
//      and git_ref. Caller's only action is the call. Glimmung does
//      clone + build + copy + signal + verify + record-history.
//
//   2. Each app declares its own build environment. The contract's
//      builder_image field per artifact_kind tells glimmung which
//      container image to use for the build step (e.g., node:20-alpine
//      for agent_runner, golang:1.26-alpine for backend). No language
//      heuristics, no hardcoded defaults — the contract owns this.
//
//   3. Async dispatch + durable finalize + poll (supersedes the original
//      synchronous design). The POST dispatches the build-and-swap Job and
//      returns immediately with a "running" handle + an initial history
//      entry; the build-and-swap deadline is enforced on the Job
//      (activeDeadlineSeconds, default ~120s, hard cap ~600s), not on a held
//      HTTP request; a ControlPlaneLoopsEnabled-gated finalizer records the
//      terminal outcome (persisted | build_failed | swap_failed | timeout)
//      when the Job completes; the caller polls
//      GET /v1/test-slots/apply-hot-swap/{project}/{job} until terminal.
//      The durable hot-swap history — not the response body — is the source
//      of truth, and every write runs on a request-detached context
//      (context.WithoutCancel) so a client disconnect, proxy deadline, or
//      orchestrator rollout can never abort it. This replaced the original
//      "endpoint blocks until done" shape, which tied the result and the
//      history write to the inbound connection and surfaced only a ~30s
//      proxy timeout while the Job ran on to completion. Issue 3: the
//      classifier diff context (base...head changed files) is resolved
//      server-side and plumbed into the build container as
//      GLIMMUNG_CHANGED_FILES, since the shallow build checkout cannot
//      compute a real diff.
//
//   4. The apply endpoint is now the ONLY hot-swap path. backend joined
//      static + the runner kinds on the CI-gated endpoint, so the legacy
//      glimmung-agent test-slot-hot-swap subcommand and its
//      internal/ops/agentops/hotswap.go implementation are DELETED — a
//      read-only / restricted-git session can no longer be told to run
//      kubectl-fu by hand for backend. Per docs/migration-policy.md the old
//      path is removed end to end, and these checks fail if it returns. The
//      /v1/test-slots/hot-swap-history endpoint and the other
//      /v1/test-slots/* endpoints stay unchanged.
//
// Skip slow exec gates during structural iteration with:
//   SKIP_EXEC=1 node scripts/check-apply-test-slot-hot-swap-migration.mjs

import { spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const skipExec = process.env.SKIP_EXEC === "1";

const CHECKS = [
  // ─────────────────────── Guarantee 1: end-to-end "place new code, step back" ───────────────────────

  {
    id: "endpoint-registered",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/server.go",
    description: "POST /v1/test-slots/apply-hot-swap registered in the server mux",
    kind: "grep-present",
    pattern: /POST\s+\/v1\/test-slots\/apply-hot-swap/,
  },
  {
    id: "endpoint-handler-exists",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "applyTestSlotHotSwap handler function exists in its own file (mirrors test_slot_hot_swap_api.go shape)",
    kind: "grep-present",
    pattern: /func applyTestSlotHotSwap\(/,
  },
  {
    id: "endpoint-request-shape",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Request struct names project, slot identifier (index or name), artifact_kind, git_ref, optional timeout_seconds",
    kind: "grep-present",
    pattern: /type\s+TestSlotApplyHotSwapRequest\s+struct[\s\S]{0,500}?Project[\s\S]{0,500}?ArtifactKind[\s\S]{0,500}?GitRef/,
  },
  {
    id: "endpoint-response-shape",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Response struct names the build/copy/restart/health result fields the dev needs to diagnose a failure",
    kind: "grep-present",
    pattern: /type\s+TestSlotApplyHotSwapResult\s+struct/,
  },
  {
    id: "endpoint-resolves-lease",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler resolves the lease via the existing resolveTestSlotLease helper (matches the record-history pattern)",
    kind: "grep-present",
    pattern: /resolveTestSlotLease/,
  },
  {
    id: "endpoint-reads-contract",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler reads the project's contract via hotswap.FromMetadata (same path as the project-write validator)",
    kind: "grep-present",
    pattern: /hotswap\.FromMetadata/,
  },
  {
    id: "endpoint-dispatches-job",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler dispatches the build-and-swap Job via the agentops performer (function-typed seam for testability; production wires to *Ops.ApplyHotSwap)",
    kind: "grep-present",
    pattern: /applyHotSwapPerformer|agentops\.ApplyHotSwapOptions/,
  },
  {
    id: "endpoint-records-history-always",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler records hot-swap history on every outcome (success, build_failed, swap_failed, timeout) so durable state lives in the system",
    kind: "grep-present",
    pattern: /AppendTestSlotHotSwapHistory/,
  },
  {
    id: "ops-dispatcher-fn",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Dispatcher function DispatchHotSwap takes a k8sJobClient (no kubectl shell-out — glimmung pod has no kubectl; matches the native_launcher request() pattern) and returns without waiting",
    kind: "grep-present",
    pattern: /func\s+DispatchHotSwap\([\s\S]{0,200}?k8sJobClient/,
  },
  {
    id: "ops-dispatcher-nonblocking",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "DispatchHotSwap returns Outcome=\"running\" after ApplyJob — it does NOT block on a synchronous WaitForJob (the gated finalizer owns completion).",
    kind: "grep-present",
    pattern: /ApplyJob\([\s\S]{0,400}?Outcome\s*=\s*"running"/,
  },
  {
    id: "ops-no-synchronous-wait",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "The blocking WaitForJob poll-to-completion is removed from the apply path — the deadline lives on the Job (activeDeadlineSeconds) and the finalizer reads the terminal status. Reintroduction fails this check.",
    kind: "grep-absent",
    pattern: /WaitForJob/,
  },
  {
    id: "ops-job-active-deadline",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "The build-and-swap timeout is enforced as the Job's spec.activeDeadlineSeconds (k8s fails the Job with DeadlineExceeded on overrun), not by a held request.",
    kind: "grep-present",
    pattern: /activeDeadlineSeconds/,
  },
  {
    id: "ops-dispatcher-job-spec",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Job spec uses init container for build, main container for swap (sequential via initContainers)",
    kind: "grep-present",
    pattern: /initContainers|InitContainers/,
  },
  {
    id: "ops-finalize-classifies-terminal",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "finalizeHotSwap classifies an already-terminal Job from its status + logs into the bounded outcome set, mapping DeadlineExceeded → timeout. It does not wait — the gated finalizer invokes it once the apiserver reports the Job terminal.",
    kind: "grep-present",
    pattern: /func\s+finalizeHotSwap\([\s\S]{0,800}?DeadlineExceeded[\s\S]{0,200}?"timeout"/,
  },
  {
    id: "finalizer-watcher-gated",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/apply_hot_swap_watcher.go",
    description: "The apply-hot-swap finalizer is a ControlPlaneLoopsEnabled-gated watcher (slot processes never finalize prod Jobs), reusing the cluster-wide k8sJobWatcher to detect terminal Jobs event-driven.",
    kind: "grep-multi-present",
    patterns: [
      /func\s+StartApplyHotSwapJobWatcher\(/,
      /func\s+shouldStartApplyHotSwapJobWatcher\([\s\S]{0,200}?ControlPlaneLoopsEnabled/,
      /func\s+\(w \*k8sJobWatcher\)\s+dispatchHotSwapTerminal\(/,
    ],
  },
  {
    id: "finalizer-wired-gated-block",
    from: "Guarantee 1: end-to-end apply",
    file: "cmd/glimmung-go/main.go",
    description: "StartApplyHotSwapJobWatcher is wired in the ControlPlaneLoopsEnabled-gated reconciler block alongside the run-job watcher.",
    kind: "grep-present",
    pattern: /StartApplyHotSwapJobWatcher\(/,
  },
  {
    id: "finalizer-idempotent",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/apply_hot_swap_watcher.go",
    description: "The finalizer is idempotent across duplicate apiserver events / post-restart re-lists: it skips appending a second terminal entry when one already exists for the job.",
    kind: "grep-present",
    pattern: /hotSwapJobHasTerminalEntry/,
  },
  {
    id: "ops-backend-resolved",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "resolveArtifact maps artifact_kind=backend to a resolved artifact (single-file ArtifactFile + HealthPath/HealthPort), so the apply endpoint accepts backend.",
    kind: "grep-present",
    pattern: /case\s+"backend":[\s\S]{0,400}?ArtifactFile:\s*b\.Artifact/,
  },
  {
    id: "ops-backend-single-file-swap",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Backend swap streams one executable to Target.next, chmod +x, atomic mv, then SIGHUP — not the runner/static dir-extract path.",
    kind: "grep-present",
    pattern: /func\s+backendSwapSteps[\s\S]{0,1500}?chmod \+x[\s\S]{0,200}?mv -f/,
  },
  {
    id: "ops-backend-health-gate",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Backend swap health-gates the re-exec by polling http://127.0.0.1:<HealthPort><HealthPath> inside the pod, so a crashing binary yields swap_failed, not persisted.",
    kind: "grep-present",
    pattern: /127\.0\.0\.1:%d%s/,
  },
  {
    id: "api-backend-request-fields",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Apply endpoint enforces backend request-time fields (builder_image, pod_selector, container, health_port) before dispatch.",
    kind: "grep-present",
    pattern: /ArtifactKind\s*==\s*"backend"[\s\S]{0,600}?HealthPort\s*<=\s*0/,
  },

  // ─────────────────────── Guarantee 2: each app declares its own builder ───────────────────────

  {
    id: "contract-agent-runner-struct",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract struct exists alongside StaticContract and BackendContract",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct/,
  },
  {
    id: "contract-agent-runner-on-contract",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Contract has runner fields of type AgentRunnerContract",
    kind: "grep-present",
    pattern: /type\s+Contract\s+struct[\s\S]{0,500}?AgentRunner\s+AgentRunnerContract[\s\S]{0,200}?CodexRunner\s+AgentRunnerContract[\s\S]{0,200}?AntigravityRunner\s+AgentRunnerContract/,
  },
  {
    id: "contract-builder-image-backend",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "BackendContract has BuilderImage field (per-app build environment)",
    kind: "grep-present",
    pattern: /type\s+BackendContract\s+struct[\s\S]{0,2400}?BuilderImage\s+string/,
  },
  {
    id: "contract-builder-image-static",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "StaticContract has BuilderImage field",
    kind: "grep-present",
    pattern: /type\s+StaticContract\s+struct[\s\S]{0,1600}?BuilderImage\s+string/,
  },
  {
    id: "contract-builder-image-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract has BuilderImage field",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct[\s\S]{0,2000}?BuilderImage\s+string/,
  },
  {
    id: "contract-agent-runner-required-fields",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract has source/target/build_command/restart/container/pod_selector (the kubectl-orchestration inputs)",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct[\s\S]{0,2500}?Source[\s\S]{0,800}?Target[\s\S]{0,800}?BuildCommand[\s\S]{0,800}?PodSelector/,
  },
  {
    id: "contract-validate-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Contract.Validate enforces required runner fields when enabled (source, target, builder_image, build_command, pod_selector)",
    kind: "grep-present",
    pattern: /validateRunnerContract[\s\S]{0,2000}?BuilderImage/,
  },
  {
    id: "contract-validate-builder-image-required-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Validate rejects empty builder_image when a runner is enabled (apply endpoint is the only consumer of runner artifacts; no legacy CLI fallback)",
    kind: "grep-present",
    pattern: /validateRunnerContract[\s\S]{0,2500}?"builder_image"[\s\S]{0,500}?runner\.BuilderImage/,
  },
  {
    id: "contract-validate-builder-image-applytime-backend",
    from: "Guarantee 2: per-app builder",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Apply endpoint rejects request when Backend artifact_kind is requested but builder_image is missing (validated at request time, not at Contract validation — keeps existing registered contracts from breaking)",
    kind: "grep-present",
    pattern: /Backend[\s\S]{0,400}?BuilderImage[\s\S]{0,400}?(?:required|missing|empty)/,
  },

  // ─────────────────────── Guarantee 3: sync UX, ArgoCD pattern, durable state ───────────────────────

  {
    id: "endpoint-default-timeout-bounded",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler enforces a server-side default timeout (~120s) when caller doesn't specify",
    kind: "grep-present",
    pattern: /(?:DefaultApplyHotSwap|defaultApplyHotSwap)?Timeout[\s\S]{0,400}?(?:120|2\*time\.Minute|2 ?\* ?Minute)/,
  },
  {
    id: "endpoint-hard-cap-timeout",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler clamps caller-provided timeout to a hard server max (~600s) to prevent dangling requests",
    kind: "grep-present",
    pattern: /(?:MaxApplyHotSwap|maxApplyHotSwap)?Timeout[\s\S]{0,400}?(?:600|10\*time\.Minute|10 ?\* ?Minute)/,
  },
  {
    id: "endpoint-detached-durable-write",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "The dispatch + initial history write run on a request-detached context (context.WithoutCancel), so a client disconnect can never abort the durable record — the issue-2 fix.",
    kind: "grep-present",
    pattern: /context\.WithoutCancel\([\s\S]{0,3000}?AppendTestSlotHotSwapHistory/,
  },
  {
    id: "endpoint-returns-handle-nonblocking",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "The handler dispatches via the performer and returns the structured result (job handle + initial running entry) without waiting for the Job to finish.",
    kind: "grep-present",
    pattern: /performer\([\s\S]{0,4000}?writeJSON\(/,
  },
  {
    id: "status-poll-endpoint-registered",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/server.go",
    description: "GET /v1/test-slots/apply-hot-swap/{project}/{job} poll route registered so the caller can turn a non-blocking dispatch into a synchronous result without a long-held request.",
    kind: "grep-present",
    pattern: /GET\s+\/v1\/test-slots\/apply-hot-swap\/\{project\}\/\{job\}/,
  },
  {
    id: "status-poll-handler-exists",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/test_slot_apply_hot_swap_status.go",
    description: "getApplyHotSwapStatus handler reads the durable hot-swap history entry for a dispatched job (running → terminal).",
    kind: "grep-present",
    pattern: /func\s+getApplyHotSwapStatus\(/,
  },
  {
    id: "classifier-diff-resolved",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "The handler resolves the classifier diff context (base...head changed files) via the injectable hotSwapDiffResolver seam before dispatch — issue 3.",
    kind: "grep-present",
    pattern: /hotSwapDiffResolver|resolveDiff\(/,
  },
  {
    id: "classifier-diff-plumbed-to-build",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "The resolved changed-file set is plumbed into the build container as GLIMMUNG_CHANGED_FILES so the fidelity classifier sees a real diff instead of the empty shallow-checkout diff — issue 3.",
    kind: "grep-present",
    pattern: /GLIMMUNG_CHANGED_FILES/,
  },
  {
    id: "endpoint-history-on-failure",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler appends a hot-swap history entry with a failure-named status even when build/swap/health fails (durable failure record)",
    kind: "grep-multi-present",
    patterns: [
      /AppendTestSlotHotSwapHistory/,
      /Status:\s*status/,
      /"build_failed"|"swap_failed"|"timeout"/,
    ],
  },
  {
    id: "observability-outcome-tracked-in-result",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Result struct carries a bounded Outcome field with the named failure modes (persisted | build_failed | swap_failed | timeout); these flow into the durable hot-swap history record.",
    kind: "grep-present",
    pattern: /Outcome[\s\S]{0,400}?persisted[\s\S]{0,200}?build_failed[\s\S]{0,200}?swap_failed[\s\S]{0,200}?timeout/,
  },
  {
    id: "observability-outcome-prometheus-counter",
    from: "Guarantee 3: async dispatch + durable state",
    file: "internal/server/apply_hot_swap_watcher.go",
    description: "The finalizer increments glimmung_hot_swap_outcomes_total via metrics.RecordHotSwap when it records each terminal outcome (counted once per Job, on the durable-finalize path rather than the now-non-blocking handler).",
    kind: "grep-present",
    pattern: /metrics\.RecordHotSwap\(result\.Outcome/,
  },

  // ─────────────────────── Guarantee 4: nothing already-working is touched ───────────────────────

  {
    id: "existing-history-endpoint-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    description: "internal/server/test_slot_hot_swap_api.go is byte-identical to origin/main (history endpoint preserved)",
    kind: "git-diff-empty",
    paths: ["internal/server/test_slot_hot_swap_api.go"],
    base: "origin/main",
  },
  {
    id: "legacy-cli-hotswap-subcommand-removed",
    from: "Guarantee 4: legacy path retired",
    file: "cmd/glimmung-agent/main.go",
    description: "The glimmung-agent test-slot-hot-swap subcommand is gone — backend hot-swap runs only through the CI-gated apply endpoint. Reintroduction fails this check.",
    kind: "grep-absent",
    pattern: /test-slot-hot-swap|TestSlotHotSwap/,
  },
  {
    id: "legacy-agentops-hotswap-file-removed",
    from: "Guarantee 4: legacy path retired",
    file: "internal/ops/agentops/hotswap.go",
    description: "internal/ops/agentops/hotswap.go (the client-side kubectl cp + SIGHUP TestSlotHotSwap implementation) is deleted end to end.",
    kind: "file-absent",
  },
  {
    id: "legacy-cli-backend-pointer-removed",
    from: "Guarantee 4: legacy path retired",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "The apply endpoint no longer routes backend callers to the legacy glimmung-agent CLI (that pointer is removed now that backend is wired into the endpoint).",
    kind: "grep-absent",
    pattern: /glimmung-agent CLI/,
  },
  {
    id: "existing-server-routes-only-add",
    from: "Guarantee 4: nothing-else-touched",
    file: "internal/server/server.go",
    description: "Existing /v1/test-slots/* routes still registered (checkout, return, hot-swap-history) — the new apply route is purely additive",
    kind: "grep-multi-present",
    patterns: [
      /POST\s+\/v1\/test-slots\/checkout/,
      /POST\s+\/v1\/test-slots\/return/,
      /POST\s+\/v1\/test-slots\/hot-swap-history/,
      /POST\s+\/v1\/test-slots\/apply-hot-swap/,
    ],
  },
  {
    id: "contract-backend-apply-shape",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "BackendContract gained the apply-endpoint fields HealthPort + PodSelector + Container (health-gated swap of the app pod), preserving the build inputs.",
    kind: "grep-present",
    pattern: /type\s+BackendContract\s+struct[\s\S]{0,2400}?HealthPort\s+int[\s\S]{0,400}?PodSelector\s+string[\s\S]{0,300}?Container\s+string/,
  },
  {
    id: "contract-backend-cli-fields-removed",
    from: "Guarantee 4: legacy path retired",
    file: "internal/domain/hotswap/hotswap.go",
    description: "BackendContract's CLI-only CopyContainer/RestartContainer/RestartCommand fields are deleted (they only fed the removed glimmung-agent hot-swap path).",
    kind: "grep-absent",
    pattern: /CopyContainer|RestartContainer|RestartCommand/,
  },
  {
    id: "existing-static-contract-fields-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    file: "internal/domain/hotswap/hotswap.go",
    description: "StaticContract's existing field set is preserved (Source + Target still in declaration order; BuilderImage is additive)",
    kind: "grep-present",
    pattern: /type\s+StaticContract\s+struct[\s\S]{0,400}?Enabled\s+bool[\s\S]{0,200}?Source\s+string[\s\S]{0,200}?Target\s+string/,
  },

  // ─────────────────────── Tests ───────────────────────

  {
    id: "test-contract-agent-runner-roundtrip",
    from: "Tests",
    file: "internal/domain/hotswap/hotswap_test.go",
    description: "Contract round-trip test exercises FromMetadata + Validate for the new AgentRunner sub-contract (success + missing-field cases)",
    kind: "grep-present",
    pattern: /TestContract.*AgentRunner|AgentRunner.*roundtrip|AgentRunner.*Validate/,
  },
  {
    id: "test-ops-apply-hot-swap-job-spec",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_ops_test.go",
    description: "Test asserts DispatchHotSwap renders the correct Job spec for each artifact_kind (builder_image, init container, main container, volumes) and returns running.",
    kind: "grep-present",
    pattern: /TestDispatchHotSwap/,
  },
  {
    id: "test-backend-dispatches-job",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_ops_test.go",
    description: "Test asserts the backend Job spec uses single-file streaming + SIGHUP + the in-pod health gate, and NOT the dir-extract path.",
    kind: "grep-present",
    pattern: /TestDispatchHotSwapBackendDispatchesJob/,
  },
  {
    id: "test-finalizer-gate-and-record",
    from: "Tests",
    file: "internal/server/apply_hot_swap_watcher_test.go",
    description: "Tests cover the finalizer gate (unreachable when ControlPlaneLoopsEnabled=false), the durable terminal record, idempotency, and the status poll surface.",
    kind: "grep-multi-present",
    patterns: [
      /TestShouldStartApplyHotSwapJobWatcherGate/,
      /TestDispatchHotSwapTerminalRecordsOutcome/,
      /TestDispatchHotSwapTerminalIsIdempotent/,
      /TestGetApplyHotSwapStatusReturnsLatestEntry/,
    ],
  },
  {
    id: "test-classifier-diff",
    from: "Tests",
    file: "internal/server/hot_swap_diff_test.go",
    description: "Test asserts glimmung resolves the changed-file set via the GitHub Compare API (three-dot, default-branch base) for the classifier — issue 3.",
    kind: "grep-present",
    pattern: /TestResolveHotSwapDiffComputesChangedFiles/,
  },
  {
    id: "test-endpoint-happy-path",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test covers happy path (resolve lease + read contract + dispatch + record history + return result)",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*Happy|TestApplyTestSlotHotSwap.*Resolves/,
  },
  {
    id: "test-endpoint-failure-records-history",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test covers failure paths (build_failed / swap_failed / timeout) and asserts hot-swap history is recorded with the failure status",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*(?:Records|Failure|Timeout|Build|Swap)/,
  },
  {
    id: "test-endpoint-timeout-clamping",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test asserts caller timeout is clamped to the hard server max",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*(?:Clamp|Bound|Cap|Max)/,
  },

  // ─────────────────────── Docs ───────────────────────

  {
    id: "docs-new-test-slot-hot-swap-doc",
    from: "Docs",
    file: "docs/test-slot-hot-swap.md",
    description: "New doc describes the workflow + the contract shape + the new MCP tool",
    kind: "grep-present",
    pattern: /apply_test_slot_hot_swap|apply-hot-swap/,
  },
  {
    id: "docs-readme-mcp-surface",
    from: "Docs",
    file: "README.md",
    description: "README MCP-surface section names the new apply tool",
    kind: "grep-present",
    pattern: /apply_test_slot_hot_swap|apply-hot-swap/,
  },
  {
    id: "docs-deprecates-manual-pattern",
    from: "Docs",
    file: "docs/test-slot-hot-swap.md",
    description: "Doc explicitly names the manual kubectl-fu pattern as deprecated (the /test skill currently documents it; this doc is the replacement)",
    kind: "grep-present",
    pattern: /deprecat|retire|replaces[\s\S]{0,200}?manual/i,
  },

  // ─────────────────────── Executable gates ───────────────────────

  {
    id: "exec-go-vet",
    from: "Executable gates",
    description: "go vet passes (catches obvious type errors)",
    kind: "exec",
    command: ["go", "vet", "./..."],
  },
  {
    id: "exec-go-test-hotswap",
    from: "Executable gates",
    description: "go test ./internal/domain/hotswap/... passes",
    kind: "exec",
    command: ["go", "test", "./internal/domain/hotswap/..."],
  },
  {
    id: "exec-go-test-agentops",
    from: "Executable gates",
    description: "go test ./internal/ops/agentops/... passes (the CLI hot-swap path is removed; this covers the remaining agent-job ops)",
    kind: "exec",
    command: ["go", "test", "./internal/ops/agentops/..."],
  },
  {
    id: "exec-go-test-server",
    from: "Executable gates",
    description: "go test ./internal/server/... passes (covers existing endpoints + new apply endpoint)",
    kind: "exec",
    command: ["go", "test", "./internal/server/..."],
  },
  {
    id: "exec-helm-template",
    from: "Executable gates",
    description: "helm template k8s renders (chart still valid)",
    kind: "exec",
    command: ["helm", "template", "glimmung", "k8s"],
  },
];

// ─────────────────────────────────────────────────────────────────────────────
// Runner
// ─────────────────────────────────────────────────────────────────────────────

printHeader();

const results = [];
for (const check of CHECKS) {
  if (check.kind === "exec" && skipExec) {
    results.push({ check, pass: true, skipped: true, evidence: "SKIP_EXEC=1" });
    printResult(results[results.length - 1]);
    continue;
  }
  const result = await runCheck(check);
  results.push(result);
  printResult(result);
}

printSummary(results);
process.exit(results.some((r) => !r.pass) ? 1 : 0);

// ─────────────────────────────────────────────────────────────────────────────
// Dispatch
// ─────────────────────────────────────────────────────────────────────────────

async function runCheck(check) {
  try {
    const result = await dispatch(check);
    return { check, ...result };
  } catch (err) {
    return { check, pass: false, evidence: `error: ${err.message}` };
  }
}

async function dispatch(check) {
  switch (check.kind) {
    case "grep-present":        return await grepPresent(check);
    case "grep-absent":         return await grepAbsent(check);
    case "grep-multi-present":  return await grepMultiPresent(check);
    case "git-diff-empty":      return gitDiffEmpty(check);
    case "file-absent":         return await fileAbsent(check);
    case "exec":                return execCheck(check);
    default: return { pass: false, evidence: `unknown kind: ${check.kind}` };
  }
}

async function grepPresent({ file, pattern }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const m = pattern.exec(content);
  if (!m) return { pass: false, evidence: `pattern not found in ${file}: ${pattern}` };
  const { line } = locate(content, m.index);
  return { pass: true, evidence: `${file}:${line}` };
}

async function fileAbsent({ file }) {
  if (await fileExists(file)) {
    return { pass: false, evidence: `${file} still present but should be deleted` };
  }
  return { pass: true, evidence: `${file}: deleted` };
}

async function grepAbsent({ file, pattern }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const m = pattern.exec(content);
  if (m) {
    const { line, column } = locate(content, m.index);
    return { pass: false, evidence: `${file}:${line}:${column} present but should be absent: ${JSON.stringify(m[0].slice(0, 80))}` };
  }
  return { pass: true, evidence: `${file}: pattern absent` };
}

async function grepMultiPresent({ file, patterns }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const missing = [];
  for (const p of patterns) {
    if (!p.exec(content)) missing.push(String(p));
  }
  if (missing.length) return { pass: false, evidence: `${file}: missing ${missing.length} pattern(s): ${missing.join(", ")}` };
  return { pass: true, evidence: `${file}: all ${patterns.length} patterns present` };
}

function gitDiffEmpty({ paths, base }) {
  const result = spawnSync("git", ["diff", "--quiet", base, "--", ...paths], {
    cwd: repoRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) return { pass: false, evidence: `spawn error: ${result.error.message}` };
  if (result.status === 0) return { pass: true, evidence: `unchanged vs ${base}: ${paths.join(", ")}` };
  if (result.status === 1) return { pass: false, evidence: `MODIFIED vs ${base}: ${paths.join(", ")}` };
  return { pass: false, evidence: `git diff failed (status=${result.status}): ${result.stderr.trim().slice(0, 200)}` };
}

function execCheck({ command, cwd }) {
  const cwdAbs = cwd ? path.join(repoRoot, cwd) : repoRoot;
  const result = spawnSync(command[0], command.slice(1), {
    cwd: cwdAbs,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) return { pass: false, evidence: `spawn error: ${result.error.message}` };
  if (result.status !== 0) {
    const stream = (result.stderr && result.stderr.trim()) || (result.stdout && result.stdout.trim()) || "";
    const tail = stream.split("\n").slice(-3).join(" ¶ ").slice(0, 240);
    return { pass: false, evidence: `exit ${result.status}: ${tail}` };
  }
  return { pass: true, evidence: `exit 0` };
}

// ─────────────────────────────────────────────────────────────────────────────
// Output + helpers
// ─────────────────────────────────────────────────────────────────────────────

function printHeader() {
  const byCategory = new Map();
  for (const check of CHECKS) {
    byCategory.set(check.from, (byCategory.get(check.from) ?? 0) + 1);
  }
  console.log(`apply_test_slot_hot_swap manifest: ${CHECKS.length} checks across ${byCategory.size} categories`);
  for (const [cat, n] of byCategory) console.log(`  ${String(n).padStart(2)} ${cat}`);
  if (skipExec) console.log("  (SKIP_EXEC=1 — exec gates marked PASS without running)");
  console.log("");
}

function printResult(r) {
  const sym = r.skipped ? "SKIP" : r.pass ? "PASS" : "FAIL";
  console.log(`${sym}  ${r.check.id.padEnd(50)}  ${r.check.description}`);
  if (!r.pass || r.skipped) {
    if (r.evidence) console.log(`      ↳ ${r.evidence}`);
  }
}

function printSummary(results) {
  const passed = results.filter((r) => r.pass && !r.skipped).length;
  const skipped = results.filter((r) => r.skipped).length;
  const failed = results.filter((r) => !r.pass);
  console.log("");
  console.log(`${passed}/${results.length} pass${skipped ? `, ${skipped} skipped` : ""}${failed.length ? `, ${failed.length} fail` : ""}`);
  if (failed.length) {
    console.log("");
    console.log("Failing checks:");
    for (const r of failed) {
      console.log(`  ${r.check.id}  [${r.check.from}]`);
      console.log(`      ${r.evidence}`);
    }
  }
}

async function fileExists(rel) {
  try {
    await fs.access(path.join(repoRoot, rel));
    return true;
  } catch {
    return false;
  }
}

async function readRel(rel) {
  return await fs.readFile(path.join(repoRoot, rel), "utf8");
}

function locate(content, index) {
  const before = content.slice(0, index);
  const lines = before.split(/\r\n|\r|\n/);
  return { line: lines.length, column: lines[lines.length - 1].length + 1 };
}

# Test-slot hot-swap

> **Superseded by [`docs/test-slot-deploy-plan.md`](test-slot-deploy-plan.md).**
> The artifact build-and-stream model described here is being replaced by
> deploying the CI-built ACR image to the slot (correct-by-construction, no
> per-artifact build/detection). This doc is retained only until that
> migration's cutover removes it — do not extend the model below.

Build new code, place it on a running test slot, step back. One MCP
call. This doc describes the workflow, the contract shape, and the
guarantees the platform makes.

The workflow replaces the manual `kubectl cp` + `kubectl exec` +
`kill -HUP 1` pattern that previous test-slot iteration required (and
which the `/test` skill in agent harnesses currently documents as the
expected dance). That manual pattern is **deprecated** by this endpoint;
the agent skill should migrate to the new MCP tool. Manual ops still
work for one-off debugging, but should not be the documented dev loop.

## The contract — project-side

Each Glimmung project that opts into test-slot hot-swap declares a
`test_slot_hot_swap` block in its `metadata`. The block has five
sub-contracts plus an optional fidelity classifier; a project enables
whichever ones it needs.

```json
{
  "test_slot_hot_swap": {
    "enabled": true,

    "static": {
      "enabled": true,
      "source": "frontend/dist",
      "target": "/var/run/orchestrator-static-override",
      "build_command": "cd frontend && npm ci && npm run build",
      "pod_selector": "app.kubernetes.io/name=orchestrator",
      "container": "orchestrator",
      "builder_image": "node:20-alpine"
    },

    "backend": {
      "enabled": true,
      "strategy": "supervisor",
      "build_command": "cd backend-go && go build -o /tmp/app ./cmd/app",
      "artifact": "/tmp/app",
      "target": "/var/run/orchestrator-hot/app",
      "health_path": "/healthz",
      "health_port": 8000,
      "pod_selector": "app.kubernetes.io/name=orchestrator",
      "container": "orchestrator",
      "builder_image": "golang:1.26-alpine"
    },

    "fidelity_classifier": {
      "enabled": true,
      "command": "node scripts/classify-tank-test-fidelity.mjs"
    },

    "agent_runner": {
      "enabled": true,
      "source": "agent-runner/dist",
      "target": "/var/run/agent-runner-hot/dist",
      "build_command": "cd agent-runner && npm run build",
      "pod_selector": "tank-operator/session-id",
      "container": "agent-runner",
      "restart": "SIGHUP",
      "builder_image": "node:20-alpine"
    },

    "codex_runner": {
      "enabled": true,
      "source": "codex-runner/dist",
      "target": "/var/run/codex-runner-hot/dist",
      "build_command": "cd codex-runner && npm run build",
      "pod_selector": "tank-operator/session-id",
      "container": "codex-runner",
      "restart": "SIGHUP",
      "builder_image": "node:20-alpine"
    },

    "antigravity_runner": {
      "enabled": true,
      "source": "antigravity-runner/hot",
      "target": "/var/run/antigravity-runner-hot",
      "build_command": "cd antigravity-runner && npm ci && npm run build && rm -rf hot && mkdir -p hot && cp -R dist hot/dist && cp -R ../runner-shared hot/runner-shared && find hot/dist -name '*.js' -exec sed -i 's|\"\\.\\./\\.\\./runner-shared/|\"/var/run/antigravity-runner-hot/runner-shared/|g; s|\"\\.\\./\\.\\./\\.\\./runner-shared/|\"/var/run/antigravity-runner-hot/runner-shared/|g' {} +",
      "pod_selector": "tank-operator/session-id,tank-operator/mode=antigravity_gui",
      "container": "antigravity-runner",
      "restart": "SIGHUP",
      "builder_image": "node:20-bookworm-slim"
    }
  }
}
```

### `builder_image` per artifact kind

Each app declares its own build environment. The build runs in a one-off
Kubernetes Job's init container using exactly the image named here. No
language heuristics, no hardcoded defaults — the contract owns this so
the project's build environment is explicit and reproducible.

For `agent_runner`, `codex_runner`, and `antigravity_runner`, `builder_image`
is **required at contract validation time**, so a missing image is unambiguous
misconfiguration. For `backend` and `static`, `builder_image` is **optional at
validation time** (contracts registered before these kinds joined the apply
endpoint predate the field) but **required at request time** when the apply
endpoint is invoked with that kind. `static` additionally requires
`pod_selector` and `container` at request time. `backend` additionally requires
`pod_selector`, `container`, and `health_port` at request time: it streams a
single executable to the supervisor's hot-artifact file on the matched app
pods, SIGHUPs PID 1, and then polls `http://127.0.0.1:<health_port><health_path>`
inside each pod to confirm the re-exec actually serves — a binary that never
goes healthy fails the swap (`swap_failed`) instead of being reported as
`persisted`.

### `fidelity_classifier`

Projects with runtime-specific hot-swap limits can declare a repo-local
classifier. Glimmung runs this command in the cloned repo before the build
command and appends `--artifact-kind`, `--validation-target`, and `--enforce`.
The command decides whether the requested hot-swap is faithful for targets like
`existing_session`, `new_session`, or `full_runtime`.

When this block is enabled, callers must pass `validation_target`. This is a
project-owned guard, not a generic webapp heuristic: for example, Tank's runner
hot-swap updates already-running session pods, while newly created session pods
boot runner code from the branch image.

## The endpoint

`POST /v1/test-slots/apply-hot-swap` (admin-authenticated).

```json
{
  "project": "tank-operator",
  "slot_name": "tank-operator-slot-1",
  "artifact_kind": "agent_runner",
  "git_ref": "feat/durable-stop-request",
  "validation_target": "existing_session",
  "timeout_seconds": 120
}
```

### Async dispatch + durable finalize + poll

The POST **dispatches the build-and-swap Job and returns immediately** with a
`running` handle (`job_name`) plus an initial history entry — it does not hold a
connection open for the build. The build-and-swap deadline is enforced on the
Job itself (`spec.activeDeadlineSeconds`, default 120s, caller-overridable via
`timeout_seconds`, clamped to a hard 600s); a `ControlPlaneLoopsEnabled`-gated
finalizer records the terminal outcome when the Job completes; and the caller
polls `GET /v1/test-slots/apply-hot-swap/{project}/{job}` until the entry is
terminal.

This replaced the original "endpoint blocks until done" shape. Blocking tied
both the result and the history write to the inbound connection, so a ~30s
client/proxy deadline aborted the request (and, because the same request context
fed the history write, corrupted or dropped the durable record) while the
Kubernetes Job ran on to completion. The async shape removes the long-held
connection entirely: every durable write runs on a request-detached context
(`context.WithoutCancel`), so a client disconnect, proxy deadline, or
orchestrator rollout can never abort it, and the `mcp-glimmung` wrapper turns
dispatch + poll back into a synchronous developer UX without holding any single
HTTP request open for the whole build.

### What the dispatch (POST) does

1. Resolves the active test-slot lease for `project + slot`.
2. Reads the project's `test_slot_hot_swap` contract from metadata.
3. Validates `artifact_kind` is supported and the request-time fields are
   present (`builder_image`, `pod_selector`, `container`, and `health_port`
   for `backend`; `builder_image`, `pod_selector`, and `container` for
   `static`).
4. Resolves the fidelity classifier's diff context. The build Job's shallow
   single-SHA checkout cannot compute a real diff, so glimmung computes the
   changed-file set server-side via the GitHub Compare API
   (`base...git_ref`, merge-base three-dot; `base_ref` defaults to the repo's
   default branch) and passes it to the build container as
   `GLIMMUNG_CHANGED_FILES` / `GLIMMUNG_BASE_REF` / `GLIMMUNG_HEAD_REF`.
5. Dispatches a one-off Kubernetes Job (`activeDeadlineSeconds` = the resolved
   timeout; an `app.kubernetes.io/name=glimmung-apply-hot-swap` label and a
   `glimmung.io/slot-name` label the finalizer joins on):
   - **Init container** uses `contract.<kind>.builder_image`. Clones the repo
     at `git_ref`, runs the optional `fidelity_classifier` command (which now
     sees the real changed-file set), runs `contract.<kind>.build_command`, and
     leaves either a source dir at `/work/source` (static/runner) or, for
     `backend`, the single built executable (`contract.backend.artifact`) at
     `/work/artifact`.
   - **Main container** uses a kubectl-only image and resolves the target pods
     from `contract.<kind>.pod_selector`. For static/runner it tar-streams
     `/work/source` into `contract.<kind>.target` and sends
     `contract.<kind>.restart` to PID 1. **Static differs:** its target is the
     slot's app pods (the `<slot_name>` namespace, not `<slot_name>-sessions`),
     the override dir is cleared before the copy so stale content-hashed assets
     don't linger, and no restart is sent because static assets are served live.
     **Backend differs:** it also targets the slot's app pods, but streams
     `/work/artifact` onto the supervisor's hot-artifact file (`chmod +x`,
     atomic `mv`), SIGHUPs PID 1 so the supervisor re-execs, then polls
     `http://127.0.0.1:<health_port><health_path>` inside each pod until a
     `2xx` — a re-exec that never serves fails the swap.
6. Appends an initial `running` hot-swap history entry carrying the job handle.
7. Extends the lease so the slot survives the full build-and-swap.
8. Returns the structured result with status `running` + the job handle.

### What the finalizer does

A gated, event-driven finalizer watches `glimmung-apply-hot-swap` Jobs (it runs
only in the prod control plane, never in a slot process — same isolation gate as
the run-job watcher). When a Job reaches a terminal condition it:

1. Joins the Job back to its leased slot via the `glimmung.io/slot-name` label.
2. Collects build + swap logs (last 4000 chars each) and classifies the outcome.
3. Appends the **terminal** hot-swap history entry — idempotently, keyed by the
   job handle, so duplicate apiserver events and post-restart re-lists never
   double-record.
4. Re-checks the lease minimum TTL and deletes the Job.

If the dispatching caller is gone, the finalizer still records everything — the
durable lease history is the source of truth.

## The outcome

The history entry's status starts at `running` and is finalized to one of four
bounded terminal values:

- `persisted` — the Job's "complete" condition fired; new code is running in the
  target pod(s).
- `build_failed` — the init container exited non-zero. Build logs in the entry
  surface the failure.
- `swap_failed` — the swap container exited non-zero. Swap logs surface the
  failure.
- `timeout` — the Job hit its `activeDeadlineSeconds` (reason `DeadlineExceeded`)
  before completing.

Poll `GET /v1/test-slots/apply-hot-swap/{project}/{job}` for the latest status;
it returns the durable history entry (`running` until the finalizer records a
terminal value).

## Migrating from the manual kubectl pattern

If you're an agent harness or developer following the previous `/test`
skill instructions:

| Old | New |
|---|---|
| `cd backend-go && go build -o /tmp/app ./cmd/app` | (Glimmung does this in the Job's init container) |
| `kubectl exec -i $pod -- sh -c 'cat > /var/run/.../app' < /tmp/app` | (Glimmung's main container does kubectl-stream) |
| `kubectl exec $pod -- kill -HUP 1` | (Glimmung's main container sends the restart signal) |
| Manual log inspection | Build + swap logs in the response |

This is no longer just a recommendation. The legacy `glimmung-agent
test-slot-hot-swap` CLI that automated this kubectl dance has been
removed, and session pods run with read-only Kubernetes RBAC (a
restricted-git session can't `kubectl cp`/`exec` into a slot at all), so
the gated apply endpoint — which carries its own privilege in a
`glimmung-runs` Job — is the only supported path. That is deliberate: it
routes every slot mutation through CI-gated, health-verified, history-recorded
glimmung, including for `backend`.

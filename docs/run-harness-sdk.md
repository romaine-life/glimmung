# Run-harness SDK

The run-harness SDK is the public Go package tree
`github.com/romaine-life/glimmung/harness/...`. It holds Glimmung's run
contract **as types** so a producing app (spirelens, ambience, …) physically
cannot emit an untyped error, skip the verification verdict, or mislabel a
harness crash as a model failure. It is the sanctioned replacement for the
~600-line hand-rolled `scripts/glimmung-native/lib.sh` fork each consumer app
used to carry.

This document is the SDK's contract. The evidence frame it fills is settled and
documented elsewhere — this SDK is the *producer* that feeds that frame
honestly; it does not rebuild it:

- The verdict is written only by the glimmung-owned `verification_finalize`
  primitive (`internal/server/evidence_gate.go`).
- Terminal-failure attribution lives in `internal/server/terminal_observation.go`
  and `internal/store/store` (`terminalObservationForRun`).
- The typed verification failure block and `suspected_cause` enum live in
  `internal/domain/decision` and `internal/server/completion_api.go`.

## Why a typed SDK

Shell cannot be honest. A `throw` becomes `exit 1` with the real reason stranded
in stderr; a harness crash before the model runs gets blamed on the model at $0
cost, filling `suspected_cause` wrong. The SDK makes "the line Glimmung holds"
compiler-enforced: the only way to fail is to return a typed error whose layer
decides attribution, and the only way to produce a verdict is to write the
finalizer's `artifacts/verification.json`.

## Packages

### `harness/step` — the spine

- `Handler interface { Slug() string; Run(*Context) (Result, error) }`, a
  `Registry`, and `func Main(*Registry)`.
- `Main` reads `GLIMMUNG_STEP_SLUG`, builds a typed `Context` **once** from the
  `GLIMMUNG_*` environment (a missing required wire path is a single typed
  `*LayeredError{Layer: harness}`, never a mid-run crash), runs the handler
  under panic recovery, translates the outcome into the runner's wire shapes,
  and calls `os.Exit` itself.
- `Context` exposes typed accessors (`RunID`, `RunRef`, `Project`, `Workflow`,
  `Phase`, `JobID`, `StepSlug`, `IssueRepo`, `IssueNumber`, `AttemptIndex`,
  `WorkingDir`) and declared `Input(name)` / `RunInput(name)` that **fail
  closed** on a missing required value (env-name normalization mirrors the
  launcher's `GLIMMUNG_INPUT_*` / `GLIMMUNG_RUN_INPUT_*`). Output emitters
  `EmitOutput` / `EmitJSONOutput` have behavioral parity with the apps'
  `native_emit_output` / `native_emit_json_output`; `Abort(reason)` writes the
  key `decision.AbortReasonOutputKey` — literally the runner's key — exactly as
  `native_emit_abort`.
- `LayeredError{Layer, Subcommand, Code, Message, Retryable, Cause}` with
  `Layer ∈ {harness, host, model}`. Returning it is how attribution happens; the
  layer maps onto the existing `suspected_cause` enum (see `steperr`).

#### Honest exit contract

| Outcome | Exit | Wire effect |
| --- | --- | --- |
| success | `0` | emitted `Result` (outputs + optional `summary_markdown`) |
| `Abort(reason)` | `0` | `abort_reason` in `GLIMMUNG_OUTPUT_FILE` → runner routes to teardown-then-abort |
| `*LayeredError` / panic / other error | non-zero | typed `error{layer,code,message}` block in `GLIMMUNG_COMPLETION_FILE` + real message to stderr |
| unknown slug | `2` | typed completion naming the unknown slug |

### `harness/agent`

- `Invoke(ctx, Spec) (Outcome, *step.LayeredError)` runs an agent CLI as a child
  process, streaming its stdout **line-unbuffered** to the runner's log stream so
  `internal/domain/agentcost.FromJSONLogLine` prices each `usage` line exactly as
  today (regression guard against the $0-cost bug).
- This is the **only** place a `Layer: model` error may originate. `Outcome.Ran`
  reports whether the model ran; it never carries a verification verdict.

### `harness/verification`

- `Verification` is the typed `artifacts/verification.json` document in the exact
  shape `evidence_gate.go`'s finalizer reads (`status` / `reasons` / `failure` /
  `abort_reason` / `evidence_refs` / `evidence` / `evidence_results` / `notes`).
- `Verification.Extra` (`map[string]json.RawMessage`) is the producer-domain
  passthrough. The finalizer reads its known fields and **preserves every other
  top-level key**, so a consumer with a richer verdict (spirelens carries
  `unit_tests` / `live_mcp_validation` / `screenshot_validation` rows; ambience
  its own) carries those keys through the SDK writer verbatim — feeding the
  recycle context and human review intact — **without forking the writer**. The
  custom marshaller emits the typed known fields, then merges `Extra`; an `Extra`
  key that shadows a typed field (a reserved finalizer key like `status`) is a
  marshal error, so the typed field is always authoritative and the document can
  never carry two conflicting copies of a finalizer-read key. `Unmarshal` is the
  inverse: known keys decode typed and validated, everything else lands in
  `Extra`.
- `WriteFinalizable(workingDir, v)` validates the typed known fields and lands
  `${GLIMMUNG_WORKING_DIR}/artifacts/{verification.json,screenshots,evidence}`
  (the validation gate runs regardless of `Extra`).
- `Gate(workingDir, check)` runs a deterministic check before the agent: on a
  non-pass verdict it writes the verdict and returns `proceed=false`, so a
  deterministic failure never spends model tokens. The verdict is still finalized
  by the glimmung-owned primitive — the SDK never writes the completion-file
  `verification` verdict itself.

### `harness/evidence`

- Boundary-exact matchers ported from spirelens `EvidenceContract.ps1`:
  `ExpectedTextMatch` (`vigor gained: 8` ≠ `88`) and `GameStateJSONContainsID`
  (`CLOAK_CLASP` ≠ `CLOAK_CLASP_PLUS`). Go's RE2 has no lookaround, so the
  boundary checks scan explicitly; the ported Pester cases prove parity.
- `ObservedUnitTestResult` / `ParseTRX` port `UnitTestResult.ps1`: a verdict of
  `exit==0 && failed==0`, enumerated failing rows trumping a stale counter, and
  synthetic fallback for a missing/unparseable TRX.

### `harness/remotehost`

- Typed port of `lib.sh`'s remote-host venue: `MintAndConnect(ctx, cfg, hostTag)`
  (ssh-cert + tailscale-authkey mint via `GLIMMUNG_SSH_CERT_URL` /
  `GLIMMUNG_TAILSCALE_AUTHKEY_URL` with `X-Glimmung-Attempt-Token`; userspace
  `tailscaled`; `tailscale nc` ssh ProxyCommand), `Conn.RunSelf(ctx, subcmd,
  args...)` (ssh-run the same binary's host face, replacing the pwsh-over-ssh
  here-doc), and `ScpPull` / `ScpPullTree` / `ScpPush` / `ScpPushTree` /
  `SyncCheckout`.
- Host-unreachable / venue-setup failure → `Layer: host`.

- **`RunSelf` streams the remote process's stdout/stderr line-unbuffered.** When
  the agent runs ON the remote host (the laptop venue), its `usage` lines are
  emitted host-locally by `harness/agent.Invoke`; `RunSelf` forwards each
  newline-terminated line to the pod step's stdout as a single write the instant
  it arrives (and stderr likewise), so the runner's
  `internal/domain/agentcost.FromJSONLogLine` prices each one across the ssh hop.
  This is the honesty guarantee restored end to end — a swallowed or buffered
  remote stdout would defeat the $0-cost misattribution guard at the hop. ssh
  propagates the remote exit status, so a non-zero remote exit is surfaced as a
  host-layer error (the honest exit-code contract is preserved). `Config.Stdout`
  / `Config.Stderr` default to `os.Stdout` / `os.Stderr`; set them only to
  capture the stream in tests.

- **`MintAndConnect` is step-idempotent within a pod.** A prepare phase runs
  several steps in one pod, each a fresh process sharing the working dir, so a
  per-step call converges on the existing venue instead of rebuilding it: the
  ed25519 keypair is generated once and reused while on disk, `tailscaled` is
  started (and the node brought up, with a fresh authkey) only when no daemon is
  already serving the pod's socket (`BackendState == "Running"`), and only the
  short-TTL ssh certificate is re-minted every call. Re-keygen would invalidate
  the live cert; a second `tailscaled` on the same socket would conflict.

- **`ScpPush` (single file) / `ScpPullTree` (recursive)** complete the matrix
  alongside `ScpPull` (single file) / `ScpPushTree` (recursive): a consumer can
  stage one file without a one-file directory wrapper and pull a directory of
  evidence without packing it into an archive host-side.

### `harness/runcallbacks`

- `Config.MintGitHubToken(ctx)` mints the per-attempt GitHub token from the run
  callback Glimmung bakes onto the pod (`GLIMMUNG_GITHUB_TOKEN_URL`,
  authenticated with the `X-Glimmung-Attempt-Token` header; `FromContext` reads
  both from a `step.Context`). It is the venue-independent companion to
  `remotehost`'s ssh-cert / tailscale-authkey mints — an in-cluster consumer
  needs the token without importing the remote-host venue, so it lives in its own
  package. The callback is the only sanctioned source of the token; a runner Job
  never mounts the real provider OAuth secret. Layering is honest: a missing wire
  (URL / attempt token) or a malformed response is `Layer: harness`; an
  unreachable or error-status callback is `Layer: host`. `HTTPClient` is
  injectable for testing.

## Shared wire type

`internal/domain/steperr.Block{Layer, Code, Message}` is the canonical typed
step-error wire shape, shared by the SDK, the runner (`cmd/glimmung-runner`), the
completion API, and the store. One definition to encode and decode is what stops
the SDK and runner from drifting. `Layer` maps onto `suspected_cause`:
`harness → harness_flake`, `host → environment_config`, `model → code_bug`.

## Additive step-error attribution (runner + completion)

A non-verification producer step crash used to terminal-attribute as a generic
"exited with code N". The runner now reads an optional typed
`error{layer,code,message}` block from `GLIMMUNG_COMPLETION_FILE` on step
failure and carries it onto the `step_failed` event and the `/completed`
request; the completion API threads it onto the failing job completion and the
store's `terminalJobFailureCause` promotes the typed message as the terminal
cause (with the layer in the message) when no verification verdict supplied one.
**Completions without the block behave byte-for-byte as before.**

## Migration guard

`harness/step` carries `TestSDKIsTheOnlySanctionedStepProducerSurface`, the seed
guard the later app slices extend: it fails if a retired `native_emit_*` shell
producer fork reappears inside the glimmung module tree. The SDK is the only
sanctioned step-producer surface.

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
- `WriteFinalizable(workingDir, v)` validates and lands
  `${GLIMMUNG_WORKING_DIR}/artifacts/{verification.json,screenshots,evidence}`.
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
  here-doc), and `ScpPull` / `ScpPushTree` / `SyncCheckout`.
- Host-unreachable / venue-setup failure → `Layer: host`.

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

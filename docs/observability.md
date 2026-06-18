# Observability

Glimmung exposes a Prometheus `/metrics` endpoint on the existing app port,
alongside `/healthz`. The metric surface is domain-shaped — it describes
runs, decisions, leases, hot-swap outcomes, and the HTTP layer — not just
generic Go runtime data. The contract here is the names, labels, and
cardinality budget; treat changes to them like API changes.

## Endpoint

- Path: `GET /metrics`
- Port: same as the rest of the app (`8000`)
- Format: Prometheus text exposition + OpenMetrics
- Auth: none (same surface as `/healthz`; relies on in-cluster network
  scope, not on per-request auth)

The endpoint is served from
[`internal/metrics`](../internal/metrics/metrics.go) by a package-private
registry — the default Prometheus global registry is intentionally
unused so the metric set is explicit and testable in isolation.

## Metric families

All glimmung metrics are namespaced `glimmung_*`. Default Go runtime and
process collectors are also exposed (`go_*`, `process_*`).

### HTTP layer

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_http_requests_total` | counter | `pattern`, `method`, `status` | `pattern` is the Go 1.22+ `ServeMux` registered route (e.g. `GET /v1/runs/dispatch`), never raw URL. Unmatched requests get `pattern="unmatched"`. |
| `glimmung_http_request_duration_seconds` | histogram | `pattern`, `method` | Default Prometheus buckets. |

### Verify loop

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_decisions_total` | counter | `decision` | One increment per well-formed `decision.Decide()` return. Values: `retry`, `advance`, `abort_budget_attempts`, `abort_budget_cost`, `abort_malformed`. |
| `glimmung_budget_breaches_total` | counter | `reason` | Increments when `Decide()` returns an abort caused by budget. Values: `cost`, `attempts`. |

### Runs

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_runs_created_total` | counter | `workflow` | One per successful `CreateRun` at dispatch. |
| `glimmung_run_terminal_total` | counter | `class`, `state` | One increment per run reaching a terminal state, emitted at the guarded terminal-write choke point. See _Run terminal attribution backstop_ below. |

Terminal-state run histograms (duration, attempt count, cumulative cost)
are intentionally out of scope for V1 — they need the run's
creation timestamp and cost data plumbed into `SetRunTerminalState`
callers, which is a separate plumbing change. Terminal *decisions* remain
fully observable via `glimmung_decisions_total`.

#### Run terminal attribution backstop

`glimmung_run_terminal_total` is the runtime backstop for the platform
invariant: _no run may settle terminal-failed without an attributed cause._
Slices 1–3 make every terminal failure carry a typed observation, a failed
owner step, and an inspector that renders it; this metric is the alarm that
fires if that invariant is ever violated in production, so an operator is paged
instead of left staring at a blank failed run.

- **Emission site.** Incremented exactly once per run reaching a terminal
  state, at the same terminal-write choke points where
  `server.GuardTerminalFailureObservation` runs:
  `Store.SetRunTerminalState` (completion-callback settle, passed or aborted)
  and `Store.AbortRunByID` (admin abort). Both are genuine terminal
  transitions a run passes through exactly once (the admin path returns early
  when the run is already terminal), so a run that settles once increments
  once. `Store.RepairRunTerminalObservation` re-derives an already-terminal
  run's observation — it is **not** a new transition and deliberately does not
  increment, to avoid double-counting.
- **`class` label.** The guarded observation's class — the closed
  `RunTerminalObservation` enum (`producer_phase_failed`,
  `verifier_contract_missing`, `verifier_failed`, `gate_failed`,
  `dispatch_failed`, `phase_requested_abort`, `manual_abort`,
  `malformed_terminal`), plus `none` (a passed run carries no failure
  observation) and `unknown` (empty-class sentinel). Derived through
  `server.TerminalObservationMetricClass`, and reflects the **post-guard**
  class — an unresolved attribution reads as `malformed_terminal`, never the
  raw pre-guard value.
- **`state` label.** The terminal run state: `passed` or `aborted` today
  (`failed` is reserved by the same `TerminalFailureState` enum the guard
  tolerates).
- **`phase` is deliberately NOT a label.** Phase names are
  workflow-author-defined, not a closed enum, so they are an unbounded-
  cardinality risk. Phase — plus the job/step owner identity — lives in the
  structured drill-down log instead (`run settled terminal with unattributed
  cause`, fields `project`/`issue`/`run`/`run_id`/`cycle`/`phase`/`job`/`step`/
  `class`/`state`), per the observability contract's no-unbounded-labels rule.
- **Failure mode.** `class="malformed_terminal"` or `class="unknown"` on a
  terminal failure means a run settled without a resolvable cause — the
  invariant has been violated. The `GlimmungRunTerminalUnattributed` alert
  pages on this (count/rate > 0); the co-located structured log is the
  drill-down. If the metric itself stopped incrementing entirely, terminal
  settles would still be observable via `glimmung_decisions_total` and the
  run's durable `terminal_observation`, but the unattributed-failure alarm
  would go dark — so the wire-contract test in `internal/metrics` pins the
  metric's presence to catch accidental de-registration.

##### Alert rule

The alert ships in this chart as a `PrometheusRule`
(`k8s/templates/prometheusrule.yaml`), gated on
`observability.prometheusRule.enabled` (default off, mirroring the
ServiceMonitor gating so a cluster without the
`monitoring.coreos.com/v1` CRD still installs cleanly):

```yaml
- alert: GlimmungRunTerminalUnattributed
  expr: sum(increase(glimmung_run_terminal_total{class=~"malformed_terminal|unknown"}[15m])) > 0
  for: 0m
  labels:
    severity: critical   # unattributed terminal failures page operators
    service: glimmung
  annotations:
    summary: "glimmung run settled terminal with an unattributed cause"
```

Intended severity/routing: **critical / page**. Whether the central Prometheus
actually evaluates this rule depends on that Prometheus CR's `ruleSelector` /
`ruleNamespaceSelector`, which is owned by the cluster monitoring stack
(infra-bootstrap), **not** by this chart. Set
`observability.prometheusRule.labels` to match that selector, or — if cluster
policy is that alert rules live centrally — lift the expression above into the
monitoring repo instead. The metric is emitted regardless of where the rule is
evaluated.

### Leases

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_leases_acquired_total` | counter | `purpose`, `outcome` | `purpose` is one of: `dispatch`, `test_slot_checkout`. `outcome` is `granted`, `conflict`, or `error`. |
| `glimmung_leases_released_total` | counter | `purpose`, `outcome` | `outcome` is `cancelled` (admin), `expired` (TTL fired), or `completed` (consumer release). |
| `glimmung_leases_held` | gauge | `purpose` | Approximate. In-process delta of acquire/release; authoritative state lives in Postgres. Per-purpose breakdown can drift because release sites do not always know the original acquire purpose; the total across purposes is correct. |
| `glimmung_lease_acquire_wait_seconds` | histogram | `purpose`, `outcome` | Exponential buckets from 10ms to ~40s. |

### Hot-swap

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_hot_swap_outcomes_total` | counter | `outcome` | One per `ApplyHotSwap` invocation. Values: `persisted`, `build_failed`, `swap_failed`, `timeout`. |
| `glimmung_hot_swap_duration_seconds` | histogram | `outcome` | Exponential buckets from 1s to ~17 min. |

## Cardinality

Every label is either a closed enum (`decision`, `outcome`, `verification`,
`purpose`, `reason`, `class`, `state`) or a registered identifier
(`workflow`, `pattern`). Raw user input — issue numbers, project slugs, repo
URLs — never lands in a label. Notably, run terminal `phase`/`job`/`step`
identifiers are kept OUT of `glimmung_run_terminal_total`'s labels (phase
names are workflow-author-defined, hence unbounded) and live in the structured
log instead. To add a new label value:

- For HTTP `pattern`: register the route in
  [`internal/server/server.go`](../internal/server/server.go); the
  middleware reads `r.Pattern` directly so new routes are picked up
  automatically.
- For `decision`: add a constant in
  [`internal/domain/decision/decision.go`](../internal/domain/decision/decision.go);
  the deferred recorder picks it up.
- For lease `purpose`: add a constant to the `LeasePurpose*` set in
  [`internal/server/lease_api.go`](../internal/server/lease_api.go) and use
  it at the new acquire site.
- For hot-swap `outcome`: only the four named modes; adding one means
  updating both the Go path and the migration-check pattern in
  [`scripts/check-apply-test-slot-hot-swap-migration.mjs`](../scripts/check-apply-test-slot-hot-swap-migration.mjs).
- For run terminal `class`: add a constant to the
  `RunTerminalObservation` class enum in
  [`internal/server/terminal_observation.go`](../internal/server/terminal_observation.go);
  `server.TerminalObservationMetricClass` then carries it onto the metric. The
  `none` and `unknown` sentinels are reserved for passed runs and empty-class
  coercion respectively.

Empty label values are coerced to `unknown` to prevent operators from
staring at blank label rows in Grafana.

## Deployment

Glimmung's Helm chart ships an opt-in ServiceMonitor, pod scrape annotations,
and a PrometheusRule (the run-terminal-attribution alert). All default off so
installs work cleanly in clusters that do not run the Prometheus Operator.

```yaml
# k8s/values.yaml — override as needed
observability:
  serviceMonitor:
    enabled: true                # require monitoring.coreos.com/v1 CRD
    interval: 30s
    scrapeTimeout: 10s
    namespace: ""                # default: chart namespace
    labels:
      release: kube-prometheus   # any selector your Prometheus CR uses
  scrapeAnnotations:
    enabled: true                # pod-level prometheus.io/* annotations
  prometheusRule:
    enabled: true                # require monitoring.coreos.com/v1 CRD
    namespace: ""                # default: chart namespace
    labels:
      release: kube-prometheus   # match the Prometheus CR's ruleSelector
    window: 15m                  # GlimmungRunTerminalUnattributed window
    severity: critical           # Alertmanager routing label
```

The ServiceMonitor selects the `glimmung` Service by its `app: glimmung`
**label** (Prometheus Operator matches `ServiceMonitor.spec.selector` against
Service `metadata.labels`, not the Service's pod selector), so that label lives
on the Service in `templates/service.yaml`. The endpoint scrapes the Service's
`http` port (port 80 → `targetPort: http` → the app container's `:8000`, where
`/metrics` is always served), path `/metrics`.

In **this** cluster the prod `k8s/values.yaml` enables both
`serviceMonitor` and `prometheusRule` and sets **no** `labels`: the cluster
Prometheus (`monitoring/monitoring-kube-prometheus-prometheus`) ships empty
match-all `serviceMonitorSelector`, `serviceMonitorNamespaceSelector`,
`ruleSelector`, and `ruleNamespaceSelector`, so both resources are discovered
in glimmung's namespace without a `release:` selector label. The `labels`
override above is only needed on a cluster whose Prometheus CR uses a
non-empty selector.

The per-issue chart at [`k8s/issue/`](../../k8s/issue/) ships the
`scrapeAnnotations` toggle but no ServiceMonitor — per-issue releases are
ephemeral and not normally scraped by central Prometheus. The `/metrics`
endpoint is always live on the pod for ad-hoc `kubectl port-forward`
debugging.

## Operating heuristics

These are the metrics that answer the operator-facing questions glimmung's
shape implies. They are not (yet) wired to alerting rules — alert
configuration belongs with the Prometheus deployment, not with glimmung's
chart.

| Question | Query |
|---|---|
| How many runs are dispatched per hour, by workflow? | `sum by (workflow) (rate(glimmung_runs_created_total[1h])) * 3600` |
| What fraction of attempts are passing on first try? | `1 - (sum(rate(glimmung_decisions_total{decision="retry"}[1h])) / sum(rate(glimmung_decisions_total[1h])))` |
| Is the cost ceiling firing? | `sum by (workflow) (rate(glimmung_budget_breaches_total{reason="cost"}[1h]))` |
| Hot-swap success rate (last 6h)? | `sum(rate(glimmung_hot_swap_outcomes_total{outcome="persisted"}[6h])) / sum(rate(glimmung_hot_swap_outcomes_total[6h]))` |
| Are leases stacking up? | `sum by (purpose) (glimmung_leases_held)` |
| AcquireLease p99 wait time? | `histogram_quantile(0.99, sum by (le, purpose) (rate(glimmung_lease_acquire_wait_seconds_bucket[5m])))` |
| HTTP error rate by route? | `sum by (pattern) (rate(glimmung_http_requests_total{status=~"5.."}[5m]))` |

## Out of scope (V1)

The V1 cut deliberately stops short of the following — each is a follow-up
PR with its own data-plumbing requirement:

- **Run histograms** (`run_duration_seconds`, `run_attempts`,
  `run_cost_usd`): require the run's created-at timestamp and cumulative
  cost at every `SetRunTerminalState` caller. Plumbing them through is a
  refactor, not a metric addition.

### Why a Watch, not a poll

The cluster-wide runner-Job Watch in `internal/server/run_watcher.go`
is glimmung's **primary** detection path for terminal `batch/v1.Job`
events. A single persistent HTTP connection to the kube-apiserver
streams events for Jobs labelled
`app.kubernetes.io/managed-by in (glimmung, glimmung-inner)`. On a
`MODIFIED` event with `condition=Complete=True` or
`condition=Failed=True` the handler dispatches into the existing
synthesis path within ~200ms of the kubelet stamping the condition.

The 30s polling reconciler that originally caught these events has
been relaxed to a 1h cadence as belt-and-braces. Sustained non-zero
`glimmung_run_reconciler_caught_total` is the alert signal that the
Watch is dropping events. The disconnect duration is observable via
`glimmung_run_watch_disconnected_seconds`; event flow via
`glimmung_run_watch_events_total{kind, action}`.

This shape matches the rest of glimmung's event-driven model. The
answer to "why did this transition fire at time T" is now grounded in
event data — "apiserver pushed event UID=X resourceVersion=Y at
T-200ms" — instead of "the 30s tick happened to hit."

- **k8s Job apply/terminal metrics** (wired): the run-execution
  reconciler and the runner-driven completion callback site are now
  the two callers that emit
  `glimmung_run_phase_job_terminal_total{conclusion, reason}`. Reason is
  the closed enum `JobTerminalReason*` from `internal/server`
  (`deadline_exceeded` / `backoff_exceeded` / `pod_gone` /
  `callback_lost` / `job_failed` / `verification_failed` /
  `verification_error` / `timeout` / `cancelled` / `unknown` / empty
  for success); callers normalise through `server.NormalizeJobTerminalReason`
  so unexpected strings collapse to `unknown`. The companion
  `CompletionPayload.TerminalReason` also stamps
  `RunJobExecution.Reason` on the run report so the dashboard shows
  the precise failure mode instead of a generic conclusion. Apply-side
  counters remain deferred — the lease metric still covers the queue
  side.
  Inner Jobs spawned by phase scripts (see
  `docs/inner-job-observation.md`) are covered separately by
  `glimmung_run_inner_jobs_registered_total{intent}`.
- **OpenTelemetry traces**: separate decision; metrics alone cover the
  operator questions above.
- **Per-metric alert rules**: belong with the Prometheus deployment, not
  with the chart that exposes the metrics.

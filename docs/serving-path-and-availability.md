# Serving Path & Availability

Status: Stage 1 (incident hardening) implemented; Stages 2–5 planned.

This document is the durable plan for Glimmung's HTTP serving path and its
availability posture. It follows [`quality-timeframes.md`](quality-timeframes.md)
(heavy is the default; write the full plan first, stage it so each step is
coherent) and [`migration-policy.md`](migration-policy.md) (retired paths are
deleted, not kept as fallbacks). It exists because a production outage exposed
that Glimmung's durable core is sound but the *serving and availability tier*
around it was not held to the same bar.

## The incident (why this doc exists)

A burst of dashboard traffic took the whole site down for ~9 minutes:

- The run/issue **graph endpoints** recompute the full graph from normalized
  tables on every request (~12 Postgres queries each, including a per-run
  workflow-schema N+1 and a `ListReviews` that re-reads all project runs and
  issues). There is no read model and no cache.
- The dashboard **polls** those endpoints every 3s with three parallel requests
  and **no backoff**, so a handful of open tabs drove ~3–5 req/s → ~25–35
  queries/s.
- `glimmung-pg` is a **single burstable `B1ms`** instance; sustained load
  throttled it to baseline CPU and p95 query latency rose ~10×.
- `/readyz` ran a **synchronous `SELECT … FROM projects`** on every probe with
  the kubelet's default **1s** timeout. Under load the probe timed out, and with
  **`replicas: 1`** the sole pod was marked NotReady and pulled from the Service
  → gateway `504`. Failed dashboard requests retried at the same fixed cadence,
  sustaining the saturation until traffic happened to ebb.

No alert fired; it surfaced as a user-reported `504`.

## Principles (the lens)

- **Durable state is the source of truth.** Postgres stays canonical. This plan
  does not introduce a second datastore or revisit the Cosmos→Postgres
  migration; it makes the *read/serving path* worthy of that durable core.
- **Live transport wakes clients; it does not own product state.** Reconnect
  resumes from a durable cursor; an unknown cursor forces an explicit resync.
  Full-graph polling violates this and is a deletion target (Stage 3).
- **Observable systems over "it probably works."** A slow-burn saturation must
  page before it is an outage.
- **Availability is a property of the tier, not of one pod.** A stateless
  serving tier runs plural; a single replica is a latent outage.

## Target architecture

### 1. Availability — plural, leader-split serving tier
The serving tier is stateless and runs `≥3` replicas, spread across nodes, with
a PDB that survives a rolling drain. Control-plane loops (reconcilers, the
cluster Job watch, lease dispatch) run under a **single owner**
(`CONTROL_PLANE_LOOPS_ENABLED`) so scaling the API does not multiply background
DB load or duplicate watches. The DB connection budget already assumes this
(`tofu/postgres.tf`: `MaxConns=6 × 3 = 18`, under `B1ms` ~50 `max_connections`).

### 2. Readiness degrades requests, not the pod
`/readyz` reads a **cached readiness gauge** maintained by a background probe
with hysteresis — never a synchronous per-request `SELECT`. A brief read-store
slowdown degrades individual requests (bounded timeout → 503 for that request)
while the pod stays in rotation. Liveness never depends on the DB.

### 3. Bounded, cached read path
The graph is served from a **materialized/cached read projection** invalidated
by the event ledger, so a graph read is `O(1)` indexed reads, not `O(runs)`.
The N+1 and redundant full-table re-reads are removed. Every statement is bounded
server-side (`statement_timeout`) and per-request (handler context deadline).

### 4. Event-driven live updates from a durable cursor
The dashboard subscribes (SSE — `/v1/events` already exists) and receives
**deltas keyed to a durable cursor** (`run_events` already carries the sequence;
`afterSeq` is currently unused). A wake event fetches only the changed run/cycle.
Reconnect resumes from the last cursor; an unknown cursor forces a full resync.
The frontend gains request coalescing and backoff+jitter. **When this lands, the
3s full-graph poll is deleted end to end** — not kept as a fallback.

### 5. Bounded, observable, available database
Read-path load is bounded (3–4) *and* the tier is sized/HA'd deliberately rather
than implicitly. `pgxpool` saturation, read-store readiness, query latency, and
query errors are exported and **alerted**. Postgres server-side metrics are
scraped so the DB is not a black box.

## Stages (each a coherent PR; full plan above)

| Stage | Scope | Prevents/Addresses |
|---|---|---|
| **1 — incident hardening (this PR)** | HA chart, readiness decoupling, `statement_timeout`, pool + read-store metrics, alerts | The exact outage chain + its detection gap |
| 2 — read-path efficiency | kill N+1/redundant reads, bound the run-cycle query, issue/run-cycle read cache + invalidation | Per-request query amplification |
| 3 — event-driven dashboard | SSE deltas on the `run_events` cursor; frontend coalescing + backoff; **delete the 3s poll** | Polling load + retry-storm amplification |
| 4 — control-plane/serving split | leader-elected loops; serving replicas loop-free | Background DB load multiplying with replicas |
| 5 — DB capacity + observability | measured HA/right-size decision; Postgres server metrics; read-path SLOs | Implicit, unalarmed DB capacity |

Stages 1–2 would have prevented this outage; 3–5 make it not the default failure
mode. The only things being retired are the synchronous readiness probe (Stage 1)
and the full-graph poll (Stage 3) — both deleted, per migration policy.

## What Stage 1 implements (this PR)

- **Readiness decoupled from the hot path.** `internal/server/read_store_health.go`
  adds `StoreHealthMonitor`: a single background goroutine probes the read store
  on its own bounded context with hysteresis (flips NotReady only after
  `FailureThreshold` consecutive failures; recovers after `SuccessThreshold`).
  `/readyz` reads `Ready()` in O(1) with **no database I/O**. The synchronous
  `readStoreReady`/per-request `SELECT` is **deleted**.
- **Statement backstop.** `internal/store/pg` sets a server-side
  `statement_timeout` (default 60s) on every pooled connection so a pathological
  query cannot pin a connection indefinitely. It is a runaway backstop;
  per-request deadlines (Stage 2) own the tight read-path budget.
- **Observability.** New metrics: `glimmung_read_store_ready` (cached gauge),
  `glimmung_read_store_probe_total{outcome}`, and `glimmung_pg_pool_conns{state}`
  / `glimmung_pg_pool_acquire_total{kind}` (pgxpool saturation). New
  `PrometheusRule` group `glimmung-read-store`: `GlimmungReadStoreNotReady`
  (pages), plus warning-level latency, query-error, and pool-saturation alerts.
- **Availability.** Chart `replicas: 1 → 3`, soft node `topologySpread`, explicit
  probe `timeoutSeconds`/`failureThreshold`, and an HA-aware PDB
  (`maxUnavailable: 1`, meaningful only with a plural tier).

### Stage 1 done standard
- `/readyz` performs no per-request database query (verified:
  `internal/server/read_store_health_test.go`, `internal/server/server_test.go`).
- A transient read-store blip under `FailureThreshold` does not flip readiness
  (verified: `TestStoreHealthMonitorTransientFailureDoesNotFlip`).
- The chart renders a `≥2`-replica, node-spread, HA-PDB deployment
  (`helm template`).
- The read-store failure mode is alarmed on metrics that exist
  (`glimmung-read-store` rule group).

## Feature Contracts

- **Observability And Evidence** — new read-store/pool metrics + alert rules;
  cardinality bounded (`outcome`, `state`, `kind` are closed enums). Evidence:
  `internal/metrics` tests pass; `helm template` renders the rule group;
  `scripts/check-503-observability.mjs` clean.
- **Auth And API Surface** — `/readyz` contract preserved (200 ready / 503
  not-ready / 503 not-configured) with the synchronous probe retired. Evidence:
  `internal/server` tests; `route_inventory_test.go` still lists `GET /readyz`.
- **Issues And Runs / Workflow Execution / Test Slots / Review Surfaces /
  Dashboard And Styleguide** — not changed in Stage 1 (read-model, poll
  deletion, and graph-query fixes are Stages 2–3).

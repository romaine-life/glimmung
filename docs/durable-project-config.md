# Durable Project Configuration

Project configuration is **durable state in Postgres** — the `projects` row is
the single source of truth at runtime. Authored config is versioned immutably;
server-reconciled status is a separate, reconciler-owned column.

## Source of truth

- Postgres `projects` owns project/slot configuration: count, DNS, Helm,
  workload identity, hot-swap metadata, and per-project agent-runtime policy.
- A config write replaces *authored* config and mints a new immutable version
  in `project_config_schemas` (content-hash `config_schema_ref`),
  transactionally, **without touching the reconciler-owned `status` column**.
- Server-reconciled status (`managed_auth_origin_status`,
  `runner_standby_workload_identity_status`) lives in `projects.status`, never
  in authored config. Reads merge it back under `Metadata` so the API/frontend
  shape is unchanged.

## Authoring — how config gets written

Project config is written through the Glimmung control plane (Postgres is the
source of truth; there is no repo-file source):

- `register_project` (`POST /v1/projects`) — upserts authored config. It
  **replaces authored config wholesale**, so always send the *complete* authored
  metadata; it never touches the status column. A caller/agent reconstructs the
  full payload before writing.
- Dedicated authored-config setters (`SetTestEnvironmentCount`, the TTL setters,
  …) mutate a single field and re-version.

Authored config = `{name, github_repo, metadata}` with the server-managed
`*_status` keys removed. The content hash is computed over a canonicalized form
of that document (stable key order).

## Removed: file-based config sync

An earlier iteration reconciled a repo-committed `.glimmung/project.yaml` into
the durable row — `POST /v1/projects/{project}/sync` + `GET …/upstream`, the
`sync_project` / `check_project_updates` MCP tools, and a per-repo
`project-config-reconcile` workflow.

That path is **removed**. A config file in git plus a config row in Postgres are
two sources of truth that **drift**, and the durable row is already the
authority. Edit project config directly through the control plane (above) — not
a committed repo file. Do not reintroduce a repo-file source or a sync/upstream
reconcile path.

## The durable substrate (kept)

`projects` carries `config_schema_ref` (pointer to the current authored-config
version) and `status` (reconciler-owned). `project_config_schemas` holds every
authored-config version immutably, keyed `(name, schema_ref)`. `UpsertProject`
mints the version row, moves the pointer, and replaces the authored payload in
one transaction, leaving `status` untouched. This is what makes the Postgres row
a trustworthy single source of truth.

### Who runs migrations: only the prod control plane

Glimmung test slots run a hot-swappable copy of the binary against the **same
Postgres database** as prod. The startup DB mutators (`RunMigrations`,
`BackfillConfigSchemas`, and the slot-storage migration) are gated behind
`Settings.ControlPlaneLoopsEnabled`, so only the prod control plane mutates the
shared schema; slots serve HTTP against the prod-owned schema. A consequence: a
new migration cannot be validated in a slot — new-migration changes are
validated by unit tests + CI and land atomically at prod rollout, where the same
process both migrates and reads the new shape under the advisory lock.

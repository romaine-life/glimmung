package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkflowsStore is the Postgres-backed workflows + workflow_schemas store.
// Workflow registrations and immutable schema snapshots live in separate
// tables.
type WorkflowsStore struct {
	pool *pgxpool.Pool
}

// WorkflowRow is the per-project, per-name workflow row. Payload stores the
// phase graph, PR policy, budget, metadata, and other workflow fields as jsonb
// so this package does not reimplement every sub-type marshaler.
//
// ControlPins is the operator-owned column (parallel to projects.status):
// registration writes never touch it — only UpdateControlPins does — and
// registration enforces pinned values into the authored payload before the
// schema hash is computed.
type WorkflowRow struct {
	Project     string
	Name        string
	SchemaRef   string
	Payload     []byte // raw workflow JSON payload
	ControlPins []byte // raw operator pin JSON document
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// WorkflowSchemaRow is the immutable workflow-schema row keyed by
// (project, schema_ref). Schemas accumulate over time; the same
// workflow re-registered with a different shape gets a new schema_ref.
type WorkflowSchemaRow struct {
	Project   string
	SchemaRef string
	Payload   []byte
	CreatedAt time.Time
}

// WorkflowControlEventRow is one append-only attribution ledger entry for a
// control-plane write to a workflow (register, patch, pin, unpin, delete).
// The ledger exists because workflow_schemas rows are content-addressed:
// re-registering a previously-seen shape (e.g. a revert) reuses the existing
// schema row and would otherwise leave no durable trace of who moved the
// pointer or what control values changed.
type WorkflowControlEventRow struct {
	ID        int64
	Project   string
	Name      string
	Action    string
	Actor     string
	SchemaRef string
	Detail    []byte
	CreatedAt time.Time
}

var ErrWorkflowNotFound = errors.New("workflow not found")

func NewWorkflowsStore(pool *pgxpool.Pool) *WorkflowsStore {
	return &WorkflowsStore{pool: pool}
}

// List returns every workflow row across all projects.
func (s *WorkflowsStore) List(ctx context.Context) ([]WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT project, name, schema_ref, payload, control_pins, created_at, updated_at FROM workflows ORDER BY project, name`
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("workflows: list: %w", err)
	}
	defer rows.Close()
	return scanWorkflowRows(rows)
}

// ListByProject returns workflow rows scoped to one project.
func (s *WorkflowsStore) ListByProject(ctx context.Context, project string) ([]WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT project, name, schema_ref, payload, control_pins, created_at, updated_at FROM workflows WHERE project = $1 ORDER BY name`
	rows, err := s.pool.Query(ctx, sql, project)
	if err != nil {
		return nil, fmt.Errorf("workflows: list by project: %w", err)
	}
	defer rows.Close()
	return scanWorkflowRows(rows)
}

// GetByName point-reads one workflow. Returns ErrWorkflowNotFound when
// no row exists.
func (s *WorkflowsStore) GetByName(ctx context.Context, project, name string) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	const sql = `SELECT project, name, schema_ref, payload, control_pins, created_at, updated_at FROM workflows WHERE project = $1 AND name = $2`
	rows, err := s.pool.Query(ctx, sql, project, name)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: get by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return WorkflowRow{}, ErrWorkflowNotFound
	}
	return scanWorkflowRow(rows)
}

// GetSchemaByRef point-reads one workflow_schemas row.
func (s *WorkflowsStore) GetSchemaByRef(ctx context.Context, project, schemaRef string) (WorkflowSchemaRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowSchemaRow{}, fmt.Errorf("workflows store not configured")
	}
	const sql = `SELECT project, schema_ref, payload, created_at FROM workflow_schemas WHERE project = $1 AND schema_ref = $2`
	var row WorkflowSchemaRow
	if err := s.pool.QueryRow(ctx, sql, project, schemaRef).Scan(&row.Project, &row.SchemaRef, &row.Payload, &row.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkflowSchemaRow{}, ErrWorkflowNotFound
		}
		return WorkflowSchemaRow{}, fmt.Errorf("workflows: get schema: %w", err)
	}
	return row, nil
}

// Upsert creates or updates a workflow row inside a transaction that
// also writes the corresponding workflow_schemas row idempotently and
// appends the attribution ledger event for the write. CreatedAt is
// preserved on update, and control_pins is deliberately NOT in the
// ON CONFLICT update set: registration cannot move operator pins.
func (s *WorkflowsStore) Upsert(ctx context.Context, row WorkflowRow, schema WorkflowSchemaRow, event WorkflowControlEventRow) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: begin upsert: %w", err)
	}
	defer tx.Rollback(ctx)

	const schemaSQL = `
		INSERT INTO workflow_schemas (project, schema_ref, payload, created_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (project, schema_ref) DO NOTHING
	`
	if _, err := tx.Exec(ctx, schemaSQL, schema.Project, schema.SchemaRef, schema.Payload); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: upsert schema: %w", err)
	}

	const upsertSQL = `
		INSERT INTO workflows (project, name, schema_ref, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, name) DO UPDATE
		  SET schema_ref = EXCLUDED.schema_ref,
		      payload    = EXCLUDED.payload,
		      updated_at = now()
		RETURNING project, name, schema_ref, payload, control_pins, created_at, updated_at
	`
	rows, err := tx.Query(ctx, upsertSQL, row.Project, row.Name, row.SchemaRef, row.Payload)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: upsert workflow: %w", err)
	}
	out, scanErr := scanWorkflowFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	if err := insertControlEvent(ctx, tx, event); err != nil {
		return WorkflowRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: commit upsert: %w", err)
	}
	return out, nil
}

// UpdateControlPins replaces the operator-owned control_pins column inside a
// transaction that also appends the pin/unpin ledger event. The authored
// payload and schema pointer are untouched.
func (s *WorkflowsStore) UpdateControlPins(ctx context.Context, project, name string, pins []byte, event WorkflowControlEventRow) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: begin pins update: %w", err)
	}
	defer tx.Rollback(ctx)

	const updateSQL = `
		UPDATE workflows SET control_pins = $3, updated_at = now()
		WHERE project = $1 AND name = $2
		RETURNING project, name, schema_ref, payload, control_pins, created_at, updated_at
	`
	rows, err := tx.Query(ctx, updateSQL, project, name, pins)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: update pins: %w", err)
	}
	out, scanErr := scanWorkflowFirstRowNotFound(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	if err := insertControlEvent(ctx, tx, event); err != nil {
		return WorkflowRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: commit pins update: %w", err)
	}
	return out, nil
}

// ListControlEvents returns the newest ledger entries for one workflow,
// newest first, capped at limit.
func (s *WorkflowsStore) ListControlEvents(ctx context.Context, project, name string, limit int) ([]WorkflowControlEventRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT id, project, name, action, actor, schema_ref, detail, created_at
		FROM workflow_control_events
		WHERE project = $1 AND name = $2
		ORDER BY id DESC
		LIMIT $3
	`
	rows, err := s.pool.Query(ctx, sql, project, name, limit)
	if err != nil {
		return nil, fmt.Errorf("workflows: list control events: %w", err)
	}
	defer rows.Close()
	out := []WorkflowControlEventRow{}
	for rows.Next() {
		var row WorkflowControlEventRow
		if err := rows.Scan(&row.ID, &row.Project, &row.Name, &row.Action, &row.Actor, &row.SchemaRef, &row.Detail, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("workflows: scan control event: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflows: iterate control events: %w", err)
	}
	return out, nil
}

// Delete tombstones a workflow row and returns its updated state, appending the
// delete ledger event in the same transaction. The workflow row itself is not
// physically deleted: run history and replay paths may still need to resolve it
// by (project, name) when old runs predate schema_ref fallback or when an
// operator is repairing a parked gate.
func (s *WorkflowsStore) Delete(ctx context.Context, project, name string, event WorkflowControlEventRow) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: begin delete: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `
		SELECT project, name, schema_ref, payload, control_pins, created_at, updated_at
		FROM workflows
		WHERE project = $1 AND name = $2
		FOR UPDATE
	`
	rows, err := tx.Query(ctx, selectSQL, project, name)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: select for delete: %w", err)
	}
	out, scanErr := scanWorkflowFirstRowNotFound(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	updatedPayload, err := tombstoneWorkflowPayload(out.Payload, event.Actor, time.Now().UTC())
	if err != nil {
		return WorkflowRow{}, err
	}
	const updateSQL = `
		UPDATE workflows
		SET payload = $3, updated_at = now()
		WHERE project = $1 AND name = $2
		RETURNING project, name, schema_ref, payload, control_pins, created_at, updated_at
	`
	rows, err = tx.Query(ctx, updateSQL, project, name, updatedPayload)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: tombstone delete: %w", err)
	}
	out, scanErr = scanWorkflowFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	if err := insertControlEvent(ctx, tx, event); err != nil {
		return WorkflowRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: commit delete: %w", err)
	}
	return out, nil
}

func tombstoneWorkflowPayload(raw []byte, actor string, now time.Time) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("workflows: decode delete tombstone payload: %w", err)
	}
	metadata, _ := payload["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["deleted_at"] = now.UTC().Format(time.RFC3339Nano)
	if actor != "" {
		metadata["deleted_by"] = actor
	}
	metadata["usable"] = false
	metadata["visible"] = false
	payload["metadata"] = metadata
	updated, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("workflows: encode delete tombstone payload: %w", err)
	}
	return updated, nil
}

func insertControlEvent(ctx context.Context, tx pgx.Tx, event WorkflowControlEventRow) error {
	detail := event.Detail
	if len(detail) == 0 {
		detail = []byte(`{}`)
	}
	const sql = `
		INSERT INTO workflow_control_events (project, name, action, actor, schema_ref, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
	`
	if _, err := tx.Exec(ctx, sql, event.Project, event.Name, event.Action, event.Actor, event.SchemaRef, detail); err != nil {
		return fmt.Errorf("workflows: insert control event: %w", err)
	}
	return nil
}

func scanWorkflowRows(rows pgx.Rows) ([]WorkflowRow, error) {
	out := []WorkflowRow{}
	for rows.Next() {
		row, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflows: iterate: %w", err)
	}
	return out, nil
}

func scanWorkflowRow(rows pgx.Rows) (WorkflowRow, error) {
	var row WorkflowRow
	if err := rows.Scan(&row.Project, &row.Name, &row.SchemaRef, &row.Payload, &row.ControlPins, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: scan: %w", err)
	}
	return row, nil
}

func scanWorkflowFirstRow(rows pgx.Rows) (WorkflowRow, error) {
	if !rows.Next() {
		return WorkflowRow{}, fmt.Errorf("workflows: returned no row")
	}
	return scanWorkflowRow(rows)
}

// scanWorkflowFirstRowNotFound maps an empty result to ErrWorkflowNotFound —
// for statements whose WHERE clause targets one existing row.
func scanWorkflowFirstRowNotFound(rows pgx.Rows) (WorkflowRow, error) {
	if !rows.Next() {
		return WorkflowRow{}, ErrWorkflowNotFound
	}
	return scanWorkflowRow(rows)
}

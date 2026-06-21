package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PreviewEnvironmentsStore is the Postgres-backed store for the live-preview
// lane's durable source of truth (the preview_environments table). Keyed by
// (project, name); each preview env is its own row, so per-env writes don't
// contend. Mirrors SlotsStore's CAS shape.
type PreviewEnvironmentsStore struct {
	pool *pgxpool.Pool
}

// PreviewEnvironmentRow is one preview_environments row. UpdatedAt is the CAS
// version — pass it back to UpdateWithCAS to optimistically replace the payload.
type PreviewEnvironmentRow struct {
	Project   string
	Name      string
	Payload   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	ErrPreviewEnvNotFound           = errors.New("preview environment not found")
	ErrPreviewEnvPreconditionFailed = errors.New("preview environment precondition failed")
	ErrPreviewEnvAlreadyExists      = errors.New("preview environment already exists")
)

func NewPreviewEnvironmentsStore(pool *pgxpool.Pool) *PreviewEnvironmentsStore {
	return &PreviewEnvironmentsStore{pool: pool}
}

// Get returns the row for (project, name).
func (s *PreviewEnvironmentsStore) Get(ctx context.Context, project, name string) (PreviewEnvironmentRow, error) {
	if s == nil || s.pool == nil {
		return PreviewEnvironmentRow{}, fmt.Errorf("preview environments store not configured")
	}
	const sql = `SELECT project, name, payload, created_at, updated_at FROM preview_environments WHERE project = $1 AND name = $2`
	var out PreviewEnvironmentRow
	if err := s.pool.QueryRow(ctx, sql, project, name).Scan(
		&out.Project, &out.Name, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PreviewEnvironmentRow{}, ErrPreviewEnvNotFound
		}
		return PreviewEnvironmentRow{}, fmt.Errorf("preview_environments: get: %w", err)
	}
	return out, nil
}

// ListByProject returns every preview env for project, newest first.
func (s *PreviewEnvironmentsStore) ListByProject(ctx context.Context, project string) ([]PreviewEnvironmentRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT project, name, payload, created_at, updated_at
		FROM preview_environments
		WHERE project = $1
		ORDER BY updated_at DESC
	`
	return s.queryRows(ctx, sql, project)
}

// ListAll returns every preview env across all projects, newest first. Used by
// the verifier reconciler and the state snapshot.
func (s *PreviewEnvironmentsStore) ListAll(ctx context.Context) ([]PreviewEnvironmentRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT project, name, payload, created_at, updated_at
		FROM preview_environments
		ORDER BY updated_at DESC
	`
	return s.queryRows(ctx, sql)
}

func (s *PreviewEnvironmentsStore) queryRows(ctx context.Context, sql string, args ...any) ([]PreviewEnvironmentRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("preview_environments: list: %w", err)
	}
	defer rows.Close()
	out := []PreviewEnvironmentRow{}
	for rows.Next() {
		var row PreviewEnvironmentRow
		if err := rows.Scan(&row.Project, &row.Name, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("preview_environments: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("preview_environments: iterate: %w", err)
	}
	return out, nil
}

// Create inserts a new preview env row. If (project, name) already exists,
// returns ErrPreviewEnvAlreadyExists so the caller can fall back to Get.
func (s *PreviewEnvironmentsStore) Create(ctx context.Context, row PreviewEnvironmentRow) (PreviewEnvironmentRow, error) {
	if s == nil || s.pool == nil {
		return PreviewEnvironmentRow{}, fmt.Errorf("preview environments store not configured")
	}
	const insertSQL = `
		INSERT INTO preview_environments (project, name, payload, created_at, updated_at)
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT (project, name) DO NOTHING
		RETURNING project, name, payload, created_at, updated_at
	`
	var out PreviewEnvironmentRow
	err := s.pool.QueryRow(ctx, insertSQL, row.Project, row.Name, row.Payload).Scan(
		&out.Project, &out.Name, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PreviewEnvironmentRow{}, ErrPreviewEnvAlreadyExists
	}
	if err != nil {
		return PreviewEnvironmentRow{}, fmt.Errorf("preview_environments: create: %w", err)
	}
	return out, nil
}

// UpdateWithCAS optimistically replaces the payload. expected is the UpdatedAt
// the caller read; on a version mismatch returns ErrPreviewEnvPreconditionFailed,
// on a missing row ErrPreviewEnvNotFound.
func (s *PreviewEnvironmentsStore) UpdateWithCAS(ctx context.Context, project, name string, payload []byte, expected time.Time) (PreviewEnvironmentRow, error) {
	if s == nil || s.pool == nil {
		return PreviewEnvironmentRow{}, fmt.Errorf("preview environments store not configured")
	}
	const updateSQL = `
		UPDATE preview_environments
		SET payload = $3, updated_at = now()
		WHERE project = $1 AND name = $2 AND updated_at = $4
		RETURNING project, name, payload, created_at, updated_at
	`
	var out PreviewEnvironmentRow
	err := s.pool.QueryRow(ctx, updateSQL, project, name, payload, expected).Scan(
		&out.Project, &out.Name, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		const existsSQL = `SELECT 1 FROM preview_environments WHERE project = $1 AND name = $2`
		var dummy int
		existsErr := s.pool.QueryRow(ctx, existsSQL, project, name).Scan(&dummy)
		if errors.Is(existsErr, pgx.ErrNoRows) {
			return PreviewEnvironmentRow{}, ErrPreviewEnvNotFound
		}
		if existsErr != nil {
			return PreviewEnvironmentRow{}, fmt.Errorf("preview_environments: cas existence check: %w", existsErr)
		}
		return PreviewEnvironmentRow{}, ErrPreviewEnvPreconditionFailed
	}
	if err != nil {
		return PreviewEnvironmentRow{}, fmt.Errorf("preview_environments: update: %w", err)
	}
	return out, nil
}

// Delete removes a preview env row. Idempotent.
func (s *PreviewEnvironmentsStore) Delete(ctx context.Context, project, name string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("preview environments store not configured")
	}
	const sql = `DELETE FROM preview_environments WHERE project = $1 AND name = $2`
	if _, err := s.pool.Exec(ctx, sql, project, name); err != nil {
		return fmt.Errorf("preview_environments: delete: %w", err)
	}
	return nil
}

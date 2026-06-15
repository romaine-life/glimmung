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

type ReviewsStore struct {
	pool *pgxpool.Pool
}

type ReviewRow struct {
	Project     string
	IssueNumber int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var ErrReviewNotFound = errors.New("review not found")

func NewReviewsStore(pool *pgxpool.Pool) *ReviewsStore {
	return &ReviewsStore{pool: pool}
}

// List returns review rows, optionally filtered by project, repo
// (matches payload->>'repo'), state (matches payload->>'state').
// Ordered by updated_at DESC.
func (s *ReviewsStore) List(ctx context.Context, project, repo, state string, limit int) ([]ReviewRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT project, issue_number, payload, created_at, updated_at FROM reviews`
	args := []any{}
	var where []string
	if project != "" {
		where = append(where, fmt.Sprintf("project = $%d", len(args)+1))
		args = append(args, project)
	}
	if repo != "" {
		where = append(where, fmt.Sprintf("payload->>'repo' = $%d", len(args)+1))
		args = append(args, repo)
	}
	if state != "" {
		where = append(where, fmt.Sprintf("payload->>'state' = $%d", len(args)+1))
		args = append(args, state)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + where[0]
		for i := 1; i < len(where); i++ {
			sqlText += " AND " + where[i]
		}
	}
	sqlText += " ORDER BY updated_at DESC"
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("reviews: list: %w", err)
	}
	defer rows.Close()
	out := []ReviewRow{}
	for rows.Next() {
		var row ReviewRow
		if err := rows.Scan(&row.Project, &row.IssueNumber, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("reviews: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reviews: iterate: %w", err)
	}
	return out, nil
}

// FindByLinkedIssueID returns the most-recently-updated review
// for a (project, payload->>'linked_issue_id') pair.
func (s *ReviewsStore) FindByLinkedIssueID(ctx context.Context, project, linkedIssueID string) (ReviewRow, error) {
	if s == nil || s.pool == nil {
		return ReviewRow{}, fmt.Errorf("reviews store not configured")
	}
	const sql = `
		SELECT project, issue_number, payload, created_at, updated_at
		FROM reviews
		WHERE project = $1 AND payload->>'linked_issue_id' = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out ReviewRow
	if err := s.pool.QueryRow(ctx, sql, project, linkedIssueID).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRow{}, ErrReviewNotFound
		}
		return ReviewRow{}, fmt.Errorf("reviews: find by linked_issue_id: %w", err)
	}
	return out, nil
}

// FindByRepoNumber returns the review with the given (repo, number)
// regardless of project.
func (s *ReviewsStore) FindByRepoNumber(ctx context.Context, repo string, number int) (ReviewRow, error) {
	if s == nil || s.pool == nil {
		return ReviewRow{}, fmt.Errorf("reviews store not configured")
	}
	const sql = `
		SELECT project, issue_number, payload, created_at, updated_at
		FROM reviews
		WHERE payload->>'repo' = $1 AND issue_number = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out ReviewRow
	if err := s.pool.QueryRow(ctx, sql, repo, number).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRow{}, ErrReviewNotFound
		}
		return ReviewRow{}, fmt.Errorf("reviews: find by repo+number: %w", err)
	}
	return out, nil
}

// GetByProjectAndPR looks up a review by (project, issue_number).
// issue_number is the PR number on the review cluster.
func (s *ReviewsStore) GetByProjectAndPR(ctx context.Context, project string, prNumber int) (ReviewRow, error) {
	if s == nil || s.pool == nil {
		return ReviewRow{}, fmt.Errorf("reviews store not configured")
	}
	const sql = `SELECT project, issue_number, payload, created_at, updated_at FROM reviews WHERE project = $1 AND issue_number = $2`
	var out ReviewRow
	if err := s.pool.QueryRow(ctx, sql, project, prNumber).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRow{}, ErrReviewNotFound
		}
		return ReviewRow{}, fmt.Errorf("reviews: get by project+pr: %w", err)
	}
	return out, nil
}

// Create inserts a new review row. Returns the inserted row, or the
// existing row on conflict. Create is reserved for the explicit "create new"
// branch of EnsureReview.
func (s *ReviewsStore) Create(ctx context.Context, row ReviewRow) (ReviewRow, error) {
	if s == nil || s.pool == nil {
		return ReviewRow{}, fmt.Errorf("reviews store not configured")
	}
	const insertSQL = `
		INSERT INTO reviews (project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT (project, issue_number) DO NOTHING
		RETURNING project, issue_number, payload, created_at, updated_at
	`
	var out ReviewRow
	err := s.pool.QueryRow(ctx, insertSQL, row.Project, row.IssueNumber, row.Payload).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRow{}, fmt.Errorf("review %s/%d already exists", row.Project, row.IssueNumber)
	}
	if err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// transaction. Used by EnsureReview when linkages are patched on
// an existing row.
func (s *ReviewsStore) PatchPayload(ctx context.Context, project string, prNumber int, mutate func(payload map[string]any) error) (ReviewRow, error) {
	if s == nil || s.pool == nil {
		return ReviewRow{}, fmt.Errorf("reviews store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)
	const selectSQL = `SELECT payload FROM reviews WHERE project = $1 AND issue_number = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, prNumber).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRow{}, ErrReviewNotFound
		}
		return ReviewRow{}, fmt.Errorf("reviews: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return ReviewRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: marshal patched payload: %w", err)
	}
	const updateSQL = `
		UPDATE reviews SET payload = $3, updated_at = now()
		WHERE project = $1 AND issue_number = $2
		RETURNING project, issue_number, payload, created_at, updated_at
	`
	var out ReviewRow
	if err := tx.QueryRow(ctx, updateSQL, project, prNumber, newPayload).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewRow{}, fmt.Errorf("reviews: commit patch: %w", err)
	}
	return out, nil
}

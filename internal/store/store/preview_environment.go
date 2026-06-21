package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pgstore "github.com/romaine-life/glimmung/internal/store/pg"

	"github.com/romaine-life/glimmung/internal/server"
)

// previewEnvDoc is the JSON payload stored in the preview_environments table.
// It embeds the domain type so the durable shape and the API shape stay in
// lockstep (one struct, one set of json tags).
type previewEnvDoc struct {
	ID string `json:"id"`
	server.PreviewEnvironment
}

func newPreviewEnvDoc(e server.PreviewEnvironment) previewEnvDoc {
	return previewEnvDoc{ID: server.PreviewEnvDocID(e.Project, e.Name), PreviewEnvironment: e}
}

// previewEnvETagFromUpdatedAt / parsePreviewEnvETag mirror the slot CAS etag:
// the pg row's updated_at, RFC3339Nano, round-trips through the domain ETag.
func previewEnvETagFromUpdatedAt(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parsePreviewEnvETag(etag string) (time.Time, error) {
	if etag == "" {
		return time.Time{}, fmt.Errorf("preview env etag missing")
	}
	t, err := time.Parse(time.RFC3339Nano, etag)
	if err != nil {
		return time.Time{}, fmt.Errorf("preview env etag parse: %w", err)
	}
	return t, nil
}

func previewEnvFromPGRow(row pgstore.PreviewEnvironmentRow) (server.PreviewEnvironment, error) {
	var doc previewEnvDoc
	if err := json.Unmarshal(row.Payload, &doc); err != nil {
		return server.PreviewEnvironment{}, fmt.Errorf("preview env unmarshal: %w", err)
	}
	env := doc.PreviewEnvironment
	// created_at/updated_at are owned by the row, not the payload, so the
	// authoritative timestamps survive a re-marshal.
	env.CreatedAt = row.CreatedAt
	env.UpdatedAt = row.UpdatedAt
	return env.WithETag(previewEnvETagFromUpdatedAt(row.UpdatedAt)), nil
}

// CreatePreviewEnvironment writes a new preview env row. Idempotent at the
// Store layer: if a row already exists at (project, name) the existing row is
// returned (a re-provision of the same env name reuses the durable record).
func (s *Store) CreatePreviewEnvironment(ctx context.Context, env server.PreviewEnvironment) (server.PreviewEnvironment, error) {
	if s == nil || s.pgPreviewEnvironments == nil {
		return server.PreviewEnvironment{}, errors.New("preview environments store not configured")
	}
	payload, err := json.Marshal(newPreviewEnvDoc(env))
	if err != nil {
		return server.PreviewEnvironment{}, fmt.Errorf("preview env marshal: %w", err)
	}
	row, err := s.pgPreviewEnvironments.Create(ctx, pgstore.PreviewEnvironmentRow{
		Project: env.Project,
		Name:    env.Name,
		Payload: payload,
	})
	if errors.Is(err, pgstore.ErrPreviewEnvAlreadyExists) {
		return s.GetPreviewEnvironment(ctx, env.Project, env.Name)
	}
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	return previewEnvFromPGRow(row)
}

// GetPreviewEnvironment reads a single preview env, attaching the CAS etag.
// Returns server.ErrNotFound when absent.
func (s *Store) GetPreviewEnvironment(ctx context.Context, project, name string) (server.PreviewEnvironment, error) {
	if s == nil || s.pgPreviewEnvironments == nil {
		return server.PreviewEnvironment{}, errors.New("preview environments store not configured")
	}
	row, err := s.pgPreviewEnvironments.Get(ctx, project, name)
	if errors.Is(err, pgstore.ErrPreviewEnvNotFound) {
		return server.PreviewEnvironment{}, server.ErrNotFound
	}
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	return previewEnvFromPGRow(row)
}

// ListPreviewEnvironments returns every preview env across all projects.
func (s *Store) ListPreviewEnvironments(ctx context.Context) ([]server.PreviewEnvironment, error) {
	if s == nil || s.pgPreviewEnvironments == nil {
		return nil, nil
	}
	rows, err := s.pgPreviewEnvironments.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	return previewEnvsFromRows(rows)
}

// ListPreviewEnvironmentsByProject returns every preview env for one project.
func (s *Store) ListPreviewEnvironmentsByProject(ctx context.Context, project string) ([]server.PreviewEnvironment, error) {
	if s == nil || s.pgPreviewEnvironments == nil {
		return nil, nil
	}
	rows, err := s.pgPreviewEnvironments.ListByProject(ctx, project)
	if err != nil {
		return nil, err
	}
	return previewEnvsFromRows(rows)
}

func previewEnvsFromRows(rows []pgstore.PreviewEnvironmentRow) ([]server.PreviewEnvironment, error) {
	out := make([]server.PreviewEnvironment, 0, len(rows))
	for _, row := range rows {
		env, err := previewEnvFromPGRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, nil
}

// UpdatePreviewEnvironmentIfMatch reads the env, applies mutate, and writes it
// back conditionally on the CAS etag. Returns server.ErrNotFound if the row is
// gone and server.ErrPreconditionFailed on a concurrent write, mirroring
// UpdateIfMatch for slots. The mutate closure receives the current durable env
// and returns the desired next state.
func (s *Store) UpdatePreviewEnvironmentIfMatch(ctx context.Context, project, name string, mutate func(server.PreviewEnvironment) (server.PreviewEnvironment, error)) (server.PreviewEnvironment, error) {
	if s == nil || s.pgPreviewEnvironments == nil {
		return server.PreviewEnvironment{}, errors.New("preview environments store not configured")
	}
	current, err := s.GetPreviewEnvironment(ctx, project, name)
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	next, err := mutate(current)
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	// Identity invariants: a mutate can never move a row to a different key.
	next.Project = current.Project
	next.Name = current.Name
	expected, err := parsePreviewEnvETag(current.ETag())
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	payload, err := json.Marshal(newPreviewEnvDoc(next))
	if err != nil {
		return server.PreviewEnvironment{}, fmt.Errorf("preview env marshal: %w", err)
	}
	row, err := s.pgPreviewEnvironments.UpdateWithCAS(ctx, project, name, payload, expected)
	if errors.Is(err, pgstore.ErrPreviewEnvNotFound) {
		return server.PreviewEnvironment{}, server.ErrNotFound
	}
	if errors.Is(err, pgstore.ErrPreviewEnvPreconditionFailed) {
		return server.PreviewEnvironment{}, server.ErrPreconditionFailed
	}
	if err != nil {
		return server.PreviewEnvironment{}, err
	}
	return previewEnvFromPGRow(row)
}

// DeletePreviewEnvironment removes a preview env row. Idempotent.
func (s *Store) DeletePreviewEnvironment(ctx context.Context, project, name string) error {
	if s == nil || s.pgPreviewEnvironments == nil {
		return errors.New("preview environments store not configured")
	}
	return s.pgPreviewEnvironments.Delete(ctx, project, name)
}

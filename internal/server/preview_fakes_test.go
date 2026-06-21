package server

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// fakePreviewStore is an in-memory store satisfying ReadStore, ProjectReader,
// PreviewControlStore, and PreviewVerifierStore for the preview-lane tests.
type fakePreviewStore struct {
	mu       sync.Mutex
	envs     map[string]PreviewEnvironment
	projects map[string]Project
	etagSeq  int
}

func newFakePreviewStore() *fakePreviewStore {
	return &fakePreviewStore{envs: map[string]PreviewEnvironment{}, projects: map[string]Project{}}
}

func previewKey(project, name string) string { return project + "/" + name }

// ReadStore.
func (f *fakePreviewStore) ListProjects(ctx context.Context) ([]Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Project, 0, len(f.projects))
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, nil
}
func (f *fakePreviewStore) ListWorkflows(ctx context.Context) ([]Workflow, error) { return nil, nil }

// ProjectReader.
func (f *fakePreviewStore) ReadProject(ctx context.Context, project string) (Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[project]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (f *fakePreviewStore) putProject(p Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[p.Name] = p
}

func (f *fakePreviewStore) nextETag() string {
	f.etagSeq++
	return strconv.Itoa(f.etagSeq)
}

func (f *fakePreviewStore) CreatePreviewEnvironment(ctx context.Context, env PreviewEnvironment) (PreviewEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := previewKey(env.Project, env.Name)
	if existing, ok := f.envs[key]; ok {
		return existing, nil // idempotent
	}
	now := time.Now().UTC()
	env.CreatedAt = now
	env.UpdatedAt = now
	env = env.WithETag(f.nextETag())
	f.envs[key] = env
	return env, nil
}

func (f *fakePreviewStore) GetPreviewEnvironment(ctx context.Context, project, name string) (PreviewEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	env, ok := f.envs[previewKey(project, name)]
	if !ok {
		return PreviewEnvironment{}, ErrNotFound
	}
	return env, nil
}

func (f *fakePreviewStore) ListPreviewEnvironments(ctx context.Context) ([]PreviewEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PreviewEnvironment, 0, len(f.envs))
	for _, env := range f.envs {
		out = append(out, env)
	}
	return out, nil
}

func (f *fakePreviewStore) UpdatePreviewEnvironmentIfMatch(ctx context.Context, project, name string, mutate func(PreviewEnvironment) (PreviewEnvironment, error)) (PreviewEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := previewKey(project, name)
	cur, ok := f.envs[key]
	if !ok {
		return PreviewEnvironment{}, ErrNotFound
	}
	next, err := mutate(cur)
	if err != nil {
		return PreviewEnvironment{}, err
	}
	next.Project = cur.Project
	next.Name = cur.Name
	next.CreatedAt = cur.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	next = next.WithETag(f.nextETag())
	f.envs[key] = next
	return next, nil
}

func (f *fakePreviewStore) DeletePreviewEnvironment(ctx context.Context, project, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.envs, previewKey(project, name))
	return nil
}

// stubStatusReader returns a canned edge status per preview URL.
type stubStatusReader struct {
	mu        sync.Mutex
	byURL     map[string]PreviewEdgeStatus
	err       error
	callCount int
}

func (s *stubStatusReader) ReadStatus(ctx context.Context, previewURL string) (PreviewEdgeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	if s.err != nil {
		return PreviewEdgeStatus{}, s.err
	}
	return s.byURL[previewURL], nil
}

func (s *stubStatusReader) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

// fakePreviewProvisioner records provision/deprovision calls.
type fakePreviewProvisioner struct {
	mu          sync.Mutex
	err         error
	provisioned []string
	deprovised  []string
}

func (f *fakePreviewProvisioner) ProvisionPreview(ctx context.Context, env PreviewEnvironment, project Project, minter RunnerGitHubTokenMinter) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisioned = append(f.provisioned, env.Name)
	return f.err
}

func (f *fakePreviewProvisioner) DeprovisionPreview(ctx context.Context, env PreviewEnvironment, project Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deprovised = append(f.deprovised, env.Name)
	return nil
}

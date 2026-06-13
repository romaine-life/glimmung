package runnermcp

import "context"

// RunContext is the durable identity of the run a tool acts within. It scopes
// where artifacts land and which run owns the result. It is injected into the
// runner MCP server at launch from the run's environment, not chosen by the
// agent.
type RunContext struct {
	Project string
	RunID   string
}

// ArtifactUploader is the narrow subset of Glimmung's artifact store the runner
// tools need. The production store (internal/store/artifacts.Store) satisfies
// it; tests pass a fake. Keeping it an interface here keeps runnermcp decoupled
// from the large server package.
type ArtifactUploader interface {
	Upload(ctx context.Context, blobName string, body []byte, contentType string) (int64, error)
}

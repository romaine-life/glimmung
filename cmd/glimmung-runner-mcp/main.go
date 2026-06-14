// Command glimmung-runner-mcp is the per-job runner MCP server. It runs as a
// sidecar in a run pod, exposes exactly the tools the job declared (via the
// GLIMMUNG_RUNNER_TOOLS allow-list) over loopback HTTP, and performs privileged
// actions with the run's own identity. That lets the agent container invoke
// capabilities through structured tools without holding their credentials —
// the credentials live here, in the sidecar.
//
// It is intentionally a separate surface from the operator MCP server
// (mcp-glimmung): a run sees only its job's tools, never the operator tools.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/romaine-life/glimmung/internal/runnermcp"
	"github.com/romaine-life/glimmung/internal/server"
	"github.com/romaine-life/glimmung/internal/store/artifacts"
)

func main() {
	rc := runnermcp.RunContext{
		Project: strings.TrimSpace(os.Getenv("GLIMMUNG_PROJECT")),
		RunID:   strings.TrimSpace(os.Getenv("GLIMMUNG_RUN_ID")),
	}
	addr := envOr("GLIMMUNG_RUNNER_MCP_ADDR", "127.0.0.1:8765")
	workspace := envOr("GLIMMUNG_RUNNER_MCP_WORKSPACE", "/workspace")
	allow := splitCSV(os.Getenv("GLIMMUNG_RUNNER_TOOLS"))

	reg := runnermcp.NewRegistry()

	// Register tools whose dependencies are available. A tool that is
	// allow-listed but could not be registered (e.g. its backing store is
	// unavailable) fails loudly at Scoped() below, rather than silently
	// vanishing from the job's surface.
	if store, err := artifacts.NewFromSettings(server.Settings{
		ArtifactsStorageAccount: strings.TrimSpace(os.Getenv("ARTIFACTS_STORAGE_ACCOUNT")),
		ArtifactsContainer:      strings.TrimSpace(os.Getenv("ARTIFACTS_CONTAINER")),
	}); err == nil {
		reg.Register(runnermcp.NewUploadEvidenceTool(rc, os.DirFS(workspace), store))
	} else {
		slog.Warn("runner-mcp: artifact store unavailable; upload_evidence not registered", "error", err)
	}
	// Future runner tools (capture_video, capture_screenshot, …) register here.

	tools, err := reg.Scoped(allow)
	if err != nil {
		slog.Error("runner-mcp: tool allow-list rejected", "error", err, "allow", allow)
		os.Exit(1)
	}

	srv := runnermcp.NewServer(envOr("GLIMMUNG_VERSION", "dev"), tools)
	slog.Info("runner-mcp: serving",
		"addr", addr, "project", rc.Project, "run", rc.RunID, "tools", toolNames(tools))

	httpSrv := &http.Server{Addr: addr, Handler: runnermcp.HTTPHandler(srv)}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("runner-mcp: server stopped", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func toolNames(ts []runnermcp.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// Package runnermcp is the runner-side tool surface: the small, strictly scoped
// set of tools a run's agent may call, composed per job.
//
// It is deliberately distinct from the operator MCP surface (mcp-glimmung,
// used by interactive Tank sessions). A run is not an operator: it must not see
// dispatch/registration/issue tools, and it should not be handed tools it has
// no way to use. So the runner gets its own surface, and a job composes that
// surface from a declared allow-list — the agent sees exactly those tools and
// nothing else.
//
// This file is the scoping core: a registry of every runner tool the build
// knows how to perform, plus per-job composition. The MCP transport/server and
// the concrete tools are layered on top.
package runnermcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Tool is one runner capability the agent can invoke. Implementations reuse
// Glimmung's internal libraries (artifact store, evidence checks, …) so a tool
// is a thin, structured front door over an existing capability.
type Tool struct {
	// Name is the unique tool identifier the agent calls.
	Name string
	// Description is the human/model-facing summary used in the MCP listing.
	Description string
	// InputSchema is the JSON Schema for the tool's arguments.
	InputSchema json.RawMessage
	// Handler performs the work. It receives the per-call run context and the
	// raw JSON arguments, and returns a JSON-serializable result.
	Handler func(ctx context.Context, args json.RawMessage) (any, error)
}

// Registry holds every runner tool the build knows how to perform. Tools are
// registered once at startup; a job then composes its surface from a subset by
// name via Scoped.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool. It panics on an empty or duplicate name: these are
// programmer errors wired at startup, not runtime conditions, and should fail
// the process immediately rather than silently shadow a tool.
func (r *Registry) Register(t Tool) {
	if t.Name == "" {
		panic("runnermcp: tool registered with empty name")
	}
	if t.Handler == nil {
		panic("runnermcp: tool " + t.Name + " registered with nil handler")
	}
	if _, exists := r.tools[t.Name]; exists {
		panic("runnermcp: duplicate tool registration: " + t.Name)
	}
	r.tools[t.Name] = t
}

// Has reports whether a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// Names returns every registered tool name, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Scoped returns exactly the tools named in allow, sorted and de-duplicated.
//
// It is strict by design: a name that is not registered is an error, so a job
// can never request a tool that does not exist — a typo or a stale allow-list
// fails loudly at compose time instead of silently handing the agent a smaller
// surface than the workflow intended. An empty allow-list yields an empty
// surface: a job that declares no tools gets none, which is the safe default.
func (r *Registry) Scoped(allow []string) ([]Tool, error) {
	seen := make(map[string]bool, len(allow))
	out := make([]Tool, 0, len(allow))
	for _, name := range allow {
		if name == "" {
			return nil, fmt.Errorf("runnermcp: allow-list contains an empty tool name")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		tool, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("runnermcp: job requested unknown tool %q (registered: %v)", name, r.Names())
		}
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

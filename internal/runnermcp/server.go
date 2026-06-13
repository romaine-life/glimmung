package runnermcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the MCP implementation name the runner surface advertises. It
// is intentionally distinct from the operator server (mcp-glimmung) so the two
// surfaces are never confused.
const ServerName = "glimmung-runner"

// NewServer builds an MCP server exposing exactly the given tools. Callers
// compose `tools` from a Registry via Scoped(allow-list), so the server
// advertises only the job's permitted surface — there is nothing else for the
// agent to discover or call.
func NewServer(version string, tools []Tool) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, nil)
	for _, t := range tools {
		s.AddTool(sdkTool(t), toolHandler(t))
	}
	return s
}

func sdkTool(t Tool) *mcp.Tool {
	var schema any = t.InputSchema
	if len(t.InputSchema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return &mcp.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
	}
}

func toolHandler(t Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := json.RawMessage(`{}`)
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			raw = req.Params.Arguments
		}
		out, err := t.Handler(ctx, raw)
		if err != nil {
			// Tool-execution failures are returned in-band (isError) rather than
			// as MCP protocol errors, so the agent sees the message and can
			// react instead of the call failing at the transport level.
			return errorResult(err.Error()), nil
		}
		text, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return errorResult("tool result was not serializable: " + marshalErr.Error()), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
			StructuredContent: out,
		}, nil
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// HTTPHandler serves the scoped server over the streamable HTTP transport for
// the run's agent sidecar (bound to loopback by the caller). The same scoped
// server instance is reused across sessions within the run.
func HTTPHandler(s *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}

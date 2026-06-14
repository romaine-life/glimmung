package runnermcp

import (
	"context"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHTTPHandler_RoundTripOverStreamableHTTP exercises the sidecar exactly as a
// run agent does: a streamable-HTTP client connects over a real loopback TCP
// socket, lists the scoped tools, and calls upload_evidence. It guards the
// transport contract the agent depends on and that the in-memory transport
// cannot cover: the loopback DNS-rebinding guard must admit a 127.0.0.1 Host,
// the handler must serve regardless of request path (the agent connects to
// <addr>/mcp), and a tool result must round-trip end to end.
func TestHTTPHandler_RoundTripOverStreamableHTTP(t *testing.T) {
	ws := fstest.MapFS{"videos/run.webm": {Data: []byte("webmbytes")}}
	up := &fakeUploader{}
	srv := NewServer("test", []Tool{NewUploadEvidenceTool(RunContext{Project: "ambience", RunID: "168.3"}, ws, up)})

	ts := httptest.NewServer(HTTPHandler(srv))
	defer ts.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	// Connect to <addr>/mcp — the exact URL the launcher hands the agent. The
	// handler ignores the path, but using the real URL proves the contract.
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("client connect over HTTP: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools over HTTP: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != ToolUploadEvidence {
		t.Fatalf("ListTools over HTTP = %+v, want exactly [upload_evidence]", tools.Tools)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      ToolUploadEvidence,
		Arguments: map[string]any{"path": "videos/run.webm", "kind": "video"},
	})
	if err != nil {
		t.Fatalf("call tool over HTTP: %v", err)
	}
	if res.IsError {
		t.Fatalf("upload_evidence over HTTP errored: %+v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content type = %T", res.StructuredContent)
	}
	if sc["blob_name"] != "runs/ambience/168.3/videos/run.webm" {
		t.Fatalf("blob_name = %v", sc["blob_name"])
	}
	if up.calls != 1 {
		t.Fatalf("uploader calls = %d, want 1", up.calls)
	}
}

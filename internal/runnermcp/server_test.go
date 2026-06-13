package runnermcp

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectScoped wires a scoped runner server to an in-memory client session, so
// tests exercise the real MCP protocol (initialize / tools/list / tools/call).
func connectScoped(t *testing.T, tools []Tool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := NewServer("test", tools)
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

func TestServer_ListsOnlyScopedTools(t *testing.T) {
	reg := newPopulated() // upload_evidence, capture_video, await_pr_checks
	scoped, err := reg.Scoped([]string{"upload_evidence", "capture_video"})
	if err != nil {
		t.Fatal(err)
	}
	cs := connectScoped(t, scoped)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		got[i] = tool.Name
	}
	want := []string{"capture_video", "upload_evidence"}
	if !equalStrings(got, want) {
		t.Fatalf("ListTools = %v, want exactly %v (unscoped/operator tools must not leak)", got, want)
	}
}

func TestServer_CallToolSuccessAndInBandError(t *testing.T) {
	ws := fstest.MapFS{"videos/run.webm": {Data: []byte("webmbytes")}}
	up := &fakeUploader{}
	tool := NewUploadEvidenceTool(RunContext{Project: "ambience", RunID: "168.3"}, ws, up)
	cs := connectScoped(t, []Tool{tool})
	ctx := context.Background()

	// Success path: result carries structured content the agent can read.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upload_evidence",
		Arguments: map[string]any{"path": "videos/run.webm", "kind": "video"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
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

	// Failure path: a tool error surfaces in-band (IsError), not as a transport
	// /protocol error, so the agent sees the message.
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upload_evidence",
		Arguments: map[string]any{"path": "videos/missing.webm", "kind": "video"},
	})
	if err != nil {
		t.Fatalf("error path returned a protocol error, want in-band: %v", err)
	}
	if !res2.IsError {
		t.Fatal("missing-file call should surface as IsError")
	}
}

func TestServer_UnknownToolCallIsRejected(t *testing.T) {
	cs := connectScoped(t, []Tool{noopTool("upload_evidence")})
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "dispatch_run"})
	if err == nil {
		t.Fatal("calling a tool outside the scoped surface must fail")
	}
}

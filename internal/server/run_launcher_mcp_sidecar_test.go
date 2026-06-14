package server

import (
	"fmt"
	"testing"
)

func sidecarReqSettings() (RunLaunchRequest, Settings) {
	runNumber := 3
	runDisplay := "3"
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "verify"},
		Run: RunReplayData{
			ID:               "168.3",
			Project:          "ambience",
			IssueNumber:      168,
			RunNumber:        &runNumber,
			RunDisplayNumber: &runDisplay,
		},
	}
	settings := Settings{
		RunnerNamespace:         "glimmung-runs",
		ArtifactsStorageAccount: "romaineglimmungartifacts",
		ArtifactsContainer:      "artifacts",
	}
	return req, settings
}

func rmcpPodSpec(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	return m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func rmcpContainerNames(t *testing.T, m map[string]any) []string {
	t.Helper()
	cs := rmcpPodSpec(t, m)["containers"].([]any)
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.(map[string]any)["name"].(string)
	}
	return out
}

func rmcpHasVolume(t *testing.T, m map[string]any, name string) bool {
	t.Helper()
	vs, _ := rmcpPodSpec(t, m)["volumes"].([]any)
	for _, v := range vs {
		if v.(map[string]any)["name"] == name {
			return true
		}
	}
	return false
}

func rmcpSidecarEnv(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	cs := rmcpPodSpec(t, m)["containers"].([]any)
	sidecar := cs[len(cs)-1].(map[string]any)
	out := map[string]string{}
	for _, e := range sidecar["env"].([]map[string]any) {
		out[e["name"].(string)] = fmt.Sprint(e["value"])
	}
	return out
}

func rmcpAgentEnv(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	agent := rmcpPodSpec(t, m)["containers"].([]any)[0].(map[string]any)
	out := map[string]string{}
	ev, _ := agent["env"].([]map[string]any)
	for _, e := range ev {
		out[e["name"].(string)] = fmt.Sprint(e["value"])
	}
	return out
}

func TestRunnerJobManifest_NoToolsHasNoSidecar(t *testing.T) {
	req, settings := sidecarReqSettings()

	m := runnerJobManifest(settings, req, RunnerJobSpec{ID: "verify", Managed: true}, "job", "secret", "attempt")
	if names := rmcpContainerNames(t, m); len(names) != 1 {
		t.Fatalf("job without tools must have exactly one container, got %v", names)
	}
	if rmcpHasVolume(t, m, "runner-workspace") {
		t.Fatal("job without tools must not get the runner-workspace volume (pod spec must be unchanged)")
	}
	if _, ok := rmcpAgentEnv(t, m)["GLIMMUNG_RUNNER_MCP_URL"]; ok {
		t.Fatal("job without tools must not set GLIMMUNG_RUNNER_MCP_URL on the agent container")
	}
}

func TestRunnerJobManifest_ToolsAddScopedSidecar(t *testing.T) {
	req, settings := sidecarReqSettings()

	m := runnerJobManifest(settings, req, RunnerJobSpec{ID: "verify", Managed: true, Tools: []string{"upload_evidence"}}, "job", "secret", "attempt")

	names := rmcpContainerNames(t, m)
	if len(names) != 2 || names[len(names)-1] != "runner-mcp" {
		t.Fatalf("job with tools must add the runner-mcp sidecar, got %v", names)
	}
	if !rmcpHasVolume(t, m, "runner-workspace") {
		t.Fatal("sidecar job must declare the shared runner-workspace volume")
	}

	env := rmcpSidecarEnv(t, m)
	if env["GLIMMUNG_RUNNER_TOOLS"] != "upload_evidence" {
		t.Fatalf("sidecar GLIMMUNG_RUNNER_TOOLS = %q, want upload_evidence", env["GLIMMUNG_RUNNER_TOOLS"])
	}
	if env["ARTIFACTS_STORAGE_ACCOUNT"] != "romaineglimmungartifacts" {
		t.Fatalf("sidecar ARTIFACTS_STORAGE_ACCOUNT = %q", env["ARTIFACTS_STORAGE_ACCOUNT"])
	}
	if env["GLIMMUNG_RUN_ID"] != "168.3" {
		t.Fatalf("sidecar GLIMMUNG_RUN_ID = %q", env["GLIMMUNG_RUN_ID"])
	}

	// The agent container must learn where the sidecar lives so the runner can
	// inject the agent's MCP config; the URL must match the addr the sidecar binds.
	agentEnv := rmcpAgentEnv(t, m)
	if agentEnv["GLIMMUNG_RUNNER_MCP_URL"] != "http://127.0.0.1:8765/mcp" {
		t.Fatalf("agent GLIMMUNG_RUNNER_MCP_URL = %q, want http://127.0.0.1:8765/mcp", agentEnv["GLIMMUNG_RUNNER_MCP_URL"])
	}
	if got := env["GLIMMUNG_RUNNER_MCP_ADDR"]; got != "127.0.0.1:8765" {
		t.Fatalf("sidecar GLIMMUNG_RUNNER_MCP_ADDR = %q, want 127.0.0.1:8765 (must match the agent URL host)", got)
	}

	// Both containers must mount the shared workspace so the agent's output is
	// readable by the sidecar tools.
	for _, c := range rmcpPodSpec(t, m)["containers"].([]any) {
		mounts, _ := c.(map[string]any)["volumeMounts"].([]any)
		found := false
		for _, vm := range mounts {
			if vm.(map[string]any)["name"] == "runner-workspace" {
				found = true
			}
		}
		if !found {
			t.Fatalf("container %v must mount runner-workspace", c.(map[string]any)["name"])
		}
	}
}

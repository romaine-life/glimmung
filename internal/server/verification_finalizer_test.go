package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationFinalizeRunScriptWritesTypedCompletion(t *testing.T) {
	workdir := t.TempDir()
	artifacts := filepath.Join(workdir, "artifacts")
	screenshots := filepath.Join(artifacts, "screenshots")
	if err := os.MkdirAll(screenshots, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(screenshots, "default.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "verification.json"), []byte(`{
		"status":"pass",
		"notes":"verified tooltip",
		"evidence_results":[{"evidence_id":"tooltip","kind":"screenshot","passed":true,"artifact_paths":["default.png"]}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "verification.md"), []byte("## Test Process\nobserved tooltip"), 0o644); err != nil {
		t.Fatal(err)
	}
	completion := filepath.Join(workdir, "completion.json")

	bin := filepath.Join(workdir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	azLog := filepath.Join(workdir, "az.log")
	if err := os.WriteFile(filepath.Join(bin, "az"), []byte("#!/usr/bin/env sh\nprintf '%s\\n' \"$*\" >> \"$AZ_FAKE_LOG\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(workdir, "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}

	runVerificationFinalizer(t, map[string]string{
		"PATH":                       bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GLIMMUNG_COMPLETION_FILE":   completion,
		"GLIMMUNG_PROJECT":           "proj",
		"GLIMMUNG_RUN_ID":            "run-1",
		"GLIMMUNG_RUN_REF":           "run-ref",
		"GLIMMUNG_WORKING_DIR":       workdir,
		"ARTIFACTS_STORAGE_ACCOUNT":  "acct",
		"ARTIFACTS_CONTAINER":        "artifacts",
		"AZURE_CLIENT_ID":            "client",
		"AZURE_TENANT_ID":            "tenant",
		"AZURE_FEDERATED_TOKEN_FILE": tokenFile,
		"AZ_FAKE_LOG":                azLog,
	})

	var payload struct {
		Verification        map[string]any `json:"verification"`
		SummaryMarkdown     string         `json:"summary_markdown"`
		ScreenshotsMarkdown string         `json:"screenshots_markdown"`
	}
	readJSONFile(t, completion, &payload)
	if payload.Verification["status"] != "pass" {
		t.Fatalf("verification=%#v", payload.Verification)
	}
	refs := stringValues(payload.Verification["evidence_refs"])
	if len(refs) != 1 || refs[0] != "runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("evidence_refs=%#v", refs)
	}
	if !strings.Contains(payload.SummaryMarkdown, "observed tooltip") {
		t.Fatalf("summary_markdown=%q", payload.SummaryMarkdown)
	}
	if !strings.Contains(payload.ScreenshotsMarkdown, "blob://artifacts/runs/proj/run-1/screenshots/default.png") {
		t.Fatalf("screenshots_markdown=%q", payload.ScreenshotsMarkdown)
	}
	azCalls, err := os.ReadFile(azLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(azCalls), "storage blob upload-batch") {
		t.Fatalf("az calls=%s", azCalls)
	}
}

func TestVerificationFinalizeRunScriptFailsClaimedScreenshotPassWithoutFile(t *testing.T) {
	workdir := t.TempDir()
	artifacts := filepath.Join(workdir, "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "verification.json"), []byte(`{
		"status":"pass",
		"evidence_results":[{"evidence_id":"tooltip","kind":"screenshot","passed":true,"artifact_paths":["missing.png"]}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	completion := filepath.Join(workdir, "completion.json")

	runVerificationFinalizer(t, map[string]string{
		"GLIMMUNG_COMPLETION_FILE":  completion,
		"GLIMMUNG_PROJECT":          "proj",
		"GLIMMUNG_RUN_ID":           "run-1",
		"GLIMMUNG_RUN_REF":          "run-ref",
		"GLIMMUNG_WORKING_DIR":      workdir,
		"ARTIFACTS_STORAGE_ACCOUNT": "acct",
		"ARTIFACTS_CONTAINER":       "artifacts",
	})

	var payload struct {
		Verification map[string]any `json:"verification"`
	}
	readJSONFile(t, completion, &payload)
	if payload.Verification["status"] != "fail" {
		t.Fatalf("verification=%#v", payload.Verification)
	}
	reasons := strings.Join(stringValues(payload.Verification["reasons"]), "\n")
	if !strings.Contains(reasons, "claimed passed screenshot evidence") {
		t.Fatalf("reasons=%q", reasons)
	}
}

func runVerificationFinalizer(t *testing.T, env map[string]string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", verificationFinalizeRunScript)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verification finalizer failed: %v\n%s", err, out)
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, data)
	}
}

func stringValues(raw any) []string {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

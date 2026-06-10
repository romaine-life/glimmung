package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/hotswap"
)

// fakeK8sJobClient records the calls ApplyHotSwap makes against the
// k8s API surface. Lets the test assert on dispatched Job spec + the
// happy/failure paths without standing up a real k8s API.
type fakeK8sJobClient struct {
	appliedJobs []map[string]any
	waitResult  string
	waitErr     error
	buildLogs   string
	swapLogs    string
	deleted     []string
}

func (f *fakeK8sJobClient) ApplyJob(_ context.Context, _ string, spec map[string]any) error {
	f.appliedJobs = append(f.appliedJobs, spec)
	return nil
}

func (f *fakeK8sJobClient) WaitForJob(_ context.Context, _ string, _ string, _ time.Duration) (string, error) {
	return f.waitResult, f.waitErr
}

func (f *fakeK8sJobClient) GetPodLogs(_ context.Context, _ string, _ string, container string) (string, error) {
	if container == "build" {
		return f.buildLogs, nil
	}
	return f.swapLogs, nil
}

func (f *fakeK8sJobClient) DeleteJob(_ context.Context, _ string, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

// TestApplyHotSwapHappyPathDispatchesJob asserts the Job spec carries
// the contract's builder_image, build_command, target, container, and
// pod selector — and that the swap script does pod resolution + tar-
// stream + SIGHUP via kubectl inside the alpine/k8s container (not
// from the glimmung pod, which has no kubectl).
func TestApplyHotSwapHappyPathDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "complete",
		buildLogs:  "build ok",
		swapLogs:   "swap ok",
	}
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:          "tank-operator",
		ArtifactKind:     "agent_runner",
		GitRef:           "feat/x",
		RepoURL:          "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace:  "tank-operator-slot-1-sessions",
		ValidationTarget: "new_session",
		JobNamespace:     "glimmung",
		Timeout:          30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			FidelityClassifier: hotswap.FidelityClassifierContract{
				Enabled: true,
				Command: "node scripts/classify-tank-test-fidelity.mjs",
			},
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "agent-runner/dist",
				Target:       "/var/run/agent-runner-hot/dist",
				BuildCommand: "cd agent-runner && npm run build",
				PodSelector:  "tank-operator/session-id",
				Container:    "agent-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v (result %+v)", err, result)
	}
	if result.Outcome != "persisted" {
		t.Fatalf("outcome = %q, want persisted", result.Outcome)
	}
	if result.ValidationTarget != "new_session" {
		t.Fatalf("validation target = %q, want new_session", result.ValidationTarget)
	}
	if len(k8s.appliedJobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(k8s.appliedJobs))
	}

	// Marshal + grep the Job spec for the contract-shaped fields.
	jobJSON, _ := json.Marshal(k8s.appliedJobs[0])
	s := string(jobJSON)
	checks := []string{
		`"image":"node:20-alpine"`,     // builder_image
		"npm run build",                // build command
		`"image":"alpine/k8s:1.31.13"`, // default swap container
		`"glimmung.io/hot-swap-validation-target":"new_session"`,
		"node scripts/classify-tank-test-fidelity.mjs",
		"--validation-target",
		"--enforce",
		"kubectl -n 'tank-operator-slot-1-sessions'", // namespace into kubectl
		"tank-operator/session-id",                   // pod selector
		"tar c -C /work source",                      // tar-stream: archive the source dir from its parent
		`/var/run/agent-runner-hot/dist`,             // target path
		"tar x --strip-components=1 -f -",            // strip the source/ member so the non-root pod never chmods the root-owned mount dir
		"kill -HUP 1",                                // SIGHUP signal
		"feat/x",                                     // git ref
		// Post-swap verification: the Job must confirm a process is running the
		// streamed artifact under Target and fail loudly otherwise, so a
		// non-hot-swap-aware target image cannot report a false "persisted".
		"/proc/[0-9]*", // verification scans the target container's processes
		"grep -qF /var/run/agent-runner-hot/dist",           // a process must reference the artifact under Target
		"hot-swap reported success but did not take effect", // loud failure when the restart was a no-op
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
	// Cleanup ran
	if len(k8s.deleted) != 1 {
		t.Fatalf("delete jobs = %d, want 1", len(k8s.deleted))
	}
}

func TestApplyHotSwapCodexRunnerDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "complete",
		buildLogs:  "build ok",
		swapLogs:   "swap ok",
	}
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "codex_runner",
		GitRef:          "feat/codex",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "tank-operator-slot-1-sessions",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			CodexRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "codex-runner/dist",
				Target:       "/var/run/codex-runner-hot/dist",
				BuildCommand: "cd codex-runner && npm run build",
				PodSelector:  "tank-operator/session-id",
				Container:    "codex-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v (result %+v)", err, result)
	}
	if result.Outcome != "persisted" {
		t.Fatalf("outcome = %q, want persisted", result.Outcome)
	}
	if len(k8s.appliedJobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(k8s.appliedJobs))
	}

	jobJSON, _ := json.Marshal(k8s.appliedJobs[0])
	s := string(jobJSON)
	checks := []string{
		`"glimmung.io/apply-hot-swap-kind":"codex_runner"`,
		"codex-runner/dist",
		`/var/run/codex-runner-hot/dist`,
		"codex-runner",
		"kill -HUP 1",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
}

func TestApplyHotSwapAntigravityRunnerDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "complete",
		buildLogs:  "build ok",
		swapLogs:   "swap ok",
	}
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "antigravity_runner",
		GitRef:          "feat/antigravity",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "tank-operator-slot-1-sessions",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AntigravityRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "antigravity-runner/hot",
				Target:       "/var/run/antigravity-runner-hot",
				BuildCommand: "cd antigravity-runner && npm run build",
				PodSelector:  "tank-operator/session-id,tank-operator/mode=antigravity_gui",
				Container:    "antigravity-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-bookworm-slim",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v (result %+v)", err, result)
	}
	if result.Outcome != "persisted" {
		t.Fatalf("outcome = %q, want persisted", result.Outcome)
	}
	if len(k8s.appliedJobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(k8s.appliedJobs))
	}

	jobJSON, _ := json.Marshal(k8s.appliedJobs[0])
	s := string(jobJSON)
	checks := []string{
		`"glimmung.io/apply-hot-swap-kind":"antigravity_runner"`,
		"antigravity-runner/hot",
		`/var/run/antigravity-runner-hot`,
		"antigravity-runner",
		"antigravity_gui",
		"kill -HUP 1",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
}

func TestApplyHotSwapRejectsUnsupportedKind(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	for _, kind := range []string{"static", "backend", "", "frontend"} {
		_, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
			ArtifactKind: kind,
			Contract:     hotswap.Contract{Enabled: true, AgentRunner: hotswap.AgentRunnerContract{Enabled: true, BuilderImage: "x"}},
		})
		if err == nil {
			t.Fatalf("kind %q should be rejected", kind)
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("kind %q error should say not supported; got %v", kind, err)
		}
	}
	if len(k8s.appliedJobs) != 0 {
		t.Fatal("no jobs should be applied on rejection")
	}
}

func TestApplyHotSwapRejectsMissingBuilderImage(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	_, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		ArtifactKind:    "agent_runner",
		TargetNamespace: "ns",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		Contract: hotswap.Contract{
			Enabled:     true,
			AgentRunner: hotswap.AgentRunnerContract{Enabled: true /* BuilderImage empty */},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "builder_image") {
		t.Fatalf("err = %v, want builder_image-named error", err)
	}
}

// TestApplyHotSwapJobFailureSurfacesLogs asserts that when WaitForJob
// returns failed, the result Outcome is build_failed/swap_failed and
// the relevant log tail is in the response.
func TestApplyHotSwapJobFailureSurfacesLogs(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "failed",
		waitErr:    errors.New("job failed: BackoffLimitExceeded"),
		buildLogs:  "npm ERR! missing script: build",
		swapLogs:   "",
	}
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		ArtifactKind:    "agent_runner",
		GitRef:          "main",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "ns",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled: true, Source: "x", Target: "/x", BuildCommand: "true",
				PodSelector: "k=v", Container: "c", Restart: "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err == nil {
		t.Fatal("expected error from failed Job")
	}
	if result.Outcome != "build_failed" {
		t.Fatalf("outcome = %q, want build_failed (build logs contain 'ERR', swap logs empty)", result.Outcome)
	}
	if result.BuildLogsTail == "" {
		t.Fatal("build logs tail should be populated on failure")
	}
}

// TestApplyHotSwapJobTimeoutSurfaces asserts the timeout outcome label.
func TestApplyHotSwapJobTimeoutSurfaces(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "timeout",
		waitErr:    errors.New("job did not complete within 30s"),
	}
	result, _ := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		ArtifactKind:    "agent_runner",
		GitRef:          "main",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "ns",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled: true, Source: "x", Target: "/x", BuildCommand: "true",
				PodSelector: "k=v", Container: "c", Restart: "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if result.Outcome != "timeout" {
		t.Fatalf("outcome = %q, want timeout", result.Outcome)
	}
}

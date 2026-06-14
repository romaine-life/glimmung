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

// TestApplyHotSwapStaticDispatchesJob asserts the static path: build from
// the ref, clear the override dir, tar-stream dist into every matched app
// replica in the slot's APP namespace, and send NO restart (static is
// served live from the override dir).
func TestApplyHotSwapStaticDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{
		waitResult: "complete",
		buildLogs:  "build ok",
		swapLogs:   "swap ok",
	}
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "static",
		GitRef:          "feat/ui",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "tank-operator-slot-1", // app namespace, NOT -sessions
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			Static: hotswap.StaticContract{
				Enabled:      true,
				Source:       "frontend/dist",
				Target:       "/var/run/tank-operator-static-override",
				BuildCommand: "cd frontend && npm ci && npm run build",
				PodSelector:  "app.kubernetes.io/name=tank-operator",
				Container:    "tank-operator",
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
		`"glimmung.io/apply-hot-swap-kind":"static"`,
		`"image":"node:20-alpine"`,               // builder image
		"npm ci",                                 // build command (install; && is JSON-escaped)
		"npm run build",                          // build command (build)
		"frontend/dist",                          // source copied out of the build
		"kubectl -n 'tank-operator-slot-1'",      // app namespace, not -sessions
		"app.kubernetes.io/name=tank-operator",   // pod selector
		"-c 'tank-operator'",                     // target container
		"/var/run/tank-operator-static-override", // override target
		"rm -rf",                                 // clean step before extract
		"tar c -C /work source",                  // tar-stream
		"tar x --strip-components=1 -f -",        // strip source/ member
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
	// Static is served live — no restart signal must be sent.
	if strings.Contains(s, "kill -HUP 1") {
		t.Errorf("static swap must not send a restart signal; spec=%s", s)
	}
}

// TestApplyHotSwapFetchesRefWithAuth pins the clone fix: the build Job
// authenticates via GIT_ASKPASS (token never on a command line / in set -x)
// and fetches the ref by name OR sha. `git clone --branch <sha>` is invalid,
// and the restricted-mode gate pins git_ref to the verified HEAD sha, so the
// Job must fetch-by-ref, not clone --branch.
func TestApplyHotSwapFetchesRefWithAuth(t *testing.T) {
	k8s := &fakeK8sJobClient{waitResult: "complete", buildLogs: "ok", swapLogs: "ok"}
	sha := "f3771d1cca46d3c9e6f931e8ca5b52a486947d3a"
	result, err := ApplyHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "agent_runner",
		GitRef:          sha, // gate pins to a sha; --branch <sha> would fail
		RepoURL:         "https://x-access-token@github.com/romaine-life/tank-operator.git",
		RepoToken:       "ghs_testtoken",
		TargetNamespace: "tank-operator-slot-1-sessions",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled: true, Source: "claude-runner/hot", Target: "/var/run/claude-runner-hot",
				BuildCommand: "true", PodSelector: "k=v", Container: "claude-runner",
				Restart: "SIGHUP", BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if result.Outcome != "persisted" {
		t.Fatalf("outcome = %q, want persisted", result.Outcome)
	}
	jobJSON, _ := json.Marshal(k8s.appliedJobs[0])
	s := string(jobJSON)
	for _, c := range []string{
		"git fetch --depth=1 origin", // fetch-by-ref (works for sha or branch)
		"git checkout -q FETCH_HEAD",
		"GIT_ASKPASS", // token fed via askpass, not the command line
		"gitaskpass",
		`"name":"GIT_TOKEN"`, // token passed as a Job env
		"ghs_testtoken",
	} {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
	if strings.Contains(s, "--branch") {
		t.Errorf("build script must not use `git clone --branch` (breaks on a sha); spec=%s", s)
	}
}

func TestApplyHotSwapRejectsUnsupportedKind(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	for _, kind := range []string{"backend", "", "frontend"} {
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

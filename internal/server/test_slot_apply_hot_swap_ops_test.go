package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/hotswap"
)

// fakeK8sJobClient records the calls DispatchHotSwap + finalizeHotSwap make
// against the k8s API surface. Lets the test assert on the dispatched Job spec
// and the finalize log/classify paths without standing up a real k8s API.
type fakeK8sJobClient struct {
	appliedJobs []map[string]any
	buildLogs   string
	swapLogs    string
	deleted     []string
}

func (f *fakeK8sJobClient) ApplyJob(_ context.Context, _ string, spec map[string]any) error {
	f.appliedJobs = append(f.appliedJobs, spec)
	return nil
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

// TestDispatchHotSwapHappyPathDispatchesJob asserts the Job spec carries the
// contract's builder_image, build_command, target, container, and pod selector
// — and that the swap script does pod resolution + tar-stream + SIGHUP via
// kubectl inside the alpine/k8s container (not from the glimmung pod, which has
// no kubectl). The dispatch returns "running" and does NOT wait or delete: the
// gated finalizer owns the terminal outcome and Job cleanup.
func TestDispatchHotSwapHappyPathDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
	}
	if result.JobName == "" {
		t.Fatal("dispatch must return a job handle")
	}
	if result.ValidationTarget != "new_session" {
		t.Fatalf("validation target = %q, want new_session", result.ValidationTarget)
	}
	if len(k8s.appliedJobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(k8s.appliedJobs))
	}
	// Dispatch must not delete the Job — the finalizer does that once it is
	// terminal. A delete here would race the build to death.
	if len(k8s.deleted) != 0 {
		t.Fatalf("dispatch deleted jobs = %d, want 0 (finalizer owns cleanup)", len(k8s.deleted))
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
		`"activeDeadlineSeconds":30`,                 // timeout enforced on the Job, not a held request
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
}

func TestDispatchHotSwapCodexRunnerDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
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

func TestDispatchHotSwapAntigravityRunnerDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
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

// TestDispatchHotSwapBackendDispatchesJob asserts the backend path: build a
// single binary from the ref, stream it onto the supervisor's hot-artifact file
// in the slot's APP namespace, SIGHUP PID 1, then health-gate the re-exec inside
// the pod. It must NOT use the runner/static dir-extract path.
func TestDispatchHotSwapBackendDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "backend",
		GitRef:          "feat/x",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "tank-operator-slot-1", // app ns, not -sessions
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			Backend: hotswap.BackendContract{
				Enabled:      true,
				Strategy:     "supervisor",
				BuildCommand: "cd backend-go && go build -o /tmp/tank-operator-go ./cmd/tank-operator",
				Artifact:     "/tmp/tank-operator-go",
				Target:       "/var/run/tank-operator-hot/tank-operator-go",
				HealthPath:   "/healthz",
				HealthPort:   8000,
				PodSelector:  "app.kubernetes.io/name=tank-operator",
				Container:    "tank-operator",
				BuilderImage: "golang:1.26-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v (result %+v)", err, result)
	}
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
	}
	if len(k8s.appliedJobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(k8s.appliedJobs))
	}
	jobJSON, _ := json.Marshal(k8s.appliedJobs[0])
	s := string(jobJSON)
	mustContain := []string{
		`"glimmung.io/apply-hot-swap-kind":"backend"`,
		"golang:1.26-alpine",                               // builder image
		"app.kubernetes.io/name=tank-operator",             // app-pod selector
		"/work/artifact",                                   // single-file build surface + stdin source
		"exec -i",                                          // stdin stream of the one binary
		"/var/run/tank-operator-hot/tank-operator-go.next", // atomic staging file
		"chmod +x /var/run/tank-operator-hot/tank-operator-go.next",
		"mv -f /var/run/tank-operator-hot/tank-operator-go.next /var/run/tank-operator-hot/tank-operator-go",
		"kill -HUP 1",
		"http://127.0.0.1:8000/healthz", // in-pod health gate
	}
	for _, c := range mustContain {
		if !strings.Contains(s, c) {
			t.Errorf("backend Job spec missing %q\nspec=%s", c, s)
		}
	}
	if strings.Contains(s, "tar x --strip-components=1") {
		t.Errorf("backend Job spec must not use the dir-extract path\nspec=%s", s)
	}
}

// TestDispatchHotSwapStaticDispatchesJob asserts the static path: build from the
// ref, clear the override dir, tar-stream dist into every matched app replica in
// the slot's APP namespace, and send NO restart (static is served live).
func TestDispatchHotSwapStaticDispatchesJob(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
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

// TestDispatchHotSwapPlumbsDiffContextAndSlotLabel pins the issue-3 wiring: the
// classifier's diff context (base/head/changed-files) reaches the build
// container as env, and the slot-name label the finalizer joins on is stamped on
// the Job.
func TestDispatchHotSwapPlumbsDiffContextAndSlotLabel(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	_, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "tank-operator",
		ArtifactKind:    "static",
		GitRef:          "feat/ui",
		RepoURL:         "https://github.com/romaine-life/tank-operator.git",
		TargetNamespace: "tank-operator-slot-1",
		SlotName:        "tank-operator-slot-1",
		JobNamespace:    "glimmung",
		Timeout:         600 * time.Second,
		BaseRef:         "main",
		HeadRef:         "feat/ui",
		ChangedFiles:    []string{"frontend/src/App.tsx", "backend-go/cmd/tank-operator/server.go"},
		Contract: hotswap.Contract{
			Enabled: true,
			Static: hotswap.StaticContract{
				Enabled: true, Source: "frontend/dist", Target: "/var/run/o",
				BuildCommand: "true", PodSelector: "app.kubernetes.io/name=tank-operator",
				Container: "tank-operator", BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(mustJSON(t, k8s.appliedJobs[0]))
	for _, c := range []string{
		`"name":"GLIMMUNG_BASE_REF","value":"main"`,
		`"name":"GLIMMUNG_HEAD_REF","value":"feat/ui"`,
		`"name":"GLIMMUNG_CHANGED_FILES"`,
		"frontend/src/App.tsx",                                // changed file present in env
		`"glimmung.io/slot-name":"tank-operator-slot-1"`,      // finalizer join key
		`"activeDeadlineSeconds":600`,                         // honored timeout_seconds
	} {
		if !strings.Contains(s, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, s)
		}
	}
}

// TestDispatchHotSwapFetchesRefWithAuth pins the clone fix: the build Job
// authenticates via GIT_ASKPASS (token never on a command line / in set -x) and
// fetches the ref by name OR sha.
func TestDispatchHotSwapFetchesRefWithAuth(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	sha := "f3771d1cca46d3c9e6f931e8ca5b52a486947d3a"
	result, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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
	if result.Outcome != "running" {
		t.Fatalf("outcome = %q, want running", result.Outcome)
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

func TestDispatchHotSwapRejectsUnsupportedKind(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	for _, kind := range []string{"", "frontend", "image"} {
		_, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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

func TestDispatchHotSwapRejectsMissingBuilderImage(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	_, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
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

// TestFinalizeHotSwapClassifiesOutcome pins the terminal classification the
// gated finalizer applies to a completed Job: success → persisted; a deadline
// overrun → timeout; a failure with empty swap logs (or an errored build) →
// build_failed; a failure that reached the swap container → swap_failed.
func TestFinalizeHotSwapClassifiesOutcome(t *testing.T) {
	cases := []struct {
		name          string
		succeeded     bool
		failureReason string
		buildLogs     string
		swapLogs      string
		want          string
	}{
		{name: "success", succeeded: true, buildLogs: "build ok", swapLogs: "swap ok", want: "persisted"},
		{name: "deadline", succeeded: false, failureReason: "DeadlineExceeded", buildLogs: "...", swapLogs: "", want: "timeout"},
		{name: "build error empty swap", succeeded: false, failureReason: "BackoffLimitExceeded", buildLogs: "npm ERR! missing script: build", swapLogs: "", want: "build_failed"},
		{name: "swap reached", succeeded: false, failureReason: "BackoffLimitExceeded", buildLogs: "build ok", swapLogs: "kubectl: no pods matched", want: "swap_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := &fakeK8sJobClient{buildLogs: tc.buildLogs, swapLogs: tc.swapLogs}
			result := finalizeHotSwap(context.Background(), k8s, finalizeHotSwapInputs{
				JobName:          "apply-hot-swap-abc",
				JobNamespace:     "glimmung-runs",
				ArtifactKind:     "static",
				ValidationTarget: "existing_session",
			}, tc.succeeded, tc.failureReason)
			if result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
			if tc.buildLogs != "" && result.BuildLogsTail == "" {
				t.Error("build logs tail should be populated")
			}
			if !tc.succeeded && result.Error == "" {
				t.Error("a non-success outcome should carry an error string")
			}
		})
	}
}

// TestDispatchHotSwapSubstitutesSlotNameInSelectorAndContainer pins the
// {slot_name} substitution for projects (e.g. chess-tactics) whose app pods are
// labeled and whose container is named by the slot name rather than a static
// label.
func TestDispatchHotSwapSubstitutesSlotNameInSelectorAndContainer(t *testing.T) {
	k8s := &fakeK8sJobClient{}
	_, err := DispatchHotSwap(context.Background(), k8s, ApplyHotSwapOptions{
		Project:         "chess-tactics",
		ArtifactKind:    "static",
		GitRef:          "feat/x",
		RepoURL:         "https://x-access-token@github.com/romaine-life/chess-tactics.git",
		RepoToken:       "ghs_x",
		TargetNamespace: "chess-tactics-1",
		SlotName:        "chess-tactics-1",
		JobNamespace:    "glimmung",
		Timeout:         30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			Static: hotswap.StaticContract{
				Enabled:      true,
				Source:       "frontend/dist",
				Target:       "/var/run/chess-tactics-static-override",
				BuildCommand: "cd frontend && npm ci && npm run build",
				PodSelector:  "app={slot_name}",
				Container:    "{slot_name}",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	js := string(mustJSON(t, k8s.appliedJobs[0]))
	if strings.Contains(js, "{slot_name}") {
		t.Errorf("{slot_name} token not substituted; spec=%s", js)
	}
	for _, c := range []string{"app=chess-tactics-1", "-c 'chess-tactics-1'"} {
		if !strings.Contains(js, c) {
			t.Errorf("Job spec missing %q\nspec=%s", c, js)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

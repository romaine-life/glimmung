package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
)

func TestRunnerJobManifestIncludesRunnerCallbackEnv(t *testing.T) {
	runNumber := 7
	runDisplay := "7"
	callback := "callback-token"
	timeout := 120
	leaseNumber := 3
	req := RunLaunchRequest{
		Lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: &leaseNumber,
			State:       "claimed",
			Metadata: map[string]any{
				"runner_slot_name":     "tank-operator-slot-1",
				"runner_slot_index":    "1",
				"entrypoint_job_id":    "test",
				"entrypoint_step_slug": "verify-ui",
				"evidence_requirements": []EvidenceRequirement{{
					Kind:  "video",
					Label: "primary browser flow",
				}},
				"phase_inputs": map[string]any{
					"target": "provision",
				},
				"agent_runtime": agentruntime.Snapshot{
					Default: agentruntime.ResolvedProfile{
						ProfileID:       "codex-deep",
						Provider:        agentruntime.ProviderCodex,
						Model:           "gpt-5.5",
						ReasoningEffort: "xhigh",
						Source:          "issue",
					},
				},
			},
		},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "verify"},
		Run: RunReplayData{
			ID:               "run-123",
			Project:          "tank-operator",
			IssueNumber:      42,
			RunNumber:        &runNumber,
			RunDisplayNumber: &runDisplay,
			CallbackToken:    &callback,
			RunInputs:        map[string]string{"git_ref": "codex/lifecycle-observe"},
			Attempts:         []RunAttemptData{{AttemptIndex: 1, Phase: "verify"}},
		},
	}

	manifest := runnerJobManifest(Settings{
		RunnerNamespace:         "glimmung-runs",
		RunnerServiceAccount:    "glimmung-runner",
		RunnerCallbackBaseURL:   "http://glimmung.glimmung.svc.cluster.local",
		RunnerPlaywrightEnabled: true,
		RunnerPlaywrightPort:    "3000",
	}, req, RunnerJobSpec{
		ID:             "test",
		Image:          "runner:latest",
		TimeoutSeconds: &timeout,
		Env: map[string]string{
			"AZURE_SUBSCRIPTION_ID": "sub-123",
			"GLIMMUNG_PROJECT":      "must-not-override",
		},
	}, "job", "secret", "attempt")

	env := runnerManifestEnv(manifest)
	if env["GLIMMUNG_COMPLETED_URL"] != "http://glimmung.glimmung.svc.cluster.local/v1/run-callbacks/callback-token/run/completed" {
		t.Fatalf("completed url=%q", env["GLIMMUNG_COMPLETED_URL"])
	}
	if _, ok := env["GLIMMUNG_FAILED_URL"]; ok {
		t.Fatal("failed callback URL should not be injected")
	}
	if env["GLIMMUNG_GITHUB_TOKEN_URL"] == "" {
		t.Fatal("expected GitHub token URL")
	}
	if env["GLIMMUNG_ATTEMPT_INDEX"] != "1" {
		t.Fatalf("attempt index=%q", env["GLIMMUNG_ATTEMPT_INDEX"])
	}
	if env["GLIMMUNG_INPUT_TARGET"] != "provision" {
		t.Fatalf("phase input env=%q", env["GLIMMUNG_INPUT_TARGET"])
	}
	if env["GLIMMUNG_RUN_INPUT_GIT_REF"] != "codex/lifecycle-observe" {
		t.Fatalf("run input env=%q", env["GLIMMUNG_RUN_INPUT_GIT_REF"])
	}
	if env["GLIMMUNG_VALIDATION_NAMESPACE"] != "tank-operator-slot-1" {
		t.Fatalf("validation namespace=%q", env["GLIMMUNG_VALIDATION_NAMESPACE"])
	}
	if env["GLIMMUNG_ENTRYPOINT_JOB_ID"] != "test" || env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"] != "verify-ui" {
		t.Fatalf("entrypoint env job=%q step=%q", env["GLIMMUNG_ENTRYPOINT_JOB_ID"], env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"])
	}
	if !strings.Contains(env["GLIMMUNG_EVIDENCE_REQUIREMENTS_JSON"], `"kind":"video"`) {
		t.Fatalf("evidence requirements env=%q", env["GLIMMUNG_EVIDENCE_REQUIREMENTS_JSON"])
	}
	if !strings.Contains(env["GLIMMUNG_AGENT_RUNTIME_JSON"], `"profile_id":"codex-deep"`) {
		t.Fatalf("agent runtime env=%q", env["GLIMMUNG_AGENT_RUNTIME_JSON"])
	}
	if env["AZURE_SUBSCRIPTION_ID"] != "sub-123" {
		t.Fatalf("job env=%q", env["AZURE_SUBSCRIPTION_ID"])
	}
	if env["GLIMMUNG_PROJECT"] != "tank-operator" {
		t.Fatalf("system env was overridden: %q", env["GLIMMUNG_PROJECT"])
	}
	// Enforcement guard: the agent's main container must NOT receive a Playwright
	// WS endpoint, even though this lease has a slot browser. Browser evidence is
	// captured only through the credential-isolated runner-MCP sidecar capture
	// tools, which keep the endpoint (see the sidecar env test). Handing it to the
	// agent is what let per-repo capture scripts connect to the slot browser and
	// self-capture a white first frame; this fails if any of the three endpoint
	// vars are reintroduced into the agent env.
	for _, k := range []string{"PLAYWRIGHT_WS_ENDPOINT", "GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT", "PW_TEST_CONNECT_WS_ENDPOINT"} {
		if v, ok := env[k]; ok {
			t.Fatalf("agent env must not carry a Playwright endpoint, got %s=%q", k, v)
		}
	}
}

func TestResolveRunnerCheckoutRunInputs(t *testing.T) {
	phase := PhaseSpec{
		Name: "prepare",
		Jobs: []RunnerJobSpec{{
			ID:       "env-prep",
			Checkout: &RunnerCheckoutSpec{Repo: "owner/repo", Ref: "${{ inputs.git_ref }}", Path: "/workspace/repo"},
			ExtraCheckouts: []RunnerCheckoutSpec{{
				Repo: "owner/extra",
				Ref:  "${{ inputs.branch }}",
				Path: "/workspace/extra",
			}},
		}},
	}
	got, err := resolveRunnerCheckoutRunInputs(phase, map[string]string{
		"git_ref": "codex/lifecycle-observe",
		"branch":  "support-branch",
	})
	if err != nil {
		t.Fatalf("resolve checkout refs: %v", err)
	}
	if got.Jobs[0].Checkout.Ref != "codex/lifecycle-observe" {
		t.Fatalf("checkout ref=%q", got.Jobs[0].Checkout.Ref)
	}
	if got.Jobs[0].ExtraCheckouts[0].Ref != "support-branch" {
		t.Fatalf("extra checkout ref=%q", got.Jobs[0].ExtraCheckouts[0].Ref)
	}
	if phase.Jobs[0].Checkout.Ref != "${{ inputs.git_ref }}" {
		t.Fatalf("input phase mutated: %#v", phase.Jobs[0].Checkout)
	}
}

func TestResolveRunnerCheckoutRunInputsRequiresProvidedInput(t *testing.T) {
	phase := PhaseSpec{
		Name: "prepare",
		Jobs: []RunnerJobSpec{{
			ID:       "env-prep",
			Checkout: &RunnerCheckoutSpec{Ref: "${{ inputs.git_ref }}"},
		}},
	}
	if _, err := resolveRunnerCheckoutRunInputs(phase, nil); err == nil {
		t.Fatal("expected missing input error")
	}
}

// Teardown jobs (idempotent env-destroy) get a bounded backoffLimit so a
// transient pod-start blip self-heals instead of instantly failing the
// cleanup phase. Producer/verify jobs keep backoffLimit=0 (fail fast;
// Glimmung owns their retries at the attempt level).
func TestRunnerJobBackoffLimitTeardownAbsorbsTransientFailure(t *testing.T) {
	if got := runnerJobBackoffLimit(PhaseSpec{Name: "cleanup_early", Purpose: PhasePurposeTeardown}); got != runnerTeardownJobBackoffLimit {
		t.Fatalf("teardown backoffLimit=%d, want %d", got, runnerTeardownJobBackoffLimit)
	}
	if got := runnerJobBackoffLimit(PhaseSpec{Name: "llm-verify", Purpose: PhasePurposeVerification}); got != 0 {
		t.Fatalf("verify backoffLimit=%d, want 0 (fail fast)", got)
	}
	if got := runnerJobBackoffLimit(PhaseSpec{Name: "llm-work", Purpose: PhasePurposeWork}); got != 0 {
		t.Fatalf("producer backoffLimit=%d, want 0 (fail fast)", got)
	}
}

func TestRunnerJobManifestTeardownPhaseUsesBoundedBackoffLimit(t *testing.T) {
	runNumber := 13
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience", State: "claimed"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "cleanup_early", Purpose: PhasePurposeTeardown, Jobs: []RunnerJobSpec{{ID: "env-destroy"}}},
		Run:      RunReplayData{ID: "run-1", Project: "ambience", IssueNumber: 168, RunNumber: &runNumber, Attempts: []RunAttemptData{{AttemptIndex: 1, Phase: "cleanup_early"}}},
	}
	manifest := runnerJobManifest(Settings{RunnerNamespace: "glimmung-runs"}, req, RunnerJobSpec{ID: "env-destroy"}, "job", "secret", "attempt")
	spec, ok := manifest["spec"].(map[string]any)
	if !ok {
		t.Fatalf("manifest spec missing: %#v", manifest)
	}
	if spec["backoffLimit"] != runnerTeardownJobBackoffLimit {
		t.Fatalf("teardown manifest backoffLimit=%v, want %d", spec["backoffLimit"], runnerTeardownJobBackoffLimit)
	}
}

func TestRunnerJobManifestIncludesStringMapPhaseInputs(t *testing.T) {
	req := RunLaunchRequest{
		Lease: Lease{
			Project: "ambience",
			State:   "claimed",
			Metadata: map[string]any{
				"phase_inputs": map[string]string{
					"namespace":      "ambience-slot-1",
					"validation_url": "https://ambience-slot-1.ambience.dev.romaine.life",
				},
			},
		},
		Workflow: Workflow{Name: "default"},
		Phase:    PhaseSpec{Name: "llm-work"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			IssueRepo:     "romaine-life/ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-work"}},
		},
	}

	manifest := runnerJobManifest(Settings{
		RunnerNamespace:       "glimmung-runs",
		RunnerServiceAccount:  "glimmung-runner",
		RunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
	}, req, RunnerJobSpec{ID: "llm-test-plan", Image: "runner:latest"}, "job", "secret", "attempt")

	env := runnerManifestEnv(manifest)
	if env["GLIMMUNG_INPUT_NAMESPACE"] != "ambience-slot-1" {
		t.Fatalf("namespace input env=%q", env["GLIMMUNG_INPUT_NAMESPACE"])
	}
	if env["GLIMMUNG_INPUT_VALIDATION_URL"] != "https://ambience-slot-1.ambience.dev.romaine.life" {
		t.Fatalf("validation_url input env=%q", env["GLIMMUNG_INPUT_VALIDATION_URL"])
	}
}

func TestRunnerJobManifestManagedJobUsesSharedRunnerEntrypoint(t *testing.T) {
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "env-prep"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "env-prep"}},
		},
	}
	job := RunnerJobSpec{
		ID:               "prepare",
		Managed:          true,
		WorkingDirectory: "/workspace/ambience",
		Steps: []RunnerStepSpec{{
			Slug: "unit",
			Run:  "go test ./...",
		}},
	}

	manifest := runnerJobManifest(Settings{
		RunnerNamespace:       "glimmung-runs",
		RunnerServiceAccount:  "glimmung-runner",
		RunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		RunnerImage:           "romainecr.azurecr.io/glimmung-runner:test",
		RunnerEntrypoint:      "/runner/glimmung-runner",
	}, req, job, "job", "secret", "attempt")

	container := runnerManifestContainer(manifest)
	command, ok := container["command"].([]string)
	if !ok || len(command) != 1 || command[0] != "/runner/glimmung-runner" {
		t.Fatalf("command=%#v", container["command"])
	}
	if container["image"] != "romainecr.azurecr.io/glimmung-runner:test" {
		t.Fatalf("image=%#v", container["image"])
	}
	if _, ok := container["args"]; ok {
		t.Fatalf("managed runner should not receive legacy args: %#v", container["args"])
	}
	env := runnerManifestEnv(manifest)
	var got RunnerJobSpec
	if err := json.Unmarshal([]byte(env["GLIMMUNG_RUNNER_JOB_SPEC"]), &got); err != nil {
		t.Fatalf("runner spec JSON: %v", err)
	}
	if !got.Managed || got.ID != "prepare" || got.WorkingDirectory != "/workspace/ambience" {
		t.Fatalf("runner spec=%#v", got)
	}
	if len(got.Steps) != 1 || got.Steps[0].Run != "go test ./..." {
		t.Fatalf("runner steps=%#v", got.Steps)
	}
}

func TestRunnerJobManifestDoesNotMountProviderCredentialSecret(t *testing.T) {
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "llm-work"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			IssueRepo:     "romaine-life/ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-work"}},
		},
	}
	job := RunnerJobSpec{
		ID:      "implement",
		Managed: true,
		Steps: []RunnerStepSpec{{
			Slug:  "run-agent",
			Agent: &AgentStepSpec{Slot: "implementation"},
		}},
	}
	manifest := runnerJobManifest(Settings{
		RunnerNamespace:       "glimmung-runs",
		RunnerServiceAccount:  "glimmung-runner",
		RunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		RunnerImage:           "romainecr.azurecr.io/glimmung-runner:test",
	}, req, job, "job", "secret", "attempt")

	podSpec := runnerManifestPodSpec(manifest)
	for _, volume := range podSpec["volumes"].([]any) {
		if volume.(map[string]any)["name"] == "codex-credentials" {
			t.Fatalf("runner jobs must not mount real provider credentials: %#v", podSpec["volumes"])
		}
	}
	container := runnerManifestContainer(manifest)
	for _, mount := range container["volumeMounts"].([]any) {
		if mount.(map[string]any)["name"] == "codex-credentials" {
			t.Fatalf("runner jobs must not mount real provider credentials: %#v", container["volumeMounts"])
		}
	}
}

func TestRunnerJobManifestWiresProviderAPIProxyForAgentJob(t *testing.T) {
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "llm-work"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			IssueRepo:     "romaine-life/ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-work"}},
		},
	}
	job := RunnerJobSpec{
		ID:      "implement",
		Managed: true,
		Steps: []RunnerStepSpec{{
			Slug:  "run-agent",
			Agent: &AgentStepSpec{Slot: "implementation"},
		}},
	}
	proxyRuntime := providerAPIProxyRuntime{
		ClaudeClusterIP: "172.16.1.10",
		CodexClusterIP:  "172.16.1.11",
		GitHubClusterIP: "172.16.1.12",
		CASecretName:    "glimmung-provider-api-proxy-ca",
		CABundlePath:    "/etc/glimmung-provider-api-proxy-bundle/ca-certificates.crt",
	}
	manifest := runnerJobManifest(Settings{
		RunnerNamespace:       "glimmung-runs",
		RunnerServiceAccount:  "glimmung-runner",
		RunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		RunnerImage:           "romainecr.azurecr.io/glimmung-runner:test",
	}, req, job, "job", "secret", "attempt", proxyRuntime)

	podSpec := runnerManifestPodSpec(manifest)
	hostAliases := podSpec["hostAliases"].([]any)
	if len(hostAliases) != 3 {
		t.Fatalf("hostAliases=%#v", hostAliases)
	}
	if hostAliases[0].(map[string]any)["ip"] != "172.16.1.10" || hostAliases[1].(map[string]any)["ip"] != "172.16.1.11" || hostAliases[2].(map[string]any)["ip"] != "172.16.1.12" {
		t.Fatalf("hostAliases=%#v", hostAliases)
	}
	if _, ok := podSpec["initContainers"]; !ok {
		t.Fatalf("expected CA bundle init container: %#v", podSpec)
	}
	env := runnerManifestEnv(manifest)
	if env["NODE_EXTRA_CA_CERTS"] != "/etc/glimmung-provider-api-proxy-ca/ca.crt" {
		t.Fatalf("NODE_EXTRA_CA_CERTS=%q", env["NODE_EXTRA_CA_CERTS"])
	}
	if env["SSL_CERT_FILE"] != proxyRuntime.CABundlePath {
		t.Fatalf("SSL_CERT_FILE=%q", env["SSL_CERT_FILE"])
	}
	if env["GLIMMUNG_PROVIDER_API_PROXY_CLAUDE_IP"] != "172.16.1.10" {
		t.Fatalf("GLIMMUNG_PROVIDER_API_PROXY_CLAUDE_IP=%q", env["GLIMMUNG_PROVIDER_API_PROXY_CLAUDE_IP"])
	}
	if env["GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP"] != "172.16.1.11" {
		t.Fatalf("GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP=%q", env["GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP"])
	}
	if env["GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP"] != "172.16.1.12" {
		t.Fatalf("GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP=%q", env["GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP"])
	}
}

func TestLaunchPhaseResolvesProviderAPIProxyForAgentJobs(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	var postedJob string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:                    "https://kube.test",
			K8sSATokenPath:                tokenPath,
			RunnerNamespace:               "glimmung-runs",
			RunnerServiceAccount:          "glimmung-runner",
			RunnerCallbackBaseURL:         "http://glimmung.glimmung.svc.cluster.local",
			RunnerImage:                   "romainecr.azurecr.io/glimmung-runner:test",
			ProviderAPIProxyNamespace:     "glimmung-runs",
			ProviderAPIProxyCASecret:      "glimmung-provider-api-proxy-ca",
			ProviderAPIProxyCABundlePath:  "/etc/glimmung-provider-api-proxy-bundle/ca-certificates.crt",
			ProviderAPIProxyClaudeService: "claude-api-proxy",
			ProviderAPIProxyCodexService:  "codex-api-proxy",
			ProviderAPIProxyGitHubService: "github-git-policy-proxy",
			GitHubAppPrivateKey:           "signing-key",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			switch req.Method + " " + req.URL.Path {
			case "GET /api/v1/namespaces/glimmung-runs/services/claude-api-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.10"}}`
			case "GET /api/v1/namespaces/glimmung-runs/services/codex-api-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.11"}}`
			case "GET /api/v1/namespaces/glimmung-runs/services/github-git-policy-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.12"}}`
			case "POST /apis/batch/v1/namespaces/glimmung-runs/jobs":
				raw, _ := io.ReadAll(req.Body)
				postedJob = string(raw)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase: PhaseSpec{
			Name: "llm-work",
			Jobs: []RunnerJobSpec{{
				ID:      "implement",
				Managed: true,
				Steps: []RunnerStepSpec{{
					Slug:  "run-agent",
					Agent: &AgentStepSpec{Slot: "implementation"},
				}},
			}},
		},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			IssueRepo:     "romaine-life/ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-work"}},
		},
	}

	if _, err := launcher.LaunchPhase(context.Background(), req); err != nil {
		t.Fatalf("LaunchPhase: %v", err)
	}
	for _, want := range []string{
		"GET /api/v1/namespaces/glimmung-runs/services/claude-api-proxy",
		"GET /api/v1/namespaces/glimmung-runs/services/codex-api-proxy",
		"GET /api/v1/namespaces/glimmung-runs/services/github-git-policy-proxy",
		"POST /api/v1/namespaces/glimmung-runs/secrets",
		"POST /apis/batch/v1/namespaces/glimmung-runs/jobs",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s, paths=%#v", want, paths)
		}
	}
	for _, want := range []string{
		`"ip":"172.16.1.10"`,
		`"api.anthropic.com"`,
		`"ip":"172.16.1.11"`,
		`"chatgpt.com"`,
		`"api.openai.com"`,
		`"github.com"`,
		`"secretName":"glimmung-provider-api-proxy-ca"`,
		`"name":"SSL_CERT_FILE"`,
		`"name":"GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP"`,
		`"name":"GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP"`,
		`"name":"GLIMMUNG_GITHUB_AGENT_TOKEN_URL"`,
		`"glimmung.romaine.life/github-policy-repo"`,
		`"glimmung.romaine.life/github-policy-ref"`,
	} {
		if !strings.Contains(postedJob, want) {
			t.Fatalf("posted job missing %q: %s", want, postedJob)
		}
	}
	if strings.Contains(postedJob, "codex-credentials") || strings.Contains(postedJob, "/etc/codex-creds") {
		t.Fatalf("posted job kept retired provider credential mount: %s", postedJob)
	}
}

func TestLaunchPhaseResolvesProviderAPIProxyForVerificationPhase(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	var postedJob string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:                    "https://kube.test",
			K8sSATokenPath:                tokenPath,
			RunnerNamespace:               "glimmung-runs",
			RunnerServiceAccount:          "glimmung-runner",
			RunnerCallbackBaseURL:         "http://glimmung.glimmung.svc.cluster.local",
			RunnerImage:                   "romainecr.azurecr.io/glimmung-runner:test",
			ProviderAPIProxyNamespace:     "glimmung-runs",
			ProviderAPIProxyCASecret:      "glimmung-provider-api-proxy-ca",
			ProviderAPIProxyCABundlePath:  "/etc/glimmung-provider-api-proxy-bundle/ca-certificates.crt",
			ProviderAPIProxyClaudeService: "claude-api-proxy",
			ProviderAPIProxyCodexService:  "codex-api-proxy",
			ProviderAPIProxyGitHubService: "github-git-policy-proxy",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			switch req.Method + " " + req.URL.Path {
			case "GET /api/v1/namespaces/glimmung-runs/services/claude-api-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.10"}}`
			case "GET /api/v1/namespaces/glimmung-runs/services/codex-api-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.11"}}`
			case "GET /api/v1/namespaces/glimmung-runs/services/github-git-policy-proxy":
				body = `{"spec":{"clusterIP":"172.16.1.12"}}`
			case "POST /apis/batch/v1/namespaces/glimmung-runs/jobs":
				raw, _ := io.ReadAll(req.Body)
				postedJob = string(raw)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "default"},
		Phase: PhaseSpec{
			Name:   "llm-verify",
			Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"},
			Jobs: []RunnerJobSpec{{
				ID:      "llm-verify",
				Managed: true,
				Steps: []RunnerStepSpec{{
					Slug: "run-verification",
					Run:  "/bin/bash /workspace/ambience/scripts/glimmung-runner/verify.sh",
				}},
			}},
		},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-verify"}},
		},
	}

	if _, err := launcher.LaunchPhase(context.Background(), req); err != nil {
		t.Fatalf("LaunchPhase: %v", err)
	}
	for _, want := range []string{
		"GET /api/v1/namespaces/glimmung-runs/services/claude-api-proxy",
		"GET /api/v1/namespaces/glimmung-runs/services/codex-api-proxy",
		"GET /api/v1/namespaces/glimmung-runs/services/github-git-policy-proxy",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s, paths=%#v", want, paths)
		}
	}
	for _, want := range []string{
		`"api.anthropic.com"`,
		`"api.openai.com"`,
		`"name":"GLIMMUNG_PROVIDER_API_PROXY_CLAUDE_IP"`,
		`"name":"GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP"`,
	} {
		if !strings.Contains(postedJob, want) {
			t.Fatalf("posted job missing %q: %s", want, postedJob)
		}
	}
	if strings.Contains(postedJob, "github.com") || strings.Contains(postedJob, "GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP") {
		t.Fatalf("verification job should not route github.com through policy proxy: %s", postedJob)
	}
}

func TestRunnerJobManifestEvidenceGateUsesManagedRunner(t *testing.T) {
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "default"},
		Phase: PhaseSpec{
			Name:                     "evidence-gate",
			EvidenceVerificationGate: true,
			Jobs: []RunnerJobSpec{{
				ID:      "legacy-gate",
				Image:   "python:3.12-slim",
				Command: []string{"python", "-c"},
				Args:    []string{"exit(1)"},
			}},
		},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			IssueNumber:   42,
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 3, Phase: "evidence-gate"}},
		},
	}

	manifest := runnerJobManifest(Settings{
		RunnerNamespace:       "glimmung-runs",
		RunnerServiceAccount:  "glimmung-runner",
		RunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		RunnerImage:           "romainecr.azurecr.io/glimmung-runner:test",
		RunnerEntrypoint:      "/app/glimmung-runner",
	}, req, req.Phase.Jobs[0], "job", "secret", "attempt")

	container := runnerManifestContainer(manifest)
	if container["image"] != "romainecr.azurecr.io/glimmung-runner:test" {
		t.Fatalf("image=%#v", container["image"])
	}
	command, ok := container["command"].([]string)
	if !ok || len(command) != 1 || command[0] != "/app/glimmung-runner" {
		t.Fatalf("command=%#v", container["command"])
	}
	if _, ok := container["args"]; ok {
		t.Fatalf("evidence gate should not receive legacy args: %#v", container["args"])
	}
	env := runnerManifestEnv(manifest)
	var got RunnerJobSpec
	if err := json.Unmarshal([]byte(env["GLIMMUNG_RUNNER_JOB_SPEC"]), &got); err != nil {
		t.Fatalf("runner spec JSON: %v", err)
	}
	if got.ID != "legacy-gate" || !got.Managed || len(got.Steps) != 1 || got.Steps[0].Slug != EvidenceGateStepSlug {
		t.Fatalf("runner spec=%#v", got)
	}
}

func TestReturnTestSlotRuntimeDoesNotDeleteNamespaces(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:          "https://kube.test",
			K8sSATokenPath:      tokenPath,
			RunnerNamespace:     "glimmung-runs",
			RunnerJobTTLSeconds: 3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := ""
			if req.Method == http.MethodGet {
				body = `{"items":[]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":          "tank-slot-1",
			"runner_slot_index":         "1",
			"runner_sessions_namespace": "tank-slot-1-sessions",
		},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, Project{Name: "tank"}); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, path := range paths {
		if path == "DELETE /api/v1/namespaces/tank-slot-1" || path == "DELETE /api/v1/namespaces/tank-slot-1-sessions" {
			t.Fatalf("return should not delete slot namespaces, saw %s in %#v", path, paths)
		}
	}
	if !containsPath(paths, "DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/slot-playwright") {
		t.Fatalf("return should delete slot Playwright deployment, paths=%#v", paths)
	}
	if !containsPath(paths, "DELETE /api/v1/namespaces/tank-slot-1/services/slot-playwright") {
		t.Fatalf("return should delete slot Playwright service, paths=%#v", paths)
	}
}

func TestReturnTestSlotRuntimeDeletesSteadyRuntimeResources(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	deleted := map[string]bool{}
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:      "https://kube.test",
			K8sSATokenPath:  tokenPath,
			RunnerNamespace: "glimmung-runs",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			if req.Method == http.MethodDelete {
				deleted[req.URL.Path] = true
			}
			body := runtimeListResponse(req.URL.Path, deleted)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":          "tank-slot-1",
			"runner_slot_index":         "1",
			"runner_sessions_namespace": "tank-slot-1-sessions",
		},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, Project{Name: "tank"}); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, want := range []string{
		"DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/tank-operator",
		"DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/claude-api-proxy",
		"DELETE /api/v1/namespaces/tank-slot-1/services/tank-operator",
		"DELETE /api/v1/namespaces/tank-slot-1-sessions/pods/session-4",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing runtime delete %s, paths=%#v", want, paths)
		}
	}
	if countPath(paths, "GET /api/v1/namespaces/tank-slot-1-sessions/pods") < 2 {
		t.Fatalf("return should re-check session pods before marking cleanup complete, paths=%#v", paths)
	}
}

func TestReturnTestSlotRuntimeUninstallsHelmRuntimeRelease(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{"items":[]}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-uninstall-hot-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-1",
			"runner_slot_index":         "1",
			"runner_sessions_namespace": "tank-operator-slot-1-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, project); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, want := range []string{
		"POST /apis/batch/v1/namespaces/glimmung-runs/jobs",
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-uninstall-hot-tank-operator-slot-1-2",
		"DELETE /apis/apps/v1/namespaces/tank-operator-slot-1/deployments/slot-playwright",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s in paths=%#v", want, paths)
		}
	}
	for _, path := range paths {
		if strings.Contains(path, "/services/tank-operator") || strings.Contains(path, "/services/claude-api-proxy") || strings.Contains(path, "/services/codex-api-proxy") {
			t.Fatalf("helm cleanup should not hand-delete runtime services, paths=%#v", paths)
		}
	}
}

func TestReturnTestSlotRuntimeRetiresTankSessionScopeBeforeHelmUninstall(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	var tankAuth string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			TankOperatorBaseURL:  "https://tank.internal",
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Host+req.URL.Path)
			if req.URL.Host == "tank.internal" {
				tankAuth = req.Header.Get("Authorization")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"status":"ok","retired_count":2}`)),
				}, nil
			}
			body := `{"items":[]}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-uninstall-hot-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-1",
			"runner_slot_index":         "1",
			"runner_sessions_namespace": "tank-operator-slot-1-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}
	ctx := contextWithTankSessionScopeRetireAuth(context.Background(), "Bearer caller-jwt")

	if err := launcher.ReturnTestSlotRuntime(ctx, lease, project); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	retirePath := "POST tank.internal/api/internal/session-scopes/tank-operator-slot-1/retire"
	uninstallPath := "POST kube.test/apis/batch/v1/namespaces/glimmung-runs/jobs"
	retireIdx := indexPath(paths, retirePath)
	uninstallIdx := indexPath(paths, uninstallPath)
	if retireIdx < 0 {
		t.Fatalf("missing Tank retire call %s in paths=%#v", retirePath, paths)
	}
	if uninstallIdx < 0 {
		t.Fatalf("missing Helm uninstall job call %s in paths=%#v", uninstallPath, paths)
	}
	if retireIdx > uninstallIdx {
		t.Fatalf("Tank retire should happen before Helm uninstall, paths=%#v", paths)
	}
	if tankAuth != "Bearer caller-jwt" {
		t.Fatalf("Tank retire Authorization=%q", tankAuth)
	}
}

func TestReturnTestSlotRuntimeExchangesServiceTokenWhenCallerAuthMissing(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	var exchangeAuth string
	var tankAuth string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:               "https://kube.test",
			K8sSATokenPath:           tokenPath,
			TankOperatorBaseURL:      "https://tank.internal",
			AuthRomaineLifeBaseURL:   "https://auth.internal",
			AuthRomaineLifeTokenPath: tokenPath,
			RunnerNamespace:          "glimmung-runs",
			RunnerServiceAccount:     "glimmung-runner",
			RunnerJobTTLSeconds:      3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Host+req.URL.Path)
			switch req.URL.Host {
			case "auth.internal":
				exchangeAuth = req.Header.Get("Authorization")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"token":"glimmung-service-jwt","expires_at":"2030-01-01T00:00:00Z"}`)),
				}, nil
			case "tank.internal":
				tankAuth = req.Header.Get("Authorization")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"status":"ok","retired_count":2}`)),
				}, nil
			}
			body := `{"items":[]}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-uninstall-hot-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-1",
			"runner_slot_index":         "1",
			"runner_sessions_namespace": "tank-operator-slot-1-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, project); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	exchangePath := "POST auth.internal/api/auth/exchange/k8s"
	retirePath := "POST tank.internal/api/internal/session-scopes/tank-operator-slot-1/retire"
	uninstallPath := "POST kube.test/apis/batch/v1/namespaces/glimmung-runs/jobs"
	exchangeIdx := indexPath(paths, exchangePath)
	retireIdx := indexPath(paths, retirePath)
	uninstallIdx := indexPath(paths, uninstallPath)
	if exchangeIdx < 0 || retireIdx < 0 || uninstallIdx < 0 {
		t.Fatalf("missing expected cleanup calls in paths=%#v", paths)
	}
	if exchangeIdx > retireIdx || retireIdx > uninstallIdx {
		t.Fatalf("exchange, retire, uninstall order mismatch, paths=%#v", paths)
	}
	if exchangeAuth != "Bearer token" {
		t.Fatalf("exchange Authorization=%q", exchangeAuth)
	}
	if tankAuth != "Bearer glimmung-service-jwt" {
		t.Fatalf("Tank retire Authorization=%q", tankAuth)
	}
}

func TestEnsureTestSlotPreliminariesDoesNotCreatePlaywrightRuntime(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:              "https://kube.test",
			K8sSATokenPath:          tokenPath,
			RunnerNamespace:         "glimmung-runs",
			RunnerPlaywrightEnabled: true,
			RunnerPlaywrightImage:   "playwright:latest",
			RunnerPlaywrightPort:    "3000",
			RunnerServiceAccount:    "glimmung-runner",
			RunnerCallbackBaseURL:   "http://glimmung.glimmung.svc.cluster.local",
			RunnerJobTTLSeconds:     3600,
			RunnerNamespaceRole:     "cluster-admin",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"runner_slot_name":  "tank-slot-1",
			"runner_slot_index": "1",
		},
	}

	if err := launcher.EnsureTestSlotPreliminaries(context.Background(), lease, Project{Name: "tank"}, nil); err != nil {
		t.Fatalf("EnsureTestSlotPreliminaries: %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "/deployments") || strings.Contains(path, "/services") {
			t.Fatalf("baseline warm should not create Playwright runtime resources, paths=%#v", paths)
		}
	}
}

func TestEnsureTestSlotInstallerAccessReplacesStaleRoleBinding(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	roleBindingPosts := 0
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerNamespaceRole:  "cluster-admin",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			status := http.StatusOK
			body := `{}`
			if req.Method == http.MethodPost && req.URL.Path == "/apis/rbac.authorization.k8s.io/v1/namespaces/tank-operator-slot-11/rolebindings" {
				roleBindingPosts++
				if roleBindingPosts == 1 {
					status = http.StatusConflict
					body = `{"message":"already exists"}`
				}
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project: "tank-operator",
		Metadata: map[string]any{
			"runner_slot_name":  "tank-operator-slot-11",
			"runner_slot_index": "11",
		},
	}

	if err := launcher.ensureTestSlotInstallerAccess(context.Background(), lease, "tank-operator-slot-11"); err != nil {
		t.Fatalf("ensureTestSlotInstallerAccess: %v", err)
	}
	if roleBindingPosts != 2 {
		t.Fatalf("roleBindingPosts=%d, want 2; paths=%#v", roleBindingPosts, paths)
	}
	if !containsPath(paths, "DELETE /apis/rbac.authorization.k8s.io/v1/namespaces/tank-operator-slot-11/rolebindings/glim-test-slot-installer") {
		t.Fatalf("expected stale RoleBinding delete, paths=%#v", paths)
	}
}

func TestEnsureTestSlotRoleBindingsCreatesNamespacedSessionReadOnly(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	var posted []map[string]any
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:     "https://kube.test",
			K8sSATokenPath: tokenPath,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			if req.Method == http.MethodPost {
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				posted = append(posted, body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	lease := Lease{
		Project: "tank-operator",
		Metadata: map[string]any{
			"runner_slot_name":  "tank-operator-slot-2",
			"runner_slot_index": "2",
		},
	}
	substitutions := testSlotSubstitutions(lease, Project{Name: "tank-operator"}, "tank-operator-slot-2", "tank-operator-slot-2-sessions")

	if err := launcher.ensureTestSlotRoleBindings(context.Background(), lease, defaultTestSlotRoleBindings(Project{Name: "tank-operator"}), substitutions, "tank-operator-slot-2"); err != nil {
		t.Fatalf("ensureTestSlotRoleBindings: %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "/clusterrolebindings") {
			t.Fatalf("session read-only binding must not use ClusterRoleBinding path, paths=%#v", paths)
		}
	}
	if len(posted) != 2 {
		t.Fatalf("posted rolebindings=%d, want 2", len(posted))
	}
	seenNamespaces := map[string]bool{}
	for _, body := range posted {
		if body["kind"] != "RoleBinding" {
			t.Fatalf("kind=%q, want RoleBinding", body["kind"])
		}
		metadata := body["metadata"].(map[string]any)
		seenNamespaces[metadata["namespace"].(string)] = true
		roleRef := body["roleRef"].(map[string]any)
		if roleRef["kind"] != "ClusterRole" || roleRef["name"] != "view" {
			t.Fatalf("roleRef=%#v, want ClusterRole/view", roleRef)
		}
	}
	if !seenNamespaces["tank-operator-slot-2"] || !seenNamespaces["tank-operator-slot-2-sessions"] {
		t.Fatalf("namespaces=%#v, want slot and sessions namespaces", seenNamespaces)
	}
}

func TestEnsureTestSlotPreliminariesRunsWarmHelmOnly(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerNamespaceRole:  "cluster-admin",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-warm-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project: "tank-operator",
		State:   "warming",
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-2",
			"runner_slot_index":         "2",
			"runner_sessions_namespace": "tank-operator-slot-2-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.EnsureTestSlotPreliminaries(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}); err != nil {
		t.Fatalf("EnsureTestSlotPreliminaries: %v", err)
	}
	if !containsPath(paths, "POST /apis/batch/v1/namespaces/glimmung-runs/jobs") {
		t.Fatalf("preliminary ensure should create Helm installer job, paths=%#v", paths)
	}
	if !containsPath(paths, "GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-warm-tank-operator-slot-2-0") {
		t.Fatalf("preliminary ensure should wait for warm Helm job completion, paths=%#v", paths)
	}
	for _, want := range []string{
		"DELETE /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/tank-operator-slot-2-session-cluster-admin",
		"DELETE /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/tank-operator-slot-2-session-readonly",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("preliminary ensure should delete retired cluster-wide session binding %q, paths=%#v", want, paths)
		}
	}
	for _, path := range paths {
		if strings.Contains(path, "glim-slot-apply-hot-") {
			t.Fatalf("preliminary ensure must not run hot Helm pass, paths=%#v", paths)
		}
		if strings.Contains(path, "/deployments/slot-playwright") || strings.Contains(path, "/services/slot-playwright") {
			t.Fatalf("preliminary ensure must not create Playwright runtime, paths=%#v", paths)
		}
	}
}

func TestEnsureTestSlotPreliminariesRequiresMinterForWarmHelm(t *testing.T) {
	tokenPath := tempTokenFile(t)
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerNamespaceRole:  "cluster-admin",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	lease := Lease{
		Project: "tank-operator",
		State:   "warming",
		Metadata: map[string]any{
			"runner_slot_name":  "tank-operator-slot-2",
			"runner_slot_index": "2",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	err := launcher.EnsureTestSlotPreliminaries(context.Background(), lease, project, nil)
	if err == nil || !strings.Contains(err.Error(), "github token minter is required") {
		t.Fatalf("EnsureTestSlotPreliminaries error=%v, want github token minter requirement", err)
	}
}

func TestActivateTestSlotRuntimeRunsHelmInstallerAfterLeaseAssignment(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerNamespaceRole:  "cluster-admin",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	leaseNumber := 12
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		State:       "claimed",
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-2",
			"runner_slot_index":         "2",
			"runner_sessions_namespace": "tank-operator-slot-2-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ActivateTestSlotRuntime(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}); err != nil {
		t.Fatalf("ActivateTestSlotRuntime: %v", err)
	}
	if !containsPath(paths, "POST /apis/batch/v1/namespaces/glimmung-runs/jobs") {
		t.Fatalf("activation should create Helm installer job, paths=%#v", paths)
	}
	for _, want := range []string{
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-warm-tank-operator-slot-2-12",
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-hot-tank-operator-slot-2-12",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("activation should wait for Helm installer job completion %s, paths=%#v", want, paths)
		}
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/tank-operator-slot-2/deployments") {
		t.Fatalf("activation should delegate app runtime creation to installer job, paths=%#v", paths)
	}
}

func TestActivateTestSlotRuntimeCreatesReadyPlaywrightRuntime(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:              "https://kube.test",
			K8sSATokenPath:          tokenPath,
			RunnerNamespace:         "glimmung-runs",
			RunnerServiceAccount:    "glimmung-runner",
			RunnerNamespaceRole:     "cluster-admin",
			RunnerJobTTLSeconds:     3600,
			RunnerPlaywrightEnabled: true,
			RunnerPlaywrightImage:   "playwright:latest",
			RunnerPlaywrightPort:    "3000",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			if req.Method == http.MethodGet && req.URL.Path == "/apis/apps/v1/namespaces/tank-operator-slot-2/deployments/slot-playwright" {
				body = `{"status":{"readyReplicas":1,"availableReplicas":1}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	leaseNumber := 12
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		State:       "claimed",
		Metadata: map[string]any{
			"runner_slot_name":          "tank-operator-slot-2",
			"runner_slot_index":         "2",
			"runner_sessions_namespace": "tank-operator-slot-2-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ActivateTestSlotRuntime(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}); err != nil {
		t.Fatalf("ActivateTestSlotRuntime: %v", err)
	}
	for _, want := range []string{
		"POST /apis/apps/v1/namespaces/tank-operator-slot-2/deployments",
		"POST /api/v1/namespaces/tank-operator-slot-2/services",
		"GET /apis/apps/v1/namespaces/tank-operator-slot-2/deployments/slot-playwright",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing Playwright activation path %s, paths=%#v", want, paths)
		}
	}
}

func TestLaunchPhaseCreatesSlotPlaywrightRuntimeWithoutReadinessWait(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:              "https://kube.test",
			K8sSATokenPath:          tokenPath,
			RunnerNamespace:         "glimmung-runs",
			RunnerServiceAccount:    "glimmung-runner",
			RunnerCallbackBaseURL:   "http://glimmung.glimmung.svc.cluster.local",
			RunnerPlaywrightEnabled: true,
			RunnerPlaywrightImage:   "playwright:latest",
			RunnerPlaywrightPort:    "3000",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	runNumber := 7
	callback := "callback-token"
	leaseNumber := 3
	req := RunLaunchRequest{
		Lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: &leaseNumber,
			State:       "claimed",
			Metadata: map[string]any{
				"runner_slot_name":  "tank-operator-slot-1",
				"runner_slot_index": "1",
			},
		},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "verify", Jobs: []RunnerJobSpec{{ID: "test", Image: "runner:latest"}}},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "tank-operator",
			IssueNumber:   42,
			RunNumber:     &runNumber,
			CallbackToken: &callback,
		},
	}

	if _, err := launcher.LaunchPhase(context.Background(), req); err != nil {
		t.Fatalf("LaunchPhase: %v", err)
	}
	if !containsPath(paths, "POST /apis/apps/v1/namespaces/tank-operator-slot-1/deployments") {
		t.Fatalf("launch should create slot Playwright deployment, paths=%#v", paths)
	}
	if !containsPath(paths, "POST /api/v1/namespaces/tank-operator-slot-1/services") {
		t.Fatalf("launch should create slot Playwright service, paths=%#v", paths)
	}
	if containsPath(paths, "GET /apis/apps/v1/namespaces/tank-operator-slot-1/deployments/slot-playwright") {
		t.Fatalf("launch should not wait for slot Playwright readiness, paths=%#v", paths)
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/glimmung-runs/deployments") {
		t.Fatalf("launch should not create Playwright in glimmung-runs, paths=%#v", paths)
	}
}

func TestDeprovisionTestSlotDeletesInstallerAndNamespaces(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	namespaces := map[string]bool{
		"tank-operator-slot-11":          true,
		"tank-operator-slot-11-sessions": true,
	}
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			status := http.StatusOK
			body := `{}`
			if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/namespaces/") {
				namespace := strings.TrimPrefix(req.URL.Path, "/api/v1/namespaces/")
				if !namespaces[namespace] {
					status = http.StatusNotFound
				}
			}
			if req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/api/v1/namespaces/") {
				namespace := strings.TrimPrefix(req.URL.Path, "/api/v1/namespaces/")
				namespaces[namespace] = false
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := testEnvironmentWarmupLease(Project{Name: "tank-operator"}, 11, "tank-operator-slot-11")

	if err := launcher.DeprovisionTestSlot(context.Background(), lease, Project{Name: "tank-operator"}); err != nil {
		t.Fatalf("DeprovisionTestSlot: %v", err)
	}
	for _, want := range []string{
		"DELETE /api/v1/namespaces/glimmung-runs/secrets/glim-helm-clone-tank-operator-slot-11-0",
		"DELETE /api/v1/namespaces/tank-operator-slot-11-sessions",
		"GET /api/v1/namespaces/tank-operator-slot-11-sessions",
		"DELETE /api/v1/namespaces/tank-operator-slot-11",
		"GET /api/v1/namespaces/tank-operator-slot-11",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s in paths=%#v", want, paths)
		}
	}
}

func TestTestSlotHelmConfigDefaultsTankChart(t *testing.T) {
	config, ok := testSlotHelmConfig(Project{
		ID:         "tank-operator",
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata: map[string]any{
			"test_slot_helm": map[string]any{"enabled": true},
		},
	})
	if !ok {
		t.Fatal("expected helm config")
	}
	if config.ChartPath != "k8s" {
		t.Fatalf("chart path=%q", config.ChartPath)
	}
	if config.InstallerImage != "alpine/k8s:1.30.0" {
		t.Fatalf("installer image=%q", config.InstallerImage)
	}
	if _, ok := config.Values["testEnv.enabled"]; ok {
		t.Fatalf("testEnv.enabled should not be injected into helm values")
	}
	if len(config.ClusterRoleBindings) != 1 {
		t.Fatalf("cluster role binding templates=%d", len(config.ClusterRoleBindings))
	}
	if len(config.RoleBindings) != 2 {
		t.Fatalf("role binding templates=%d", len(config.RoleBindings))
	}
}

func TestTestSlotInstallJobManifestRendersHelmApplyJob(t *testing.T) {
	leaseNumber := 12
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		Metadata: map[string]any{
			"runner_slot_name":  "tank-operator-slot-2",
			"runner_slot_index": "2",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{
				"record_base": "tank.dev.romaine.life",
			},
			"test_slot_helm": map[string]any{"enabled": true},
		},
	}
	config, ok := testSlotHelmConfig(project)
	if !ok {
		t.Fatal("expected helm config")
	}
	manifest := testSlotInstallJobManifest(
		Settings{RunnerNamespace: "glimmung-runs", RunnerServiceAccount: "glimmung-runner", RunnerJobTTLSeconds: 3600},
		config,
		lease,
		project,
		testSlotSubstitutions(lease, project, "tank-operator-slot-2", "tank-operator-slot-2-sessions"),
		testSlotRenderModeHot,
	)
	if manifest["metadata"].(map[string]any)["name"] != "glim-slot-apply-hot-tank-operator-slot-2-12" {
		t.Fatalf("job name=%q", manifest["metadata"].(map[string]any)["name"])
	}
	spec := manifest["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	initScript := spec["initContainers"].([]any)[0].(map[string]any)["command"].([]string)[2]
	if strings.Contains(initScript, "ghs_") {
		t.Fatalf("clone script should not contain token: %s", initScript)
	}
	if !strings.Contains(initScript, "romaine-life/tank-operator") {
		t.Fatalf("clone script missing repo: %s", initScript)
	}
	installScript := spec["containers"].([]any)[0].(map[string]any)["command"].([]string)[2]
	for _, want := range []string{
		"helm template 'tank-operator-slot-2-hot' 'k8s'",
		"helm upgrade --install 'tank-operator-slot-2-hot' 'k8s'",
		"--set 'testEnv.slotName=tank-operator-slot-2'",
		"--set 'renderMode=hot'",
		"kubectl delete --ignore-not-found=true -f -",
		"--wait --wait-for-jobs --timeout 180s",
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("install script missing %q: %s", want, installScript)
		}
	}
}

// TestTestSlotInstallJobManifestClonesShaByFetch guards the deploy-to-image
// regression caught by the Stage-6 live smoke: the reconcile clone must fetch
// the exact ref (commit sha OR branch), because deploy-to-image pins
// config.GitRef to a verified commit sha so the chart and the CI image are the
// same commit — and `git clone --branch <sha>` is invalid ("Remote branch <sha>
// not found in upstream origin"). `git fetch` accepts a reachable sha (GitHub
// allowReachableSHA1InWant) or a ref name, so the one clone serves both callers.
func TestTestSlotInstallJobManifestClonesShaByFetch(t *testing.T) {
	leaseNumber := 7
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		Metadata: map[string]any{
			"runner_slot_name":  "tank-operator-slot-1",
			"runner_slot_index": "1",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{"record_base": "tank.dev.romaine.life"},
			"test_slot_helm":     map[string]any{"enabled": true},
		},
	}
	config, ok := testSlotHelmConfig(project)
	if !ok {
		t.Fatal("expected helm config")
	}
	config.GitRef = "e8c3f183abbf75ccc7066d981bb422cb1d1ae2ff" // a commit sha, not a branch
	manifest := testSlotInstallJobManifest(
		Settings{RunnerNamespace: "glimmung-runs", RunnerServiceAccount: "glimmung-runner", RunnerJobTTLSeconds: 3600},
		config, lease, project,
		testSlotSubstitutions(lease, project, "tank-operator-slot-1", "tank-operator-slot-1-sessions"),
		testSlotRenderModeHot,
	)
	spec := manifest["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	initScript := spec["initContainers"].([]any)[0].(map[string]any)["command"].([]string)[2]
	for _, want := range []string{"git fetch --depth=1 origin", "git checkout -q FETCH_HEAD"} {
		if !strings.Contains(initScript, want) {
			t.Fatalf("clone script missing %q (sha clone must fetch, not --branch): %s", want, initScript)
		}
	}
	if strings.Contains(initScript, "clone --branch") {
		t.Fatalf("clone script must not use `git clone --branch` (breaks on a sha): %s", initScript)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeRunnerGitHubTokenMinter struct {
	token string
	err   error
}

func (m fakeRunnerGitHubTokenMinter) InstallationToken(context.Context) (string, error) {
	return m.token, m.err
}

func (m fakeRunnerGitHubTokenMinter) RepositoryInstallationToken(context.Context, string, map[string]string) (string, error) {
	return m.token, m.err
}

func tempTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func indexPath(paths []string, want string) int {
	for i, path := range paths {
		if path == want {
			return i
		}
	}
	return -1
}

func countPath(paths []string, want string) int {
	count := 0
	for _, path := range paths {
		if path == want {
			count++
		}
	}
	return count
}

func runtimeListResponse(path string, deleted map[string]bool) string {
	item := func(deletePath, name string) string {
		if deleted[deletePath] {
			return ""
		}
		return `{"metadata":{"name":"` + name + `"}}`
	}
	items := func(values ...string) string {
		filtered := make([]string, 0, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				filtered = append(filtered, value)
			}
		}
		return `{"items":[` + strings.Join(filtered, ",") + `]}`
	}
	switch path {
	case "/apis/apps/v1/namespaces/tank-slot-1/deployments":
		return items(
			item("/apis/apps/v1/namespaces/tank-slot-1/deployments/tank-operator", "tank-operator"),
			item("/apis/apps/v1/namespaces/tank-slot-1/deployments/claude-api-proxy", "claude-api-proxy"),
		)
	case "/api/v1/namespaces/tank-slot-1/services":
		return items(item("/api/v1/namespaces/tank-slot-1/services/tank-operator", "tank-operator"))
	case "/api/v1/namespaces/tank-slot-1-sessions/pods":
		return items(item("/api/v1/namespaces/tank-slot-1-sessions/pods/session-4", "session-4"))
	default:
		if strings.Contains(path, "/jobs/glim-slot-apply-") {
			return `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
		}
		return `{"items":[]}`
	}
}

func runnerManifestEnv(manifest map[string]any) map[string]string {
	container := runnerManifestContainer(manifest)
	envRows := container["env"].([]map[string]any)
	env := map[string]string{}
	for _, row := range envRows {
		if value, ok := row["value"].(string); ok {
			env[row["name"].(string)] = value
		}
	}
	return env
}

func runnerManifestContainer(manifest map[string]any) map[string]any {
	podSpec := runnerManifestPodSpec(manifest)
	containers := podSpec["containers"].([]any)
	return containers[0].(map[string]any)
}

func runnerManifestPodSpec(manifest map[string]any) map[string]any {
	spec := manifest["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	return template["spec"].(map[string]any)
}

func TestRunnerJobEnvCarriesPriorVerification(t *testing.T) {
	req := RunLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "default"},
		Phase:    PhaseSpec{Name: "prepare"},
		Run: RunReplayData{
			ID:          "run-1",
			IssueNumber: 1,
			PriorVerification: &PriorVerificationData{
				Phase: "llm-verify",
				Verification: RunVerificationData{
					Status:  "fail",
					Reasons: []string{"verifier reported status=abort reason=claimed_result_not_observed"},
					Failure: &VerificationFailure{
						Expected:       "5-10 lantern cluster",
						Observed:       "counts of 13 and 12",
						SuspectedCause: "test_expectation_mismatch",
					},
				},
			},
		},
	}
	env := runnerJobEnv(Settings{}, req, RunnerJobSpec{}, "secret")
	var payload string
	for _, entry := range env {
		if entry["name"] == "GLIMMUNG_PRIOR_VERIFICATION_JSON" {
			payload, _ = entry["value"].(string)
		}
	}
	if payload == "" {
		t.Fatalf("expected GLIMMUNG_PRIOR_VERIFICATION_JSON in env: %v", env)
	}
	var decoded PriorVerificationData
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode prior verification env: %v", err)
	}
	if decoded.Phase != "llm-verify" || decoded.Verification.Status != "fail" {
		t.Fatalf("decoded=%#v", decoded)
	}
	if decoded.Verification.Failure == nil || decoded.Verification.Failure.SuspectedCause != "test_expectation_mismatch" {
		t.Fatalf("failure=%#v", decoded.Verification.Failure)
	}

	// No prior verification -> no env entry.
	req.Run.PriorVerification = nil
	for _, entry := range runnerJobEnv(Settings{}, req, RunnerJobSpec{}, "secret") {
		if entry["name"] == "GLIMMUNG_PRIOR_VERIFICATION_JSON" {
			t.Fatal("env should not carry prior verification when absent")
		}
	}
}

// TestDefaultTestSlotRBACSessionIsNamespacedReadOnly is the enforcement guard:
// the slot session SA must never get a cluster-wide binding. It gets read-only
// `view` through RoleBindings scoped to the app and sessions namespaces.
func TestDefaultTestSlotRBACSessionIsNamespacedReadOnly(t *testing.T) {
	for _, b := range defaultTestSlotClusterRoleBindings(Project{Name: "tank-operator"}) {
		subs, _ := b["subjects"].([]any)
		for _, s := range subs {
			sm, _ := s.(map[string]any)
			if sm["name"] == "{slot_name}-session" {
				t.Fatalf("slot session SA must not have a ClusterRoleBinding: %#v", b)
			}
		}
	}

	bindings := defaultTestSlotRoleBindings(Project{Name: "tank-operator"})
	if len(bindings) != 2 {
		t.Fatalf("role bindings=%d, want 2", len(bindings))
	}
	namespaces := map[string]bool{}
	for _, b := range bindings {
		metadata := b["metadata"].(map[string]any)
		namespaces[metadata["namespace"].(string)] = true
		subs, _ := b["subjects"].([]any)
		foundSessionSA := false
		for _, s := range subs {
			sm, _ := s.(map[string]any)
			if sm["name"] == "{slot_name}-session" && sm["namespace"] == "{sessions_namespace}" {
				foundSessionSA = true
			}
		}
		if !foundSessionSA {
			t.Fatalf("RoleBinding missing slot session SA subject: %#v", b)
		}
		roleRef := b["roleRef"].(map[string]any)
		if roleRef["kind"] != "ClusterRole" || roleRef["name"] != "view" {
			t.Fatalf("RoleBinding roleRef=%#v, want ClusterRole/view", roleRef)
		}
	}
	if !namespaces["{slot_name}"] || !namespaces["{sessions_namespace}"] {
		t.Fatalf("RoleBindings namespaces=%#v, want slot and sessions namespaces", namespaces)
	}
}

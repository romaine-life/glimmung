package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	testSlotInstallerJobPrefix    = "glim-slot-apply"
	testSlotInstallerSecretPrefix = "glim-slot-clone"
	testSlotUninstallJobPrefix    = "glim-slot-uninstall"
	testSlotRenderModeWarm        = "warm"
	testSlotRenderModeHot         = "hot"
)

// RunLauncher creates Kubernetes resources for runner k8s_job phases.
type RunLauncher interface {
	LaunchPhase(ctx context.Context, req RunLaunchRequest) ([]RunLaunchedJob, error)
}

// RunnerJobStatusGetter exposes the terminal status of a previously launched
// runner phase Job so the run-execution reconciler can detect Jobs that died
// without the runner ever delivering its completion callback (DeadlineExceeded
// killing the pod mid-step, OOM, eviction, etc.) and synthesize a failed
// completion. KubernetesRunLauncher implements this; the dispatch fakes
// do not have to.
type RunnerJobStatusGetter interface {
	GetRunnerJobStatus(ctx context.Context, namespace, name string) (RunnerJobStatus, error)
}

// RunnerJobLogsFetcher exposes a bounded read of a phase Job's pod
// stdout so the run-execution reconciler can capture logs into the
// artifact store on terminal transitions. Stage 3 of the inner-Job
// observation contract — the durable counterpart to the Grafana
// Explore deep-link, retained past Loki's window.
type RunnerJobLogsFetcher interface {
	GetRunnerJobLogs(ctx context.Context, namespace, jobName string, maxBytes int64) ([]byte, error)
}

// RunnerJobStatus is the small subset of batch/v1 Job status the reconciler
// needs to decide whether to synthesize a completion. Zero value (Found=false)
// means the Job has been garbage-collected from k8s; callers treat that as
// "the run lost its execution surface" and should fail the phase.
type RunnerJobStatus struct {
	Found              bool
	Active             int
	Succeeded          int
	Failed             int
	Conditions         []RunnerJobCondition
	CompletionTime     time.Time
	LastTransitionTime time.Time
	// PodTerminationReason is the pod-level termination reason read from
	// the Job's pod when the Job condition only says
	// BackoffLimitExceeded — the container's terminated.reason
	// ("OOMKilled", "Error") or the pod's status.reason ("Evicted").
	// Empty when the pod is gone or recorded no terminal reason.
	// Populated by GetRunnerJobStatus only on terminal failure.
	PodTerminationReason string
}

// RunnerJobCondition mirrors the fields of batch/v1 JobCondition the
// reconciler inspects.
type RunnerJobCondition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

// IsTerminallyFailed reports whether the Job has a Failed=True condition.
func (s RunnerJobStatus) IsTerminallyFailed() bool {
	for _, c := range s.Conditions {
		if strings.EqualFold(c.Type, "Failed") && strings.EqualFold(c.Status, "True") {
			return true
		}
	}
	return false
}

// IsTerminallySucceeded reports whether the Job has a Complete=True (or
// SuccessCriteriaMet=True) condition. Used so the reconciler can tell a
// "successful pod that simply lost its callback" from a real failure.
func (s RunnerJobStatus) IsTerminallySucceeded() bool {
	for _, c := range s.Conditions {
		if !strings.EqualFold(c.Status, "True") {
			continue
		}
		if strings.EqualFold(c.Type, "Complete") || strings.EqualFold(c.Type, "SuccessCriteriaMet") {
			return true
		}
	}
	return false
}

// FailureReason returns the reason field of the first Failed=True condition,
// or empty if there is none.
func (s RunnerJobStatus) FailureReason() string {
	for _, c := range s.Conditions {
		if strings.EqualFold(c.Type, "Failed") && strings.EqualFold(c.Status, "True") {
			return c.Reason
		}
	}
	return ""
}

// FailureMessage returns the message field of the first Failed=True condition.
func (s RunnerJobStatus) FailureMessage() string {
	for _, c := range s.Conditions {
		if strings.EqualFold(c.Type, "Failed") && strings.EqualFold(c.Status, "True") {
			return c.Message
		}
	}
	return ""
}

// TerminalTime returns the best timestamp for when the Job entered its
// terminal state: completionTime when present, otherwise the most recent
// condition transition time.
func (s RunnerJobStatus) TerminalTime() time.Time {
	if !s.CompletionTime.IsZero() {
		return s.CompletionTime
	}
	return s.LastTransitionTime
}

type TestSlotPreparer interface {
	EnsureTestSlotPreliminaries(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter) error
	ActivateTestSlotRuntime(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter) error
	ReturnTestSlotRuntime(ctx context.Context, lease Lease, project Project) error
	DeprovisionTestSlot(ctx context.Context, lease Lease, project Project) error
}

type TestSlotInstallerCleaner interface {
	CleanupTestSlotInstaller(ctx context.Context, lease Lease, project Project) error
}

type RunLaunchRequest struct {
	Lease    Lease
	Workflow Workflow
	Phase    PhaseSpec
	Run      RunReplayData
	// SkipJobIDs names the phase jobs whose `when` condition evaluated
	// false at dispatch (job ID -> resolution trace). The launcher creates
	// no Kubernetes Job for them — the dispatch layer has already written
	// their synthesized skipped records. Keyed by ID (not position) because
	// the launcher re-canonicalizes the phase from the workflow schema.
	SkipJobIDs map[string]string
}

// RunLaunchedJob pairs a launched job's schema ID with the concrete
// Kubernetes Job name. Typed (not positional) so conditionally skipped jobs
// cannot shift the ID<->name mapping.
type RunLaunchedJob struct {
	JobID      string
	K8sJobName string
}

type KubernetesRunLauncher struct {
	Settings   Settings
	HTTPClient *http.Client
}

type providerAPIProxyRuntime struct {
	ClaudeClusterIP string
	CodexClusterIP  string
	GitHubClusterIP string
	CASecretName    string
	CABundlePath    string
}

func (r providerAPIProxyRuntime) enabled() bool {
	return strings.TrimSpace(r.ClaudeClusterIP) != "" &&
		strings.TrimSpace(r.CodexClusterIP) != "" &&
		strings.TrimSpace(r.CASecretName) != "" &&
		strings.TrimSpace(r.CABundlePath) != ""
}

func NewKubernetesRunLauncher(settings Settings) *KubernetesRunLauncher {
	return &KubernetesRunLauncher{Settings: settings}
}

func (l *KubernetesRunLauncher) LaunchPhase(ctx context.Context, req RunLaunchRequest) ([]RunLaunchedJob, error) {
	req.Workflow = CanonicalWorkflow(req.Workflow)
	if phase := phaseSpecByName(req.Workflow.Phases, req.Phase.Name); phase != nil {
		req.Phase = *phase
	} else {
		req.Phase = CanonicalRunnerPhase(req.Phase)
	}
	derivedPhase, err := derivePrimaryCheckoutRepo(req.Phase, req.Run.IssueRepo)
	if err != nil {
		return nil, err
	}
	derivedPhase, err = resolveRunnerCheckoutRunInputs(derivedPhase, launchRunInputs(req))
	if err != nil {
		return nil, err
	}
	req.Phase = derivedPhase
	if len(req.Phase.Jobs) == 0 {
		return nil, fmt.Errorf("runner phase %q has no jobs", req.Phase.Name)
	}
	if err := l.ensurePlaywrightForNativePhase(ctx, req); err != nil {
		return nil, err
	}
	var proxyRuntime providerAPIProxyRuntime
	if runnerPhaseRequiresProviderAPIProxy(req.Phase) {
		runtime, err := l.resolveProviderAPIProxyRuntime(ctx)
		if err != nil {
			return nil, err
		}
		proxyRuntime = runtime
	}
	attemptIndex := runnerAttemptIndex(req)
	attemptBase := compactResourceName("glim", runRefFromData(req.Run), attemptIndex)
	launched := make([]RunLaunchedJob, 0, len(req.Phase.Jobs))
	for _, job := range req.Phase.Jobs {
		if strings.TrimSpace(job.ID) == "" {
			return nil, fmt.Errorf("runner phase %q has job with empty id", req.Phase.Name)
		}
		if _, skip := req.SkipJobIDs[job.ID]; skip {
			// The job's `when` evaluated false at dispatch. Zero compute:
			// no secret, no manifest, no Kubernetes Job. The dispatch layer
			// owns the synthesized skipped records.
			continue
		}
		jobName := runnerJobName(attemptBase, job.ID)
		secretName := jobName + "-token"
		if _, err := l.ensureAttemptSecret(ctx, secretName, attemptBase, job.ID); err != nil {
			return nil, err
		}
		jobProxyRuntime := providerAPIProxyRuntime{}
		if runnerJobRequiresProviderAPIProxyForPhase(req.Phase, job) {
			jobProxyRuntime = proxyRuntime
		}
		if err := l.createJob(ctx, runnerJobManifest(l.Settings, req, job, jobName, secretName, attemptBase, jobProxyRuntime)); err != nil {
			return nil, err
		}
		launched = append(launched, RunLaunchedJob{JobID: job.ID, K8sJobName: jobName})
	}
	if len(launched) == 0 {
		return nil, fmt.Errorf("runner phase %q has no launchable jobs (every job skipped by when conditions); the dispatch layer must synthesize a skipped phase attempt instead of launching", req.Phase.Name)
	}
	return launched, nil
}

// derivePrimaryCheckoutRepo sets every code job's primary checkout repo to the
// project-bound issue repo. The primary checkout repo is not author-controlled:
// it is derived from the project's github_repo (carried on the run as
// IssueRepo) so an org/repo migration updates one project record instead of
// every baked workflow literal. ExtraCheckouts remain author-controlled for the
// cross-repo case; jobs without a checkout (managed primitives) are untouched.
func derivePrimaryCheckoutRepo(phase PhaseSpec, issueRepo string) (PhaseSpec, error) {
	issueRepo = strings.TrimSpace(issueRepo)
	if len(phase.Jobs) == 0 {
		return phase, nil
	}
	jobs := make([]RunnerJobSpec, len(phase.Jobs))
	copy(jobs, phase.Jobs)
	for i := range jobs {
		if jobs[i].Checkout == nil {
			continue
		}
		if issueRepo == "" {
			return PhaseSpec{}, fmt.Errorf("runner phase %q job %q requires a checkout but the run has no issue repo", phase.Name, jobs[i].ID)
		}
		checkout := *jobs[i].Checkout
		checkout.Repo = issueRepo
		jobs[i].Checkout = &checkout
	}
	phase.Jobs = jobs
	return phase, nil
}

func resolveRunnerCheckoutRunInputs(phase PhaseSpec, inputs map[string]string) (PhaseSpec, error) {
	if len(phase.Jobs) == 0 {
		return phase, nil
	}
	jobs := make([]RunnerJobSpec, len(phase.Jobs))
	copy(jobs, phase.Jobs)
	for i := range jobs {
		if jobs[i].Checkout != nil {
			checkout := *jobs[i].Checkout
			ref, err := resolveRunInputTemplate(checkout.Ref, inputs)
			if err != nil {
				return PhaseSpec{}, fmt.Errorf("runner phase %q job %q checkout.ref: %w", phase.Name, jobs[i].ID, err)
			}
			checkout.Ref = ref
			jobs[i].Checkout = &checkout
		}
		if len(jobs[i].ExtraCheckouts) > 0 {
			extra := make([]RunnerCheckoutSpec, len(jobs[i].ExtraCheckouts))
			copy(extra, jobs[i].ExtraCheckouts)
			for j := range extra {
				ref, err := resolveRunInputTemplate(extra[j].Ref, inputs)
				if err != nil {
					return PhaseSpec{}, fmt.Errorf("runner phase %q job %q extra_checkouts[%d].ref: %w", phase.Name, jobs[i].ID, j, err)
				}
				extra[j].Ref = ref
			}
			jobs[i].ExtraCheckouts = extra
		}
	}
	phase.Jobs = jobs
	return phase, nil
}

func launchRunInputs(req RunLaunchRequest) map[string]string {
	out := runInputsForMetadata(req.Run.RunInputs)
	for key, raw := range anyMap(req.Lease.Metadata["run_inputs"]) {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if _, exists := out[key]; exists {
			continue
		}
		out[key] = strings.TrimSpace(fmt.Sprint(raw))
	}
	return out
}

func runnerPhaseRequiresProviderAPIProxy(phase PhaseSpec) bool {
	if phase.Verify {
		return true
	}
	for _, job := range phase.Jobs {
		if runnerJobRequiresProviderAPIProxy(job) {
			return true
		}
	}
	return false
}

func runnerJobRequiresProviderAPIProxyForPhase(phase PhaseSpec, job RunnerJobSpec) bool {
	if phase.Verify && job.Managed {
		return true
	}
	return runnerJobRequiresProviderAPIProxy(job)
}

func runnerJobRequiresProviderAPIProxy(job RunnerJobSpec) bool {
	if !job.Managed {
		return false
	}
	for _, step := range job.Steps {
		if step.Agent != nil {
			return true
		}
	}
	return false
}

func (l *KubernetesRunLauncher) resolveProviderAPIProxyRuntime(ctx context.Context) (providerAPIProxyRuntime, error) {
	namespace := firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyNamespace), strings.TrimSpace(l.Settings.RunnerNamespace))
	claudeService := firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyClaudeService), "claude-api-proxy")
	codexService := firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyCodexService), "codex-api-proxy")
	githubService := firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyGitHubService), "github-git-policy-proxy")
	claudeIP, err := l.serviceClusterIP(ctx, namespace, claudeService)
	if err != nil {
		return providerAPIProxyRuntime{}, fmt.Errorf("resolve claude provider api proxy service: %w", err)
	}
	codexIP, err := l.serviceClusterIP(ctx, namespace, codexService)
	if err != nil {
		return providerAPIProxyRuntime{}, fmt.Errorf("resolve codex provider api proxy service: %w", err)
	}
	githubIP, err := l.serviceClusterIP(ctx, namespace, githubService)
	if err != nil {
		return providerAPIProxyRuntime{}, fmt.Errorf("resolve github git policy proxy service: %w", err)
	}
	return providerAPIProxyRuntime{
		ClaudeClusterIP: claudeIP,
		CodexClusterIP:  codexIP,
		GitHubClusterIP: githubIP,
		CASecretName:    firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyCASecret), "glimmung-provider-api-proxy-ca"),
		CABundlePath:    firstNonEmpty(strings.TrimSpace(l.Settings.ProviderAPIProxyCABundlePath), "/etc/glimmung-provider-api-proxy-bundle/ca-certificates.crt"),
	}, nil
}

func (l *KubernetesRunLauncher) serviceClusterIP(ctx context.Context, namespace, service string) (string, error) {
	status, body, err := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/services/"+service, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET service %s/%s returned %d", namespace, service, status)
	}
	spec, _ := body["spec"].(map[string]any)
	ip, _ := spec["clusterIP"].(string)
	ip = strings.TrimSpace(ip)
	if ip == "" || strings.EqualFold(ip, "none") {
		return "", fmt.Errorf("service %s/%s has no ClusterIP", namespace, service)
	}
	return ip, nil
}

func (l *KubernetesRunLauncher) EnsureTestSlotPreliminaries(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if err := l.ensureTestSlotPreliminaryAccess(ctx, lease, project, slotName); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		if strings.TrimSpace(project.GitHubRepo) == "" {
			return fmt.Errorf("github_repo is required for test slot preliminary warm install")
		}
		if minter == nil {
			return fmt.Errorf("github token minter is required for test slot preliminary warm install")
		}
		if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeWarm); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) ensureTestSlotPreliminaryAccess(ctx context.Context, lease Lease, project Project, slotName string) error {
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if err := l.ensureNamespace(ctx, slotName, testSlotLabels(lease, slotName)); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
		if err := l.ensureNamespace(ctx, sessionsNamespace, testSlotLabels(lease, slotName)); err != nil {
			return err
		}
		if err := l.ensureTestSlotInstallerAccess(ctx, lease, slotName); err != nil {
			return err
		}
		if err := l.ensureTestSlotInstallerAccess(ctx, lease, sessionsNamespace); err != nil {
			return err
		}
		substitutions := testSlotSubstitutions(lease, project, slotName, sessionsNamespace)
		if err := l.ensureTestSlotClusterRoleBindings(ctx, lease, config.ClusterRoleBindings, substitutions, slotName); err != nil {
			return err
		}
		if isTankOperatorProject(project) {
			if err := l.deleteRetiredTestSlotSessionClusterRoleBindings(ctx, slotName); err != nil {
				return err
			}
		}
		if err := l.ensureTestSlotRoleBindings(ctx, lease, config.RoleBindings, substitutions, slotName); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) ActivateTestSlotRuntime(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if err := l.ensureTestSlotPreliminaryAccess(ctx, lease, project, slotName); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		if strings.TrimSpace(project.GitHubRepo) == "" {
			return fmt.Errorf("github_repo is required for test slot runtime activation")
		}
		if minter == nil {
			return fmt.Errorf("github token minter is required for test slot runtime activation")
		}
		if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeWarm); err != nil {
			return err
		}
		if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeHot); err != nil {
			return err
		}
	}
	return l.ensurePlaywrightForSlot(ctx, lease, slotName, true)
}

func (l *KubernetesRunLauncher) ReturnTestSlotRuntime(ctx context.Context, lease Lease, project Project) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	if strings.TrimSpace(slotName) == "" {
		return nil
	}
	// 1. Delete the installer Job + clone Secret first. The Job's pods run
	//    `helm install`, which creates the slot-scoped Deployments / Pods /
	//    Services we're about to delete. If we deleted those outputs first
	//    while the Job was still running, helm would recreate them faster
	//    than we could delete them and waitForNoPodsInNamespaces would spin
	//    until its 5-minute timeout fired — the failure mode that previously
	//    stranded the slot in `error`. See docs/test-slot-lifecycle.md.
	if err := l.deleteTestSlotInstaller(ctx, lease); err != nil {
		return err
	}
	// 2. Wait for the installer Job's pods to terminate. The helm-install
	//    process lives in the pod; once the pod is gone, helm cannot create
	//    further K8s objects. Background propagation on the Job DELETE marks
	//    its pods for GC; we poll until the GC catches up so the next steps
	//    don't race the helm tail.
	if err := l.waitForInstallerPodsBySlotTerminated(ctx, l.Settings.RunnerNamespace, slotName, 90*time.Second); err != nil {
		return err
	}
	if err := l.retireTankSessionScope(ctx, lease, project, slotName); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		if err := l.runTestSlotHelmUninstall(ctx, lease, project, config, testSlotRenderModeHot); err != nil {
			return err
		}
		if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
			return err
		}
		if err := l.deleteTestSlotRuntimeWorkloads(ctx, lease, project, slotName); err != nil {
			return err
		}
		return nil
	}
	// 3. Delete the lease-scoped Playwright Deployment and Service. Cleanup
	//    creates these via ensurePlaywrightForSlot during activation; the
	//    in-process activation goroutine that creates them has already been
	//    cancelled and awaited by beginTestSlotCleanup before reaching this
	//    function — so this delete sees a static target.
	if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
		return err
	}
	// 4. Delete the remaining slot-namespace workloads (project app
	//    Deployments, Services, leftover Jobs/Pods) and wait for every pod
	//    in the slot namespace(s) to terminate. With the installer Job dead
	//    and the activation goroutine unwound, no producer is left racing
	//    this final sweep.
	if err := l.deleteTestSlotRuntimeResources(ctx, lease, project, slotName); err != nil {
		return err
	}
	return nil
}

type tankScopeRetireExchangeResponse struct {
	Token string `json:"token"`
}

func (l *KubernetesRunLauncher) tankSessionScopeRetireAuthorization(ctx context.Context) (string, error) {
	if authorization := tankSessionScopeRetireAuth(ctx); authorization != "" {
		return authorization, nil
	}
	token, err := l.exchangeAuthRomaineServiceToken(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

func (l *KubernetesRunLauncher) exchangeAuthRomaineServiceToken(ctx context.Context) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(l.Settings.AuthRomaineLifeBaseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("auth.romaine.life base URL not configured for Tank session-scope retire")
	}
	tokenPath := strings.TrimSpace(l.Settings.AuthRomaineLifeTokenPath)
	if tokenPath == "" {
		return "", fmt.Errorf("auth.romaine.life token path not configured for Tank session-scope retire")
	}
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("read auth.romaine.life service account token: %w", err)
	}
	saToken := strings.TrimSpace(string(data))
	if saToken == "" {
		return "", fmt.Errorf("auth.romaine.life service account token is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/auth/exchange/k8s", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+saToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := l.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth.romaine.life service exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("auth.romaine.life service exchange returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var decoded tankScopeRetireExchangeResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", fmt.Errorf("decode auth.romaine.life service exchange response: %w", err)
	}
	token := strings.TrimSpace(decoded.Token)
	if token == "" {
		return "", fmt.Errorf("auth.romaine.life service exchange response missing token")
	}
	return token, nil
}

func (l *KubernetesRunLauncher) retireTankSessionScope(ctx context.Context, lease Lease, project Project, slotName string) error {
	if !isTankOperatorProject(project) {
		return nil
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(l.Settings.TankOperatorBaseURL), "/")
	if baseURL == "" {
		return nil
	}
	authorization, err := l.tankSessionScopeRetireAuthorization(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"source":    "glimmung.test_slot_return",
		"project":   project.Name,
		"slot_name": slotName,
		"lease_ref": LeasePublicRefFromLease(lease),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/internal/session-scopes/"+url.PathEscape(slotName)+"/retire", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	client := l.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("retire tank session scope %s: %w", slotName, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("retire tank session scope %s returned %d: %s", slotName, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func isTankOperatorProject(project Project) bool {
	return strings.EqualFold(strings.TrimSpace(project.Name), "tank-operator") ||
		strings.EqualFold(strings.TrimSpace(project.ID), "tank-operator") ||
		strings.EqualFold(strings.TrimSpace(project.GitHubRepo), "romaine-life/tank-operator")
}

// waitForInstallerPodsTerminated polls for the helm-install pod(s) spawned
// by the installer Job to be gone. The Job's DELETE call (with Background
// propagation) marks the pods for GC; this poll waits for the GC to catch
// up so the cleanup path that follows doesn't race the helm tail still
// running inside an undead pod. Selector is the standard K8s job-name
// label that controller-runtime attaches to Job pods.
func (l *KubernetesRunLauncher) waitForInstallerPodsTerminated(ctx context.Context, namespace, jobName string, timeout time.Duration) error {
	namespace = strings.TrimSpace(namespace)
	jobName = strings.TrimSpace(jobName)
	if namespace == "" || jobName == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	selector := "job-name=" + labelValue(jobName)
	path := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + url.QueryEscape(selector)
	for {
		status, list, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				return nil
			}
			return err
		}
		if len(anySlice(list["items"])) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("installer job %s/%s pods did not terminate within %s", namespace, jobName, timeout)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) waitForInstallerPodsBySlotTerminated(ctx context.Context, namespace, slotName string, timeout time.Duration) error {
	namespace = strings.TrimSpace(namespace)
	slotName = strings.TrimSpace(slotName)
	if namespace == "" || slotName == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	selector := "glimmung.romaine.life/run-slot-name=" + labelValue(slotName)
	path := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + url.QueryEscape(selector)
	for {
		status, list, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				return nil
			}
			return err
		}
		if len(anySlice(list["items"])) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("installer pods for slot %s/%s did not terminate within %s", namespace, slotName, timeout)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) CleanupTestSlotInstaller(ctx context.Context, lease Lease, _ Project) error {
	return l.deleteTestSlotInstaller(ctx, lease)
}

func (l *KubernetesRunLauncher) runTestSlotHelmReconcile(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter, config testSlotHelmSettings, renderMode string) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if minter == nil {
		return fmt.Errorf("github token minter is required for test slot helm %s", renderMode)
	}
	sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
	substitutions := testSlotSubstitutions(lease, project, slotName, sessionsNamespace)
	token, err := minter.InstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("mint github token for test slot %s install: %w", renderMode, err)
	}
	secretName := testSlotHelmSecretName(lease, renderMode)
	if err := l.ensureCloneTokenSecret(ctx, secretName, token, lease, slotName); err != nil {
		return err
	}
	jobName := testSlotHelmJobName(lease, renderMode)
	// Each reconcile must apply fresh. The installer job name is lease+renderMode-
	// keyed, and a finished job lingers for its TTL, so a repeat reconcile — e.g.
	// deploy-image-to-slot deploying image B over a just-deployed A on the same
	// lease — would hit createJob's 409-as-success and silently reuse the stale
	// job, never reconciling the new image (the running-image verify then fails
	// on a slot still serving the old image). Delete the prior job first;
	// Background propagation removes the Job object synchronously so the name is
	// free for the new job, while its pods GC in the background (the verify reads
	// the slot's app pods, not the installer's).
	delPath := "/apis/batch/v1/namespaces/" + l.Settings.RunnerNamespace + "/jobs/" + jobName
	if status, _, err := l.request(ctx, http.MethodDelete, delPath, deleteOptions("Background")); err != nil && status != http.StatusNotFound {
		return err
	}
	if err := l.createJob(ctx, testSlotInstallJobManifest(l.Settings, config, lease, project, substitutions, renderMode)); err != nil {
		return err
	}
	return l.waitForJobComplete(ctx, l.Settings.RunnerNamespace, jobName, 5*time.Minute)
}

func (l *KubernetesRunLauncher) runTestSlotHelmUninstall(ctx context.Context, lease Lease, project Project, config testSlotHelmSettings, renderMode string) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	jobName := testSlotHelmUninstallJobName(lease, renderMode)
	if err := l.createJob(ctx, testSlotUninstallJobManifest(l.Settings, config, lease, project, renderMode)); err != nil {
		return err
	}
	return l.waitForJobComplete(ctx, l.Settings.RunnerNamespace, jobName, 5*time.Minute)
}

func (l *KubernetesRunLauncher) DeprovisionTestSlot(ctx context.Context, lease Lease, project Project) error {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	if strings.TrimSpace(slotName) == "" {
		return nil
	}
	if err := l.deleteTestSlotInstaller(ctx, lease); err != nil {
		return err
	}
	if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
		return err
	}
	if err := l.deleteTestSlotClusterRoleBindings(ctx, slotName); err != nil {
		return err
	}
	sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
	if strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		if err := l.deleteNamespaceAndWait(ctx, sessionsNamespace); err != nil {
			return err
		}
	}
	return l.deleteNamespaceAndWait(ctx, slotName)
}

func (l *KubernetesRunLauncher) deletePlaywrightResources(ctx context.Context, lease Lease, slotName string) error {
	for _, target := range playwrightResourceTargets(lease, slotName) {
		for _, path := range []string{
			"/apis/apps/v1/namespaces/" + target.namespace + "/deployments/" + target.name,
			"/api/v1/namespaces/" + target.namespace + "/services/" + target.name,
		} {
			status, _, err := l.request(ctx, http.MethodDelete, path, deleteOptions("Background"))
			if err != nil && status != http.StatusNotFound {
				return err
			}
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteTestSlotRuntimeResources(ctx context.Context, lease Lease, project Project, slotName string) error {
	namespaces := []string{slotName}
	if sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName); strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		namespaces = append(namespaces, sessionsNamespace)
	}
	for _, namespace := range namespaces {
		for _, collectionPath := range []string{
			"/apis/apps/v1/namespaces/" + namespace + "/deployments",
			"/apis/apps/v1/namespaces/" + namespace + "/statefulsets",
			"/apis/apps/v1/namespaces/" + namespace + "/daemonsets",
			"/apis/batch/v1/namespaces/" + namespace + "/jobs",
			"/api/v1/namespaces/" + namespace + "/pods",
			"/api/v1/namespaces/" + namespace + "/services",
		} {
			if err := l.deleteCollectionItems(ctx, collectionPath); err != nil {
				return err
			}
		}
	}
	if err := l.waitForNoPodsInNamespaces(ctx, namespaces, 5*time.Minute); err != nil {
		return err
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteTestSlotRuntimeWorkloads(ctx context.Context, lease Lease, project Project, slotName string) error {
	namespaces := []string{slotName}
	if sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName); strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		namespaces = append(namespaces, sessionsNamespace)
	}
	for _, namespace := range namespaces {
		for _, collectionPath := range []string{
			"/apis/apps/v1/namespaces/" + namespace + "/deployments",
			"/apis/apps/v1/namespaces/" + namespace + "/statefulsets",
			"/apis/apps/v1/namespaces/" + namespace + "/daemonsets",
			"/apis/batch/v1/namespaces/" + namespace + "/jobs",
			"/api/v1/namespaces/" + namespace + "/pods",
		} {
			if err := l.deleteCollectionItems(ctx, collectionPath); err != nil {
				return err
			}
		}
	}
	return l.waitForNoPodsInNamespaces(ctx, namespaces, 5*time.Minute)
}

func (l *KubernetesRunLauncher) deleteCollectionItems(ctx context.Context, collectionPath string) error {
	status, list, err := l.request(ctx, http.MethodGet, collectionPath, nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" || name == "kubernetes" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, collectionPath+"/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) waitForNoPodsInNamespaces(ctx context.Context, namespaces []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		remaining, err := l.remainingPodsInNamespaces(ctx, namespaces)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime pods did not terminate before cleanup timeout: %s", strings.Join(remaining, ", "))
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) remainingPodsInNamespaces(ctx context.Context, namespaces []string) ([]string, error) {
	var remaining []string
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		status, list, err := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/pods", nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				continue
			}
			return nil, err
		}
		for _, item := range anySlice(list["items"]) {
			name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
			if name == "" {
				continue
			}
			remaining = append(remaining, namespace+"/"+name)
		}
	}
	sort.Strings(remaining)
	return remaining, nil
}

func (l *KubernetesRunLauncher) ensurePlaywrightForNativePhase(ctx context.Context, req RunLaunchRequest) error {
	slotName, _ := stringFromMap(req.Lease.Metadata, "runner_slot_name")
	slotName = strings.TrimSpace(slotName)
	return l.ensurePlaywrightForSlot(ctx, req.Lease, slotName, false)
}

func (l *KubernetesRunLauncher) ensurePlaywrightForSlot(ctx context.Context, lease Lease, slotName string, waitForReady bool) error {
	if !l.Settings.RunnerPlaywrightEnabled {
		return nil
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	name := playwrightResourceName(lease.Project, slotName)
	if name == "" {
		return nil
	}
	labels := playwrightLabels(lease, name)
	if err := l.createDeploymentInNamespace(ctx, slotName, playwrightDeployment(l.Settings, slotName, name, labels)); err != nil {
		return err
	}
	if err := l.createServiceInNamespace(ctx, slotName, playwrightService(l.Settings, slotName, name, labels)); err != nil {
		return err
	}
	if !waitForReady {
		return nil
	}
	return l.waitForDeploymentReady(ctx, slotName, name, 2*time.Minute)
}

func (l *KubernetesRunLauncher) ensureNamespace(ctx context.Context, name string, labels map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   name,
			"labels": labels,
		},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces", body)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesRunLauncher) deleteNamespace(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	status, _, err := l.request(ctx, http.MethodDelete, "/api/v1/namespaces/"+name, deleteOptions("Background"))
	if err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteNamespaceAndWait(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if err := l.deleteNamespace(ctx, name); err != nil {
		return err
	}
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		status, _, err := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+name, nil)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("namespace %s deletion did not complete before deprovision", name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) ensureTestSlotInstallerAccess(ctx context.Context, lease Lease, namespace string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	body := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata": map[string]any{
			"name":      "glim-test-slot-installer",
			"namespace": namespace,
			"labels":    testSlotLabels(lease, slotName),
		},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     firstNonEmpty(l.Settings.RunnerNamespaceRole, "cluster-admin"),
		},
		"subjects": []any{map[string]any{
			"kind":      "ServiceAccount",
			"name":      l.Settings.RunnerServiceAccount,
			"namespace": l.Settings.RunnerNamespace,
		}},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+namespace+"/rolebindings", body)
	if err == nil {
		return nil
	}
	if status != http.StatusConflict {
		return err
	}

	// Existing warm slots may carry stale subjects. Replace the binding
	// instead of assuming any existing glim-test-slot-installer RoleBinding
	// is still correct.
	bindingPath := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + namespace + "/rolebindings/glim-test-slot-installer"
	status, _, err = l.request(ctx, http.MethodDelete, bindingPath, deleteOptions("Background"))
	if err != nil && status != http.StatusNotFound {
		return err
	}
	status, _, err = l.request(ctx, http.MethodPost, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+namespace+"/rolebindings", body)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesRunLauncher) ensureCloneTokenSecret(ctx context.Context, name, token string, lease Lease, slotName string) error {
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": l.Settings.RunnerNamespace,
			"labels":    labels,
		},
		"type":       "Opaque",
		"stringData": map[string]string{"token": token},
	}
	path := "/api/v1/namespaces/" + l.Settings.RunnerNamespace + "/secrets"
	status, _, err := l.request(ctx, http.MethodPost, path, body)
	if err == nil {
		return nil
	}
	if status != http.StatusConflict {
		return err
	}
	existingStatus, existing, getErr := l.request(ctx, http.MethodGet, path+"/"+name, nil)
	if getErr != nil || existingStatus >= 400 {
		return fmt.Errorf("read existing test slot clone secret: status=%d err=%w", existingStatus, getErr)
	}
	if rv := mapStringValueOrEmpty(anyMap(existing["metadata"]), "resourceVersion"); rv != "" {
		body["metadata"].(map[string]any)["resourceVersion"] = rv
	}
	_, _, err = l.request(ctx, http.MethodPut, path+"/"+name, body)
	return err
}

func (l *KubernetesRunLauncher) ensureTestSlotClusterRoleBindings(ctx context.Context, lease Lease, templates []map[string]any, substitutions map[string]string, slotName string) error {
	for _, template := range templates {
		filled := deepFormat(template, substitutions)
		name := strings.TrimSpace(mapStringValueOrEmpty(anyMap(filled["metadata"]), "name"))
		if name == "" {
			name = strings.TrimSpace(mapStringValueOrEmpty(filled, "name"))
		}
		if name == "" {
			continue
		}
		labels := testSlotLabels(lease, slotName)
		labels["glimmung.romaine.life/test-slot"] = "true"
		body := map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRoleBinding",
			"metadata": map[string]any{
				"name":   name,
				"labels": labels,
			},
			"subjects": filled["subjects"],
			"roleRef":  filled["roleRef"],
		}
		path := "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"
		status, _, err := l.request(ctx, http.MethodPost, path, body)
		if err == nil {
			continue
		}
		if status != http.StatusConflict {
			return err
		}
		existingStatus, existing, getErr := l.request(ctx, http.MethodGet, path+"/"+name, nil)
		if getErr != nil || existingStatus >= 400 {
			return fmt.Errorf("read existing test slot clusterrolebinding: status=%d err=%w", existingStatus, getErr)
		}
		if rv := mapStringValueOrEmpty(anyMap(existing["metadata"]), "resourceVersion"); rv != "" {
			body["metadata"].(map[string]any)["resourceVersion"] = rv
		}
		if _, _, err := l.request(ctx, http.MethodPut, path+"/"+name, body); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) ensureTestSlotRoleBindings(ctx context.Context, lease Lease, templates []map[string]any, substitutions map[string]string, slotName string) error {
	for _, template := range templates {
		filled := deepFormat(template, substitutions)
		metadata := anyMap(filled["metadata"])
		name := strings.TrimSpace(mapStringValueOrEmpty(metadata, "name"))
		if name == "" {
			name = strings.TrimSpace(mapStringValueOrEmpty(filled, "name"))
		}
		namespace := strings.TrimSpace(mapStringValueOrEmpty(metadata, "namespace"))
		if namespace == "" {
			namespace = strings.TrimSpace(mapStringValueOrEmpty(filled, "namespace"))
		}
		if name == "" || namespace == "" {
			continue
		}
		labels := testSlotLabels(lease, slotName)
		labels["glimmung.romaine.life/test-slot"] = "true"
		body := map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels":    labels,
			},
			"subjects": filled["subjects"],
			"roleRef":  filled["roleRef"],
		}
		path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + namespace + "/rolebindings"
		status, _, err := l.request(ctx, http.MethodPost, path, body)
		if err == nil {
			continue
		}
		if status != http.StatusConflict {
			return err
		}
		existingStatus, existing, getErr := l.request(ctx, http.MethodGet, path+"/"+name, nil)
		if getErr != nil || existingStatus >= 400 {
			return fmt.Errorf("read existing test slot rolebinding: status=%d err=%w", existingStatus, getErr)
		}
		if rv := mapStringValueOrEmpty(anyMap(existing["metadata"]), "resourceVersion"); rv != "" {
			body["metadata"].(map[string]any)["resourceVersion"] = rv
		}
		if _, _, err := l.request(ctx, http.MethodPut, path+"/"+name, body); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteTestSlotClusterRoleBindings(ctx context.Context, slotName string) error {
	selector := "glimmung.romaine.life/run-slot-name=" + labelValue(slotName)
	path := "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings?labelSelector=" + url.QueryEscape(selector)
	status, list, err := l.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteRetiredTestSlotSessionClusterRoleBindings(ctx context.Context, slotName string) error {
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	for _, name := range []string{slotName + "-session-cluster-admin", slotName + "-session-readonly"} {
		status, _, err := l.request(ctx, http.MethodDelete, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteTestSlotInstaller(ctx context.Context, lease Lease) error {
	for _, path := range []string{
		"/apis/batch/v1/namespaces/" + l.Settings.RunnerNamespace + "/jobs/" + testSlotInstallerJobName(lease),
		"/api/v1/namespaces/" + l.Settings.RunnerNamespace + "/secrets/" + testSlotInstallerSecretName(lease),
		"/apis/batch/v1/namespaces/" + l.Settings.RunnerNamespace + "/jobs/" + testSlotInstallResourceName("glim-helm-install", lease),
		"/api/v1/namespaces/" + l.Settings.RunnerNamespace + "/secrets/" + testSlotInstallResourceName("glim-helm-clone", lease),
	} {
		status, _, err := l.request(ctx, http.MethodDelete, path, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	if slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name"); strings.TrimSpace(slotName) != "" {
		if err := l.deleteRunnerResourcesBySlot(ctx, "/apis/batch/v1/namespaces/"+l.Settings.RunnerNamespace+"/jobs", slotName); err != nil {
			return err
		}
		if err := l.deleteRunnerResourcesBySlot(ctx, "/api/v1/namespaces/"+l.Settings.RunnerNamespace+"/secrets", slotName); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) deleteRunnerResourcesBySlot(ctx context.Context, collectionPath, slotName string) error {
	selector := "glimmung.romaine.life/run-slot-name=" + labelValue(slotName)
	status, list, err := l.request(ctx, http.MethodGet, collectionPath+"?labelSelector="+url.QueryEscape(selector), nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, collectionPath+"/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesRunLauncher) ensureAttemptSecret(ctx context.Context, name, attemptBase, jobID string) (string, error) {
	token := uuid.New().String()
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": l.Settings.RunnerNamespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by":       "glimmung",
				"glimmung.romaine.life/run-attempt":  "true",
				"glimmung.romaine.life/attempt-base": labelValue(attemptBase),
				"glimmung.romaine.life/job-id":       labelValue(jobID),
			},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"attempt-token": token},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces/"+l.Settings.RunnerNamespace+"/secrets", body)
	if err == nil || status == http.StatusConflict {
		if status == http.StatusConflict {
			existingStatus, existing, getErr := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+l.Settings.RunnerNamespace+"/secrets/"+name, nil)
			if getErr != nil || existingStatus >= 400 {
				return "", fmt.Errorf("read existing runner attempt secret: status=%d err=%w", existingStatus, getErr)
			}
			if encoded, ok := anyMap(existing["data"])["attempt-token"].(string); ok && encoded != "" {
				return encoded, nil
			}
		}
		return token, nil
	}
	return "", err
}

func (l *KubernetesRunLauncher) createJob(ctx context.Context, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+l.Settings.RunnerNamespace+"/jobs", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

// GetRunnerJobStatus fetches the batch/v1 Job status for a previously
// launched runner-phase Job. Returns Found=false when the Job no longer
// exists (TTL-collected by k8s). Used by the run-execution reconciler to
// detect runs whose backing Job died (DeadlineExceeded, BackoffLimitExceeded,
// eviction) without the runner ever delivering its completion callback.
func (l *KubernetesRunLauncher) GetRunnerJobStatus(ctx context.Context, namespace, name string) (RunnerJobStatus, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return RunnerJobStatus{}, fmt.Errorf("namespace and name are required")
	}
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name
	status, job, err := l.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		if status == http.StatusNotFound {
			return RunnerJobStatus{Found: false}, nil
		}
		return RunnerJobStatus{}, err
	}
	parsed := parseRunnerJobStatus(job)
	// The Job condition only ever says BackoffLimitExceeded for a
	// backoffLimit=0 pod that OOM'd / was evicted / crashed. Read the
	// pod-level reason so the operator can tell those apart. Best-effort
	// and only on terminal failure to bound the extra API call.
	if parsed.IsTerminallyFailed() {
		if reason, perr := l.podTerminationReasonForJob(ctx, namespace, name); perr == nil && reason != "" {
			parsed.PodTerminationReason = reason
		}
	}
	return parsed, nil
}

// podTerminationReasonForJob returns the most specific pod-level
// termination reason for a Job's pod: the container's
// state.terminated.reason ("OOMKilled", "Error", "ContainerCannotRun")
// when present, else the pod's status.reason ("Evicted"). Returns the
// empty string when no pod exists (TTL race) or no terminal reason was
// recorded.
func (l *KubernetesRunLauncher) podTerminationReasonForJob(ctx context.Context, namespace, jobName string) (string, error) {
	listPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=job-name%3D" + url.QueryEscape(jobName)
	status, decoded, err := l.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		if status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	items := anySlice(decoded["items"])
	if len(items) == 0 {
		return "", nil
	}
	podStatus := anyMap(anyMap(items[0])["status"])
	for _, raw := range anySlice(podStatus["containerStatuses"]) {
		term := anyMap(anyMap(anyMap(raw)["state"])["terminated"])
		if r := strings.TrimSpace(mapStringValueOrEmpty(term, "reason")); r != "" {
			return r, nil
		}
	}
	return strings.TrimSpace(mapStringValueOrEmpty(podStatus, "reason")), nil
}

// GetRunnerJobLogs reads a bounded slice of the named Job's pod
// stdout. Used by the run-execution reconciler to capture logs into
// the artifact store on terminal transitions (inner-Job observation
// contract, stage 3). Returns the empty slice with a nil error when:
//
//   - the Job has no pods registered (TTL'd already, no kubelet
//     materialised a pod for it, etc), or
//   - the pod's logs are server-side empty.
//
// Errors are returned for transport / RBAC failures so the caller can
// decide whether to retry on the next reconciler tick.
//
// The kube-apiserver applies tailLines server-side; the LimitReader on
// the response stream is a defense against an apiserver implementation
// that ignores the parameter or against a pod whose lines are
// individually enormous (base64 evidence tarballs in the original
// ambience#170 case were exactly that shape).
func (l *KubernetesRunLauncher) GetRunnerJobLogs(ctx context.Context, namespace, jobName string, maxBytes int64) ([]byte, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(jobName) == "" {
		return nil, fmt.Errorf("namespace and jobName are required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultInnerJobLogsMaxBytes
	}
	podName, err := l.lookupSinglePodForJob(ctx, namespace, jobName)
	if err != nil {
		return nil, err
	}
	if podName == "" {
		return nil, nil
	}
	// Kubernetes log endpoint streams plain text, not JSON, so we
	// bypass request() (which json-decodes) and drive the HTTP request
	// here. tailLines caps server-side; the LimitReader caps client-side.
	logPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(podName) + "/log?tailLines=20000&timestamps=true&previous=false"
	body, status, err := l.getRaw(ctx, logPath, maxBytes)
	if err != nil {
		if status == http.StatusNotFound {
			// Pod GC'd between the lookup and the log read.
			return nil, nil
		}
		return nil, err
	}
	return body, nil
}

const (
	// defaultInnerJobLogsMaxBytes is the per-Job cap on log capture.
	// 8 MiB fits a long-running agent's stdout including a few base64
	// evidence tarballs and stays well below typical apiserver
	// response-size limits. Operators tuning lower should set the
	// caller's maxBytes; tuning higher is rarely useful since the
	// dashboard renders the artifact via download anyway.
	defaultInnerJobLogsMaxBytes int64 = 8 << 20
)

// lookupSinglePodForJob resolves a Job's pod name by querying the
// kubelet's pod LIST for a `job-name=` label match. Returns the empty
// string when no pod exists for the Job (TTL race or unscheduled).
func (l *KubernetesRunLauncher) lookupSinglePodForJob(ctx context.Context, namespace, jobName string) (string, error) {
	listPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=job-name%3D" + url.QueryEscape(jobName)
	status, decoded, err := l.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		if status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	items := anySlice(decoded["items"])
	if len(items) == 0 {
		return "", nil
	}
	first := anyMap(items[0])
	metadata := anyMap(first["metadata"])
	return strings.TrimSpace(mapStringValueOrEmpty(metadata, "name")), nil
}

// getRaw GETs the given kube apiserver path and returns up to
// maxBytes of the response body. Mirrors request()'s auth handling
// but does not json-decode; used for the streaming pod log endpoint.
func (l *KubernetesRunLauncher) getRaw(ctx context.Context, path string, maxBytes int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(l.Settings.K8sAPIHost, "/")+path, nil)
	if err != nil {
		return nil, 0, err
	}
	token, err := os.ReadFile(l.Settings.K8sSATokenPath)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	client := l.HTTPClient
	if client == nil {
		// Log reads can be slow (apiserver streams from kubelet).
		// 30s is generous but bounded.
		client = &http.Client{Timeout: 30 * time.Second, Transport: l.transport()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, resp.StatusCode, fmt.Errorf("kubernetes GET %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseRunnerJobStatus pulls the reconciler-relevant fields out of a
// batch/v1 Job object returned by the kube apiserver.
func parseRunnerJobStatus(job map[string]any) RunnerJobStatus {
	out := RunnerJobStatus{Found: true}
	statusMap := anyMap(job["status"])
	if active, ok := positiveIntFromMap(statusMap, "active"); ok {
		out.Active = active
	}
	if succeeded, ok := positiveIntFromMap(statusMap, "succeeded"); ok {
		out.Succeeded = succeeded
	}
	if failed, ok := positiveIntFromMap(statusMap, "failed"); ok {
		out.Failed = failed
	}
	if ct := strings.TrimSpace(mapStringValueOrEmpty(statusMap, "completionTime")); ct != "" {
		if ts, err := time.Parse(time.RFC3339Nano, ct); err == nil {
			out.CompletionTime = ts.UTC()
		} else if ts, err := time.Parse(time.RFC3339, ct); err == nil {
			out.CompletionTime = ts.UTC()
		}
	}
	for _, raw := range anySlice(statusMap["conditions"]) {
		typed := anyMap(raw)
		cond := RunnerJobCondition{
			Type:    strings.TrimSpace(mapStringValueOrEmpty(typed, "type")),
			Status:  strings.TrimSpace(mapStringValueOrEmpty(typed, "status")),
			Reason:  strings.TrimSpace(mapStringValueOrEmpty(typed, "reason")),
			Message: strings.TrimSpace(mapStringValueOrEmpty(typed, "message")),
		}
		if lt := strings.TrimSpace(mapStringValueOrEmpty(typed, "lastTransitionTime")); lt != "" {
			if ts, err := time.Parse(time.RFC3339Nano, lt); err == nil {
				cond.LastTransitionTime = ts.UTC()
			} else if ts, err := time.Parse(time.RFC3339, lt); err == nil {
				cond.LastTransitionTime = ts.UTC()
			}
			if cond.LastTransitionTime.After(out.LastTransitionTime) {
				out.LastTransitionTime = cond.LastTransitionTime
			}
		}
		out.Conditions = append(out.Conditions, cond)
	}
	return out
}

func (l *KubernetesRunLauncher) waitForJobComplete(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name
	for {
		status, job, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound {
				return fmt.Errorf("test slot runtime installer job %s/%s not found", namespace, name)
			}
			return err
		}
		for _, condition := range anySlice(anyMap(job["status"])["conditions"]) {
			typed := anyMap(condition)
			conditionType := strings.TrimSpace(mapStringValueOrEmpty(typed, "type"))
			conditionStatus := strings.TrimSpace(mapStringValueOrEmpty(typed, "status"))
			if conditionType == "Complete" && strings.EqualFold(conditionStatus, "True") {
				return nil
			}
			if conditionType == "Failed" && strings.EqualFold(conditionStatus, "True") {
				return fmt.Errorf("test slot runtime installer job %s/%s failed", namespace, name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime installer job %s/%s did not complete before timeout", namespace, name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) waitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	path := "/apis/apps/v1/namespaces/" + namespace + "/deployments/" + name
	for {
		status, deployment, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound {
				return fmt.Errorf("test slot runtime deployment %s/%s not found", namespace, name)
			}
			return err
		}
		statusMap := anyMap(deployment["status"])
		readyReplicas, _ := positiveIntFromMap(statusMap, "readyReplicas")
		availableReplicas, _ := positiveIntFromMap(statusMap, "availableReplicas")
		if readyReplicas > 0 && availableReplicas > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime deployment %s/%s did not become ready before timeout", namespace, name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesRunLauncher) createDeploymentInNamespace(ctx context.Context, namespace string, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/apis/apps/v1/namespaces/"+namespace+"/deployments", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesRunLauncher) createServiceInNamespace(ctx context.Context, namespace string, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/services", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesRunLauncher) request(ctx context.Context, method, path string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.Settings.K8sAPIHost, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	token, err := os.ReadFile(l.Settings.K8sSATokenPath)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := l.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, Transport: l.transport()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil, fmt.Errorf("kubernetes %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if len(respBody) == 0 {
		return resp.StatusCode, map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, decoded, nil
}

func (l *KubernetesRunLauncher) transport() http.RoundTripper {
	ca, err := os.ReadFile(l.Settings.K8sCACertPath)
	if err != nil {
		return http.DefaultTransport
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return http.DefaultTransport
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
}

func runnerJobManifest(settings Settings, req RunLaunchRequest, job RunnerJobSpec, jobName, secretName, attemptBase string, proxyRuntimes ...providerAPIProxyRuntime) map[string]any {
	proxyRuntime := providerAPIProxyRuntime{}
	if len(proxyRuntimes) > 0 {
		proxyRuntime = proxyRuntimes[0]
	}
	req.Phase = CanonicalRunnerPhase(req.Phase)
	for _, canonical := range req.Phase.Jobs {
		if canonical.ID == job.ID {
			job = canonical
			break
		}
	}
	labels := map[string]string{
		"app.kubernetes.io/managed-by":       "glimmung",
		"glimmung.romaine.life/run-job":      "true",
		"glimmung.romaine.life/project":      labelValue(req.Lease.Project),
		"glimmung.romaine.life/workflow":     labelValue(req.Workflow.Name),
		"glimmung.romaine.life/run-ref":      labelValue(runRefFromData(req.Run)),
		"glimmung.romaine.life/phase":        labelValue(req.Phase.Name),
		"glimmung.romaine.life/attempt-base": labelValue(attemptBase),
		"glimmung.romaine.life/job-id":       labelValue(job.ID),
	}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	podLabels["azure.workload.identity/use"] = "true"
	env := runnerJobEnv(settings, req, job, secretName)
	volumeMounts := []any{
		map[string]any{"name": "glimmung-attempt-token", "mountPath": "/var/run/glimmung", "readOnly": true},
	}
	volumes := []any{
		map[string]any{"name": "glimmung-attempt-token", "secret": map[string]any{"secretName": secretName}},
	}
	podSpec := map[string]any{
		"serviceAccountName": settings.RunnerServiceAccount,
		"restartPolicy":      "Never",
	}
	if proxyRuntime.enabled() {
		caMountPath := "/etc/glimmung-provider-api-proxy-ca"
		bundleMountPath := filepath.Dir(proxyRuntime.CABundlePath)
		env = append(env,
			map[string]any{"name": "NODE_EXTRA_CA_CERTS", "value": caMountPath + "/ca.crt"},
			map[string]any{"name": "SSL_CERT_FILE", "value": proxyRuntime.CABundlePath},
			map[string]any{"name": "REQUESTS_CA_BUNDLE", "value": proxyRuntime.CABundlePath},
			map[string]any{"name": "GIT_SSL_CAINFO", "value": proxyRuntime.CABundlePath},
			map[string]any{"name": "GLIMMUNG_PROVIDER_API_PROXY_CLAUDE_IP", "value": proxyRuntime.ClaudeClusterIP},
			map[string]any{"name": "GLIMMUNG_PROVIDER_API_PROXY_CODEX_IP", "value": proxyRuntime.CodexClusterIP},
			map[string]any{"name": "GLIMMUNG_PROVIDER_API_PROXY_CA_SECRET", "value": proxyRuntime.CASecretName},
			map[string]any{"name": "GLIMMUNG_PROVIDER_API_PROXY_CA_BUNDLE", "value": proxyRuntime.CABundlePath},
		)
		if runnerJobRequiresProviderAPIProxy(job) && strings.TrimSpace(proxyRuntime.GitHubClusterIP) != "" {
			env = append(env, map[string]any{"name": "GLIMMUNG_PROVIDER_API_PROXY_GITHUB_IP", "value": proxyRuntime.GitHubClusterIP})
		}
		volumeMounts = append(volumeMounts,
			map[string]any{"name": "provider-api-proxy-ca", "mountPath": caMountPath, "readOnly": true},
			map[string]any{"name": "provider-api-proxy-ca-bundle", "mountPath": bundleMountPath, "readOnly": true},
		)
		volumes = append(volumes,
			map[string]any{"name": "provider-api-proxy-ca", "secret": map[string]any{"secretName": proxyRuntime.CASecretName}},
			map[string]any{"name": "provider-api-proxy-ca-bundle", "emptyDir": map[string]any{}},
		)
		hostAliases := []any{
			map[string]any{"ip": proxyRuntime.ClaudeClusterIP, "hostnames": []any{"api.anthropic.com"}},
			map[string]any{"ip": proxyRuntime.CodexClusterIP, "hostnames": []any{"chatgpt.com", "api.openai.com"}},
		}
		if runnerJobRequiresProviderAPIProxy(job) && strings.TrimSpace(proxyRuntime.GitHubClusterIP) != "" {
			hostAliases = append(hostAliases, map[string]any{"ip": proxyRuntime.GitHubClusterIP, "hostnames": []any{"github.com"}})
		}
		podSpec["hostAliases"] = hostAliases
		podSpec["initContainers"] = []any{
			map[string]any{
				"name":    "provider-api-proxy-ca-bundle",
				"image":   runnerJobImage(settings, job),
				"command": []any{"/bin/sh", "-ec", "if [ -f /etc/ssl/certs/ca-certificates.crt ]; then cat /etc/ssl/certs/ca-certificates.crt /proxy-ca/ca.crt > " + proxyRuntime.CABundlePath + "; else cp /proxy-ca/ca.crt " + proxyRuntime.CABundlePath + "; fi && chmod 0444 " + proxyRuntime.CABundlePath},
				"volumeMounts": []any{
					map[string]any{"name": "provider-api-proxy-ca", "mountPath": "/proxy-ca", "readOnly": true},
					map[string]any{"name": "provider-api-proxy-ca-bundle", "mountPath": bundleMountPath},
				},
			},
		}
	}
	container := map[string]any{
		"name":         dnsLabel(job.ID),
		"image":        runnerJobImage(settings, job),
		"env":          env,
		"volumeMounts": volumeMounts,
	}
	if job.Managed {
		container["command"] = []string{runnerEntrypoint(settings)}
	} else if len(job.Command) > 0 {
		container["command"] = job.Command
	}
	if !job.Managed && len(job.Args) > 0 {
		container["args"] = job.Args
	}
	containers := []any{container}

	// Opt-in runner MCP sidecar: when a job declares tools, run the scoped
	// runner MCP server alongside the agent and share /workspace so tools such
	// as upload_evidence can read what the agent produced. Jobs that declare no
	// tools get no sidecar and no extra volume — their pod spec is unchanged.
	if len(job.Tools) > 0 {
		workspaceMount := map[string]any{"name": "runner-workspace", "mountPath": "/workspace"}
		volumes = append(volumes, map[string]any{"name": "runner-workspace", "emptyDir": map[string]any{}})
		container["volumeMounts"] = append(append([]any{}, volumeMounts...), workspaceMount)
		// Tell the agent container where the scoped sidecar lives. The runner
		// reads GLIMMUNG_RUNNER_MCP_URL and injects exactly this server into the
		// agent's provider MCP config; absence of the var means no runner tools
		// and no MCP config — the default, unchanged path.
		container["env"] = append(append([]map[string]any{}, env...), map[string]any{"name": "GLIMMUNG_RUNNER_MCP_URL", "value": runnerMCPURL})
		containers = append(containers, map[string]any{
			"name":         "runner-mcp",
			"image":        runnerJobImage(settings, job),
			"command":      []any{"/app/glimmung-runner-mcp"},
			"env":          runnerMCPSidecarEnv(settings, req, job),
			"volumeMounts": []any{workspaceMount},
		})
	}

	podSpec["volumes"] = volumes
	podSpec["containers"] = containers
	if job.TimeoutSeconds != nil && *job.TimeoutSeconds > 0 {
		podSpec["activeDeadlineSeconds"] = *job.TimeoutSeconds
	}
	annotations := map[string]string{}
	if req.Run.IssueRepo != "" {
		annotations["glimmung.romaine.life/github-policy-repo"] = req.Run.IssueRepo
	}
	if branch := runnerPolicyBranchName(req); branch != "" {
		annotations["glimmung.romaine.life/github-policy-ref"] = "refs/heads/" + branch
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"namespace":   settings.RunnerNamespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"backoffLimit":            runnerJobBackoffLimit(req.Phase),
			"ttlSecondsAfterFinished": settings.RunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels":      podLabels,
					"annotations": annotations,
				},
				"spec": podSpec,
			},
		},
	}
}

// runnerTeardownJobBackoffLimit lets a teardown Job absorb a transient
// pod-start failure (image pull blip, node churn) instead of instantly
// failing the cleanup phase. Teardown scripts (env-destroy) are idempotent,
// so re-running a fresh pod is safe. Producer/verify jobs keep backoffLimit=0
// (Glimmung owns their retries at the attempt level and wants fast failure
// detection).
const runnerTeardownJobBackoffLimit = 2

// runnerJobBackoffLimit returns the Kubernetes Job backoffLimit for a phase's
// jobs. Teardown phases are post-verdict cleanup and verdict-neutral, so a
// bounded retry self-heals transient pod-start blips; every other phase fails
// fast with backoffLimit=0.
func runnerJobBackoffLimit(phase PhaseSpec) int {
	if phase.Purpose == PhasePurposeTeardown {
		return runnerTeardownJobBackoffLimit
	}
	return 0
}

func runnerJobImage(settings Settings, job RunnerJobSpec) string {
	if job.Managed {
		return firstNonEmpty(job.Image, settings.RunnerImage)
	}
	return job.Image
}

// runnerMCPListenAddr is the loopback address the runner MCP sidecar serves on,
// and runnerMCPURL is the streamable-HTTP endpoint the agent container connects
// to. They are a matched pair: the sidecar binds the addr, the launcher hands
// the agent the URL. Loopback keeps the surface pod-local — nothing off the pod
// can reach the sidecar's privileged tools.
const (
	runnerMCPListenAddr = "127.0.0.1:8765"
	runnerMCPURL        = "http://" + runnerMCPListenAddr + "/mcp"
)

// runnerPlaywrightWSEndpoint returns the cluster-internal WebSocket URL of the
// run's leased slot browser, or "" when Playwright is disabled or the lease has
// no slot. Both the agent container and the runner MCP sidecar's capture tools
// connect to it.
func runnerPlaywrightWSEndpoint(settings Settings, req RunLaunchRequest) string {
	if !settings.RunnerPlaywrightEnabled {
		return ""
	}
	slotName, ok := req.Lease.Metadata["runner_slot_name"].(string)
	if !ok || strings.TrimSpace(slotName) == "" {
		return ""
	}
	return fmt.Sprintf("ws://%s.%s.svc.cluster.local:%s", playwrightResourceName(req.Lease.Project, slotName), slotName, settings.RunnerPlaywrightPort)
}

// runnerMCPSidecarEnv is the minimal environment the runner MCP sidecar needs:
// the run identity (to scope artifacts), the artifact store coordinates, the
// per-job tool allow-list, the loopback address it serves on, and — when the
// run holds a slot browser — the Playwright endpoint the capture tools drive. It
// is deliberately narrow: the sidecar holds only what its tools require, not the
// agent container's full environment.
func runnerMCPSidecarEnv(settings Settings, req RunLaunchRequest, job RunnerJobSpec) []map[string]any {
	env := []map[string]any{
		{"name": "GLIMMUNG_PROJECT", "value": req.Lease.Project},
		{"name": "GLIMMUNG_RUN_ID", "value": req.Run.ID},
		{"name": "GLIMMUNG_RUN_REF", "value": runRefFromData(req.Run)},
		{"name": "ARTIFACTS_STORAGE_ACCOUNT", "value": settings.ArtifactsStorageAccount},
		{"name": "ARTIFACTS_CONTAINER", "value": settings.ArtifactsContainer},
		{"name": "GLIMMUNG_RUNNER_TOOLS", "value": strings.Join(job.Tools, ",")},
		{"name": "GLIMMUNG_RUNNER_MCP_ADDR", "value": runnerMCPListenAddr},
	}
	if endpoint := runnerPlaywrightWSEndpoint(settings, req); endpoint != "" {
		env = append(env, map[string]any{"name": "PLAYWRIGHT_WS_ENDPOINT", "value": endpoint})
	}
	return env
}

func runnerJobEnv(settings Settings, req RunLaunchRequest, job RunnerJobSpec, secretName string) []map[string]any {
	metadata := req.Lease.Metadata
	baseURL := strings.TrimRight(settings.RunnerCallbackBaseURL, "/")
	callback := ""
	if req.Run.CallbackToken != nil {
		callback = *req.Run.CallbackToken
	}
	runnerPath := "/v1/run-callbacks/" + callback + "/run"
	env := []map[string]any{
		{"name": "GLIMMUNG_BASE_URL", "value": baseURL},
		{"name": "GLIMMUNG_PROJECT", "value": req.Lease.Project},
		{"name": "GLIMMUNG_WORKFLOW", "value": req.Workflow.Name},
		{"name": "GLIMMUNG_PHASE", "value": req.Phase.Name},
		{"name": "GLIMMUNG_RUN_ID", "value": req.Run.ID},
		{"name": "GLIMMUNG_RUN_REF", "value": runRefFromData(req.Run)},
		{"name": "GLIMMUNG_JOB_ID", "value": job.ID},
		{"name": "GLIMMUNG_ATTEMPT_INDEX", "value": strconv.Itoa(runnerAttemptIndex(req))},
		{"name": "GLIMMUNG_LEASE_REF", "value": leasePublicRef(req.Lease)},
		{"name": "GLIMMUNG_EVENTS_URL", "value": baseURL + runnerPath + "/events"},
		{"name": "GLIMMUNG_STATUS_URL", "value": baseURL + runnerPath + "/status"},
		{"name": "GLIMMUNG_COMPLETED_URL", "value": baseURL + runnerPath + "/completed"},
		{"name": "GLIMMUNG_GITHUB_TOKEN_URL", "value": baseURL + runnerPath + "/github-token"},
		{"name": "GLIMMUNG_GITHUB_AGENT_TOKEN_URL", "value": baseURL + runnerPath + "/github-agent-token"},
		{"name": "GLIMMUNG_PR_REVIEW_URL", "value": baseURL + runnerPath + "/pr-review"},
		{"name": "GLIMMUNG_PR_MERGE_URL", "value": baseURL + runnerPath + "/pr-merge"},
		// Remote-host execution primitives (docs/remote-host-execution.md).
		// The phase script POSTs a freshly-generated public key to the
		// ssh-cert URL and POSTs an empty body to the tailscale-authkey
		// URL. Same auth model as the other run-callback URLs above —
		// callback token is baked into the path; X-Glimmung-Attempt-Token
		// header carries the secret.
		{"name": "GLIMMUNG_SSH_CERT_URL", "value": baseURL + runnerPath + "/ssh-cert"},
		{"name": "GLIMMUNG_TAILSCALE_AUTHKEY_URL", "value": baseURL + runnerPath + "/tailscale-authkey"},
		{"name": "GLIMMUNG_ATTEMPT_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "attempt-token"}}},
	}
	seen := envNameSet(env)
	for _, key := range []string{"issue_repo", "issue_number", "issue_title", "issue_body", "runner_slot_index", "runner_slot_name", "entrypoint_job_id", "entrypoint_step_slug", "work_context_id", "work_context_branch", "work_context_base_ref", "work_context_state"} {
		if value, ok := metadata[key]; ok {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_"+envName(key), fmt.Sprint(value))
		}
	}
	if slotName := mapStringValueOrEmpty(metadata, "runner_slot_name"); slotName != "" {
		env = appendLiteralEnv(env, seen, "GLIMMUNG_VALIDATION_NAMESPACE", slotName)
	}
	if phaseInputs := anyMap(metadata["phase_inputs"]); len(phaseInputs) > 0 {
		keys := make([]string, 0, len(phaseInputs))
		for key := range phaseInputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_INPUT_"+envName(key), fmt.Sprint(phaseInputs[key]))
		}
	}
	if runInputs := launchRunInputs(req); len(runInputs) > 0 {
		for _, key := range sortedRunInputKeys(runInputs) {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_RUN_INPUT_"+envName(key), runInputs[key])
		}
	}
	if rawRequirements := metadata["evidence_requirements"]; rawRequirements != nil {
		if payload, err := json.Marshal(rawRequirements); err == nil && string(payload) != "null" {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_EVIDENCE_REQUIREMENTS_JSON", string(payload))
		}
	}
	if req.Run.PriorVerification != nil {
		// Recycle cycles carry the parent cycle's deciding verification so
		// stage prompts can address the previous failure (status, reasons,
		// and the structured failure block) instead of rediscovering it.
		if payload, err := json.Marshal(req.Run.PriorVerification); err == nil && string(payload) != "null" {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_PRIOR_VERIFICATION_JSON", string(payload))
		}
	}
	if rawRuntime := metadata["agent_runtime"]; rawRuntime != nil {
		if payload, err := json.Marshal(rawRuntime); err == nil && string(payload) != "null" {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_AGENT_RUNTIME_JSON", string(payload))
		}
	}
	// The agent's main container is deliberately NOT given a Playwright WS
	// endpoint. Browser evidence is captured only through the credential-isolated
	// runner-MCP sidecar (capture_video / capture_screenshot), which connects to
	// the leased slot browser and records AFTER the page paints. Handing the
	// agent the raw endpoint is what let per-repo capture scripts connect to the
	// slot browser and self-capture (recordVideo from about:blank → white first
	// frame) — the exact path this program deletes. The sidecar receives the
	// endpoint via runnerMCPSidecarEnv; TestRunnerJobEnvOmitsBrowserEndpoint
	// fails if it is reintroduced into the agent env.
	if job.Managed {
		env = appendLiteralEnv(env, seen, "GLIMMUNG_RUNNER_JOB_SPEC", runnerJobSpecJSON(job))
	}
	jobEnvNames := make([]string, 0, len(job.Env))
	for name := range job.Env {
		jobEnvNames = append(jobEnvNames, name)
	}
	sort.Strings(jobEnvNames)
	for _, name := range jobEnvNames {
		env = appendLiteralEnv(env, seen, name, job.Env[name])
	}
	return env
}

func runnerPolicyBranchName(req RunLaunchRequest) string {
	metadata := req.Lease.Metadata
	if branch := strings.TrimSpace(fmt.Sprint(firstAny(metadata["implementation_branch"], metadata["implementationBranch"]))); branch != "" && branch != "<nil>" {
		return branch
	}
	issueNumber := req.Run.IssueNumber
	if issueNumber <= 0 {
		if raw := strings.TrimSpace(fmt.Sprint(metadata["issue_number"])); raw != "" && raw != "<nil>" {
			issueNumber, _ = strconv.Atoi(raw)
		}
	}
	if issueNumber > 0 && strings.TrimSpace(req.Run.ID) != "" {
		return "glimmung/issue-" + runnerPolicyRefSegment(strconv.Itoa(issueNumber)) + "/" + runnerPolicyRefSegment(req.Run.ID)
	}
	if branch := strings.TrimSpace(fmt.Sprint(metadata["work_context_branch"])); branch != "" && branch != "<nil>" {
		return branch
	}
	if strings.TrimSpace(req.Run.ID) != "" {
		return "glimmung/" + runnerPolicyRefSegment(req.Run.ID)
	}
	return ""
}

func runnerPolicyRefSegment(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "unknown"
	}
	return out
}

func runnerEntrypoint(settings Settings) string {
	return firstNonEmpty(settings.RunnerEntrypoint, "/app/glimmung-runner")
}

func runnerJobSpecJSON(job RunnerJobSpec) string {
	payload, err := json.Marshal(job)
	if err != nil {
		return "{}"
	}
	return string(payload)
}

func envNameSet(env []map[string]any) map[string]bool {
	seen := map[string]bool{}
	for _, row := range env {
		if name, ok := row["name"].(string); ok && strings.TrimSpace(name) != "" {
			seen[name] = true
		}
	}
	return seen
}

func appendLiteralEnv(env []map[string]any, seen map[string]bool, name, value string) []map[string]any {
	name = strings.TrimSpace(name)
	if name == "" || seen[name] {
		return env
	}
	seen[name] = true
	return append(env, map[string]any{"name": name, "value": value})
}

type testSlotHelmSettings struct {
	InstallerImage      string
	ChartPath           string
	GitRef              string
	Values              map[string]string
	SetStringValues     map[string]string
	ClusterRoleBindings []map[string]any
	RoleBindings        []map[string]any
}

const (
	defaultTestSlotInstallerImage = "alpine/k8s:1.30.0"
	defaultTestSlotChartPath      = "k8s"
)

func testSlotHelmConfig(project Project) (testSlotHelmSettings, bool) {
	raw, ok := mapFromMap(project.Metadata, "test_slot_helm")
	if !ok {
		raw, ok = mapFromMap(project.Metadata, "testSlotHelm")
	}
	if !ok || !boolConfigValue(raw, "enabled") {
		return testSlotHelmSettings{}, false
	}
	values := stringMapFromAnyMap(anyMap(raw["values"]))
	setStringValues := stringMapFromAnyMap(anyMap(firstAny(raw["set_string_values"], raw["setStringValues"])))
	clusterRoleBindings := mapSliceFromAnySlice(anySlice(firstAny(raw["cluster_role_bindings"], raw["clusterRoleBindings"])))
	if len(clusterRoleBindings) == 0 {
		clusterRoleBindings = defaultTestSlotClusterRoleBindings(project)
	}
	roleBindings := mapSliceFromAnySlice(anySlice(firstAny(raw["role_bindings"], raw["roleBindings"])))
	if len(roleBindings) == 0 {
		roleBindings = defaultTestSlotRoleBindings(project)
	}
	return testSlotHelmSettings{
		InstallerImage:      firstNonEmpty(configString(raw, "installer_image", "installerImage"), defaultTestSlotInstallerImage),
		ChartPath:           firstNonEmpty(strings.Trim(configString(raw, "chart_path", "chartPath"), "/"), defaultTestSlotChartPath),
		GitRef:              configString(raw, "git_ref", "gitRef"),
		Values:              values,
		SetStringValues:     setStringValues,
		ClusterRoleBindings: clusterRoleBindings,
		RoleBindings:        roleBindings,
	}, true
}

func defaultTestSlotClusterRoleBindings(project Project) []map[string]any {
	if !isTankOperatorProject(project) {
		return nil
	}
	return []map[string]any{
		{
			"metadata": map[string]any{"name": "{slot_name}-auth-delegator"},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "{slot_name}",
				"namespace": "{slot_name}",
			}},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "system:auth-delegator",
			},
		},
	}
}

func defaultTestSlotRoleBindings(project Project) []map[string]any {
	if !isTankOperatorProject(project) {
		return nil
	}
	return []map[string]any{
		{
			// Slot session SA is read-only inside the slot app namespace: the
			// agent can inspect pods/services/logs, but cannot write or exec.
			"metadata": map[string]any{
				"name":      "{slot_name}-session-readonly",
				"namespace": "{slot_name}",
			},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "{slot_name}-session",
				"namespace": "{sessions_namespace}",
			}},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "view",
			},
		},
		{
			// The same read-only surface is needed in the sessions namespace
			// for kubectl get/describe/logs against session pods.
			"metadata": map[string]any{
				"name":      "{slot_name}-session-readonly",
				"namespace": "{sessions_namespace}",
			},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "{slot_name}-session",
				"namespace": "{sessions_namespace}",
			}},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "view",
			},
		},
	}
}

func testSlotSubstitutions(lease Lease, project Project, slotName, sessionsNamespace string) map[string]string {
	slotIndex := mapStringValueOrEmpty(lease.Metadata, "runner_slot_index")
	if slotIndex == "" {
		slotIndex = trailingSlotIndex(slotName)
	}
	host := ""
	if value := testSlotURL(project, &slotName); value != nil {
		host = strings.TrimPrefix(strings.TrimSuffix(*value, "/"), "https://")
	}
	return map[string]string{
		"slot_name":          slotName,
		"slot_index":         slotIndex,
		"sessions_namespace": sessionsNamespace,
		"host":               host,
		"project":            project.Name,
	}
}

func testSlotSessionsNamespace(slotName string, project Project) string {
	for _, key := range []string{"test_slot_helm", "testSlotHelm"} {
		if config, ok := mapFromMap(project.Metadata, key); ok {
			if value := configString(config, "sessions_namespace", "sessionsNamespace"); value != "" {
				return formatSubstitutions(value, map[string]string{"slot_name": slotName})
			}
		}
	}
	return slotName + "-sessions"
}

func testSlotSessionsNamespaceForLease(lease Lease, project Project, slotName string) string {
	if value, ok := stringFromMap(lease.Metadata, "runner_sessions_namespace"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return testSlotSessionsNamespace(slotName, project)
}

func testSlotInstallResourceName(prefix string, lease Lease) string {
	ref := LeasePublicRefFromLease(lease)
	if strings.TrimSpace(ref) == "" {
		ref = lease.Project
	}
	attemptIndex := 0
	if lease.LeaseNumber != nil && *lease.LeaseNumber > 0 {
		attemptIndex = *lease.LeaseNumber
	}
	return compactResourceName(prefix, ref, attemptIndex)
}

func testSlotInstallerJobName(lease Lease) string {
	return testSlotHelmJobName(lease, testSlotRenderModeHot)
}

func testSlotInstallerSecretName(lease Lease) string {
	return testSlotHelmSecretName(lease, testSlotRenderModeHot)
}

func testSlotHelmJobName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotInstallerJobPrefix+"-"+renderMode, lease)
}

func testSlotHelmSecretName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotInstallerSecretPrefix+"-"+renderMode, lease)
}

func testSlotHelmUninstallJobName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotUninstallJobPrefix+"-"+renderMode, lease)
}

func testSlotHelmReleaseName(slotName, renderMode string) string {
	slotName = strings.TrimSpace(slotName)
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	base := dnsLabel(slotName + "-" + renderMode)
	if len(base) <= 63 {
		return base
	}
	hash := sha256.Sum256([]byte(base))
	return dnsLabel(slotName + "-" + hex.EncodeToString(hash[:])[:16])
}

func testSlotInstallJobManifest(settings Settings, config testSlotHelmSettings, lease Lease, project Project, substitutions map[string]string, renderMode string) map[string]any {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	jobName := testSlotHelmJobName(lease, renderMode)
	secretName := testSlotHelmSecretName(lease, renderMode)
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	labels["glimmung.romaine.life/render-mode"] = labelValue(renderMode)
	releaseName := testSlotHelmReleaseName(slotName, renderMode)
	gitRef := strings.TrimSpace(config.GitRef)
	cloneScript := "set -eu\n" +
		"GIT_REF=" + shellQuote(gitRef) + "\n" +
		"TOKEN=\"$(cat /var/run/glim-clone/token)\"\n" +
		"REPO_URL=\"https://x-access-token:${TOKEN}@github.com/" + project.GitHubRepo + ".git\"\n" +
		"if [ -n \"$GIT_REF\" ]; then\n" +
		// Fetch the exact ref by branch/tag name OR commit sha. deploy-image-to-slot
		// pins GIT_REF to the verified HEAD sha so the chart and the CI image
		// are the same commit (anti-TOCTOU); `git clone --branch <sha>` is
		// invalid, but `git fetch` accepts a reachable sha (GitHub
		// allowReachableSHA1InWant) or a ref name, so this clone serves both
		// the branch-ref reconcile and the sha-pinned deploy.
		"  cd /workspace\n" +
		"  git init -q\n" +
		"  git remote add origin \"$REPO_URL\"\n" +
		"  git fetch --depth=1 origin \"$GIT_REF\"\n" +
		"  git checkout -q FETCH_HEAD\n" +
		"else\n" +
		"  git clone --depth 1 \"$REPO_URL\" /workspace\n" +
		"fi\n"
	installScript := "set -eu\n" +
		"cd /workspace\n" +
		"if ! helm status " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " >/dev/null 2>&1; then\n" +
		"  " + helmTemplateCommand(config, releaseName, slotName, substitutions, renderMode) + " | " + stripClusterScopedCommand() + " | kubectl delete --ignore-not-found=true -f -\n" +
		"fi\n" +
		helmUpgradeInstallCommand(config, releaseName, slotName, substitutions, renderMode) + "\n"
	podSpec := map[string]any{
		"serviceAccountName": settings.RunnerServiceAccount,
		"restartPolicy":      "Never",
		"volumes": []any{
			map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
			map[string]any{"name": "glim-clone", "secret": map[string]any{"secretName": secretName, "defaultMode": 0400}},
		},
		"initContainers": []any{map[string]any{
			"name":    "clone",
			"image":   "alpine/git:latest",
			"command": []string{"sh", "-c", cloneScript},
			"volumeMounts": []any{
				map[string]any{"name": "workspace", "mountPath": "/workspace"},
				map[string]any{"name": "glim-clone", "mountPath": "/var/run/glim-clone", "readOnly": true},
			},
		}},
		"containers": []any{map[string]any{
			"name":    "install",
			"image":   config.InstallerImage,
			"command": []string{"sh", "-c", installScript},
			"env": []any{
				map[string]any{"name": "GLIM_SLOT_NAME", "value": slotName},
				map[string]any{"name": "GLIM_SLOT_INDEX", "value": substitutions["slot_index"]},
				map[string]any{"name": "GLIM_HOST", "value": substitutions["host"]},
				map[string]any{"name": "GLIM_PROJECT", "value": project.Name},
			},
			"volumeMounts": []any{map[string]any{"name": "workspace", "mountPath": "/workspace"}},
		}},
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"namespace":   settings.RunnerNamespace,
			"labels":      labels,
			"annotations": map[string]string{"glimmung.romaine.life/run-slot-name": slotName},
		},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": settings.RunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func testSlotUninstallJobManifest(settings Settings, config testSlotHelmSettings, lease Lease, project Project, renderMode string) map[string]any {
	slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	jobName := testSlotHelmUninstallJobName(lease, renderMode)
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	labels["glimmung.romaine.life/render-mode"] = labelValue(renderMode)
	releaseName := testSlotHelmReleaseName(slotName, renderMode)
	uninstallScript := "set -eu\n" +
		"if helm status " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " >/dev/null 2>&1; then\n" +
		"  helm uninstall " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " --wait --timeout 180s\n" +
		"fi\n"
	podSpec := map[string]any{
		"serviceAccountName": settings.RunnerServiceAccount,
		"restartPolicy":      "Never",
		"containers": []any{map[string]any{
			"name":    "uninstall",
			"image":   firstNonEmpty(config.InstallerImage, defaultTestSlotInstallerImage),
			"command": []string{"sh", "-c", uninstallScript},
			"env": []any{
				map[string]any{"name": "GLIM_SLOT_NAME", "value": slotName},
				map[string]any{"name": "GLIM_PROJECT", "value": project.Name},
				map[string]any{"name": "GLIM_RENDER_MODE", "value": renderMode},
			},
		}},
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"namespace":   settings.RunnerNamespace,
			"labels":      labels,
			"annotations": map[string]string{"glimmung.romaine.life/run-slot-name": slotName},
		},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": settings.RunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func helmTemplateCommand(config testSlotHelmSettings, releaseName, namespace string, substitutions map[string]string, renderMode string) string {
	parts := []string{
		"helm", "template", shellQuote(releaseName), shellQuote(config.ChartPath),
		"--namespace", shellQuote(namespace),
	}
	for key, value := range testSlotHelmValues(config, renderMode) {
		parts = append(parts, "--set", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	for key, value := range config.SetStringValues {
		parts = append(parts, "--set-string", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	return strings.Join(parts, " ")
}

func helmUpgradeInstallCommand(config testSlotHelmSettings, releaseName, namespace string, substitutions map[string]string, renderMode string) string {
	parts := []string{
		"helm", "upgrade", "--install", shellQuote(releaseName), shellQuote(config.ChartPath),
		"--namespace", shellQuote(namespace),
		"--wait", "--wait-for-jobs", "--timeout", "180s",
	}
	for key, value := range testSlotHelmValues(config, renderMode) {
		parts = append(parts, "--set", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	for key, value := range config.SetStringValues {
		parts = append(parts, "--set-string", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	return strings.Join(parts, " ")
}

func testSlotHelmValues(config testSlotHelmSettings, renderMode string) map[string]string {
	values := make(map[string]string, len(config.Values)+2)
	for key, value := range config.Values {
		values[key] = value
	}
	values["testEnv.slotName"] = "{slot_name}"
	values["renderMode"] = renderMode
	return values
}

func stripClusterScopedCommand() string {
	awk := `awk 'BEGIN { doc=""; skip=0 } /^---[[:space:]]*$/ { if (doc != "" && skip == 0) printf "%s---\n", doc; doc=""; skip=0; next } { doc = doc $0 "\n"; if ($0 ~ /^kind:[[:space:]]*(ClusterRole|ClusterRoleBinding|Namespace)[[:space:]]*$/) skip=1 } END { if (doc != "" && skip == 0) printf "%s", doc }'`
	return "if command -v yq >/dev/null 2>&1; then yq 'select(.kind != \"ClusterRoleBinding\" and .kind != \"ClusterRole\" and .kind != \"Namespace\")'; else " + awk + "; fi"
}

func testSlotLabels(lease Lease, slotName string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":        "glimmung",
		"glimmung.romaine.life/test-slot":     "true",
		"glimmung.romaine.life/project":       labelValue(lease.Project),
		"glimmung.romaine.life/run-slot-name": labelValue(slotName),
	}
	if value := mapStringValueOrEmpty(lease.Metadata, "runner_slot_index"); value != "" {
		labels["glimmung.romaine.life/run-slot-index"] = labelValue(value)
	}
	if ref := LeasePublicRefFromLease(lease); ref != "" {
		labels["glimmung.romaine.life/lease-ref"] = labelValue(ref)
	}
	return labels
}

func deleteOptions(policy string) map[string]any {
	return map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"propagationPolicy": policy,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func formatSubstitutions(value string, substitutions map[string]string) string {
	for key, replacement := range substitutions {
		value = strings.ReplaceAll(value, "{"+key+"}", replacement)
	}
	return value
}

func deepFormat(raw map[string]any, substitutions map[string]string) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = deepFormatValue(value, substitutions)
	}
	return out
}

func deepFormatValue(raw any, substitutions map[string]string) any {
	switch value := raw.(type) {
	case string:
		return formatSubstitutions(value, substitutions)
	case map[string]any:
		return deepFormat(value, substitutions)
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, deepFormatValue(item, substitutions))
		}
		return out
	default:
		return raw
	}
}

func configString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := stringFromMap(values, key); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boolConfigValue(values map[string]any, key string) bool {
	raw, ok := values[key]
	if !ok {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(value)
		return err == nil && parsed
	default:
		return false
	}
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringMapFromAnyMap(values map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func mapSliceFromAnySlice(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if mapped := anyMap(value); len(mapped) > 0 {
			out = append(out, mapped)
		}
	}
	return out
}

func anySlice(raw any) []any {
	if value, ok := raw.([]any); ok {
		return value
	}
	return nil
}

func trailingSlotIndex(slotName string) string {
	suffix := slotName[strings.LastIndex(slotName, "-")+1:]
	if _, err := strconv.Atoi(suffix); err == nil {
		return suffix
	}
	return ""
}

func playwrightResourceName(_ string, slotName string) string {
	if strings.TrimSpace(slotName) == "" {
		return ""
	}
	return "slot-playwright"
}

// PlaywrightWSEndpointFor returns the cluster-internal WebSocket URL of the
// leased slot's `slot-playwright` Service, or nil when the cluster is not
// running playwright-enabled slots or `slotName` is blank. mcp-glimmung and
// other in-cluster callers connect to this URL to drive a Chromium running
// inside the lease, instead of spawning one on a shared host.
func PlaywrightWSEndpointFor(settings Settings, slotName string) *string {
	if !settings.RunnerPlaywrightEnabled {
		return nil
	}
	slot := strings.TrimSpace(slotName)
	if slot == "" {
		return nil
	}
	name := playwrightResourceName("", slot)
	if name == "" {
		return nil
	}
	port := strings.TrimSpace(settings.RunnerPlaywrightPort)
	if port == "" {
		port = "3000"
	}
	endpoint := fmt.Sprintf("ws://%s.%s.svc.cluster.local:%s", name, slot, port)
	return &endpoint
}

func playwrightLabels(lease Lease, name string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":          "glimmung",
		"app.kubernetes.io/part-of":             "glimmung-test-slot",
		"app.kubernetes.io/name":                name,
		"glimmung.romaine.life/slot-playwright": "true",
		"glimmung.romaine.life/resource-scope":  "tool",
		"glimmung.romaine.life/tool":            "playwright",
		"glimmung.romaine.life/project":         labelValue(lease.Project),
		"glimmung.romaine.life/run-slot-name":   labelValue(mapStringValueOrEmpty(lease.Metadata, "runner_slot_name")),
	}
	if value := mapStringValueOrEmpty(lease.Metadata, "runner_slot_index"); value != "" {
		labels["glimmung.romaine.life/run-slot-index"] = labelValue(value)
	}
	if ref := LeasePublicRefFromLease(lease); ref != "" {
		labels["glimmung.romaine.life/lease-ref"] = labelValue(ref)
	}
	return labels
}

type playwrightResourceTarget struct {
	namespace string
	name      string
}

func playwrightResourceTargets(lease Lease, slotName string) []playwrightResourceTarget {
	var targets []playwrightResourceTarget
	if name := playwrightResourceName(lease.Project, slotName); name != "" {
		targets = append(targets, playwrightResourceTarget{namespace: slotName, name: name})
	}
	return targets
}

func playwrightDeployment(settings Settings, namespace, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.RunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":  "playwright",
						"image": settings.RunnerPlaywrightImage,
						"command": []string{
							"npx", "playwright", "run-server",
							"--host", "0.0.0.0",
							"--port", strconv.Itoa(port),
						},
						"ports": []any{map[string]any{"name": "ws", "containerPort": port}},
						"env":   []any{map[string]any{"name": "PLAYWRIGHT_BROWSERS_PATH", "value": "/ms-playwright"}},
						"resources": map[string]any{
							"requests": map[string]string{"cpu": "100m", "memory": "256Mi"},
							"limits":   map[string]string{"cpu": "1000m", "memory": "1Gi"},
						},
					}},
				},
			},
		},
	}
}

func playwrightService(settings Settings, namespace, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.RunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"selector": map[string]string{"app.kubernetes.io/name": name},
			"ports":    []any{map[string]any{"name": "ws", "port": port, "targetPort": "ws"}},
		},
	}
}

func mapStringValueOrEmpty(values map[string]any, key string) string {
	value, _ := stringFromMap(values, key)
	return value
}

func runnerAttemptIndex(req RunLaunchRequest) int {
	if v, ok := req.Lease.Metadata["attempt_index"]; ok {
		if n, err := strconv.Atoi(fmt.Sprint(v)); err == nil && n >= 0 {
			return n
		}
	}
	if len(req.Run.Attempts) > 0 {
		return req.Run.Attempts[len(req.Run.Attempts)-1].AttemptIndex
	}
	return 0
}

func runnerJobName(attemptBase, jobID string) string {
	suffix := dnsLabel(jobID)
	candidate := attemptBase + "-" + suffix
	if len(candidate) <= 63 {
		return strings.Trim(candidate, "-")
	}
	hash := sha256.Sum256([]byte(jobID))
	head := strings.TrimRight(attemptBase[:min(len(attemptBase), 54)], "-")
	return head + "-" + hex.EncodeToString(hash[:])[:8]
}

var nonDNSLabel = regexp.MustCompile(`[^a-z0-9-]+`)
var nonEnvName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func dnsLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = nonDNSLabel.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "job"
	}
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func labelValue(value string) string {
	value = dnsLabel(value)
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func envName(value string) string {
	value = strings.ToUpper(nonEnvName.ReplaceAllString(value, "_"))
	return strings.Trim(value, "_")
}

func compactResourceName(prefix, value string, attemptIndex int) string {
	hash := sha256.Sum256([]byte(value))
	base := dnsLabel(prefix + "-" + value + "-" + strconv.Itoa(attemptIndex))
	if len(base) <= 63 {
		return base
	}
	return dnsLabel(prefix + "-" + hex.EncodeToString(hash[:])[:16] + "-" + strconv.Itoa(attemptIndex))
}

func anyMap(raw any) map[string]any {
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	if value, ok := raw.(map[string]string); ok {
		out := make(map[string]any, len(value))
		for key, val := range value {
			out[key] = val
		}
		return out
	}
	return map[string]any{}
}

package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/hotswap"
)

// ApplyHotSwapOptions describes the inputs to the server-side
// dispatcher. The HTTP handler builds this from the request + the
// resolved lease + the project contract.
type ApplyHotSwapOptions struct {
	Project            string
	ArtifactKind       string
	GitRef             string
	RepoURL            string
	RepoToken          string
	TargetNamespace    string
	SlotName           string
	ValidationTarget   string
	JobNamespace       string
	SwapContainerImage string
	ServiceAccount     string
	Contract           hotswap.Contract
	// Timeout bounds the build-and-swap end to end. It is enforced as the
	// Job's spec.activeDeadlineSeconds — k8s fails the Job with reason
	// DeadlineExceeded if it overruns — so the deadline lives on the durable
	// Kubernetes object, not on a held HTTP connection. The finalizer maps
	// that reason to Outcome="timeout".
	Timeout time.Duration
	// BaseRef / HeadRef / ChangedFiles carry the diff context for the
	// project's fidelity classifier. glimmung resolves these server-side
	// (GitHub Compare API) because the build Job's shallow single-SHA
	// checkout cannot compute a real diff. HeadRef mirrors GitRef. They are
	// passed into the build container as GLIMMUNG_BASE_REF / GLIMMUNG_HEAD_REF
	// / GLIMMUNG_CHANGED_FILES.
	BaseRef      string
	HeadRef      string
	ChangedFiles []string
}

// ApplyHotSwapResult is the structured outcome returned to the caller.
type ApplyHotSwapResult struct {
	JobName          string            `json:"job_name"`
	JobNamespace     string            `json:"job_namespace"`
	ArtifactKind     string            `json:"artifact_kind"`
	GitRef           string            `json:"git_ref"`
	ValidationTarget string            `json:"validation_target,omitempty"`
	Outcome          string            `json:"outcome"` // persisted | build_failed | swap_failed | timeout
	BuildLogsTail    string            `json:"build_logs_tail,omitempty"`
	SwapLogsTail     string            `json:"swap_logs_tail,omitempty"`
	Error            string            `json:"error,omitempty"`
	Timings          map[string]string `json:"timings"`
}

// k8sJobClient is the surface ApplyHotSwap needs from the k8s API.
// In production this is implemented by httpK8sJobClient (talks to the
// kubernetes API over HTTP using the in-cluster SA token, exactly like
// KubernetesRunLauncher.request). Tests inject a fake.
//
// Carving this as a small interface keeps ApplyHotSwap pure-logic and
// avoids the kubectl-shell-out approach that broke the first cut of
// this endpoint (glimmung pod has no kubectl in the runtime image).
type k8sJobClient interface {
	ApplyJob(ctx context.Context, namespace string, spec map[string]any) error
	GetPodLogs(ctx context.Context, namespace, podLabelSelector, container string) (string, error)
	DeleteJob(ctx context.Context, namespace, name string) error
}

// DispatchHotSwap renders and submits the build-and-swap Job, then returns
// immediately with Outcome="running" and the job handle. It does NOT wait for
// completion — the build-and-swap deadline lives on the Job
// (activeDeadlineSeconds) and the gated apply-hot-swap finalizer records the
// terminal outcome durably, so neither the result nor the timeout depends on a
// held HTTP connection. The Job has two containers:
//
//  1. Init container (contract.<kind>.builder_image): git clone +
//     contract.<kind>.build_command. Leaves a source dir at /work/source
//     (static/runner) or a single executable at /work/artifact (backend).
//  2. Main container (alpine/k8s): resolves target pods via
//     kubectl-get against contract.<kind>.pod_selector, then for each
//     pod either tar-streams /work/source into contract.<kind>.target
//     (static/runner) or streams /work/artifact onto the target file and
//     health-gates the SIGHUP re-exec (backend), sending the configured
//     restart signal where one is set.
//
// Supports static web assets (static), the orchestrator backend binary
// (backend), and session-pod runner artifacts (agent_runner,
// codex_runner, antigravity_runner). Static targets the slot's app pods
// and is served live from an override dir, so the swap clears that dir
// first and sends no restart signal. Backend also targets the slot's app
// pods, but streams a single executable to a file the supervisor re-execs
// from on SIGHUP and then health-gates the result. Runner kinds extract a
// directory into a session-pod volume and SIGHUP their supervisor.
func DispatchHotSwap(ctx context.Context, k8s k8sJobClient, opts ApplyHotSwapOptions) (result ApplyHotSwapResult, err error) {
	result = ApplyHotSwapResult{
		ArtifactKind:     opts.ArtifactKind,
		GitRef:           opts.GitRef,
		JobNamespace:     opts.JobNamespace,
		ValidationTarget: opts.ValidationTarget,
		Outcome:          "swap_failed",
		Timings:          map[string]string{},
	}

	art, ok := resolveArtifact(opts.Contract, opts.ArtifactKind)
	if !ok {
		result.Error = fmt.Sprintf("artifact_kind %q is not supported by the apply endpoint (supported: static, backend, agent_runner, codex_runner, antigravity_runner)", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	if !art.Enabled {
		result.Error = fmt.Sprintf("contract.%s is not enabled", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(art.BuilderImage) == "" {
		result.Error = fmt.Sprintf("contract.%s.builder_image is required", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(art.PodSelector) == "" {
		result.Error = fmt.Sprintf("contract.%s.pod_selector is required", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(art.Container) == "" {
		result.Error = fmt.Sprintf("contract.%s.container is required", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	// Slot-name substitution: some projects label their app pods / name their
	// app container by the slot name (e.g. chess-tactics uses app=<slot_name>),
	// not a static label. The contract expresses that with a {slot_name} token,
	// filled here from the resolved lease. Static-label contracts (e.g.
	// tank-operator's app.kubernetes.io/name=tank-operator) carry no token and
	// pass through unchanged.
	if opts.SlotName != "" {
		art.PodSelector = strings.ReplaceAll(art.PodSelector, "{slot_name}", opts.SlotName)
		art.Container = strings.ReplaceAll(art.Container, "{slot_name}", opts.SlotName)
	}
	if strings.TrimSpace(opts.TargetNamespace) == "" {
		result.Error = "target_namespace is required"
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(opts.RepoURL) == "" {
		result.Error = "repo_url is required (typically from project metadata's github_repo)"
		return result, fmt.Errorf("%s", result.Error)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	if strings.TrimSpace(opts.ValidationTarget) == "" {
		opts.ValidationTarget = "existing_session"
		result.ValidationTarget = opts.ValidationTarget
	}
	if opts.JobNamespace == "" {
		// glimmung-runs is where the glimmung pod's SA has Job/create
		// RBAC (via the glimmung-run-launcher Role). The glimmung
		// namespace itself doesn't grant Job/create to the orchestrator's
		// own SA — by design, since glimmung's namespace is for the
		// orchestrator deployment, not for dispatched workloads.
		opts.JobNamespace = "glimmung-runs"
	}
	result.JobNamespace = opts.JobNamespace
	if opts.ServiceAccount == "" {
		// glimmung-runner is the SA for dispatched workloads in
		// glimmung-runs. The apply-hot-swap Job's swap container runs
		// `kubectl get/exec` against pods in the target slot's session
		// namespace; the cross-namespace pods/get+list+exec permission
		// is granted via charts/.../templates/runner-pods-exec-rbac.yaml
		// (ClusterRole bound to this SA).
		opts.ServiceAccount = "glimmung-runner"
	}
	if opts.SwapContainerImage == "" {
		// Bitnami deprecated their free public Docker Hub catalog —
		// version-pinned tags like `bitnami/kubectl:1.31` started
		// disappearing in late 2025, and the `bitnamilegacy/*`
		// migration repo is itself EOL on 2026-08-28 with no security
		// updates after that. `bitnami/kubectl:latest` still resolves
		// today but sits on the same dying catalog.
		//
		// alpine/k8s is an independently-maintained image (Docker Hub
		// `alpine` org) that ships sh + kubectl + tar (plus helm, jq,
		// yq, curl, bash). The sibling `glim-slot-apply-*` Job on this
		// cluster already uses alpine/k8s:1.30.0 so node-cache hits
		// are common. The swap container's contract is sh + kubectl +
		// tar, so alpine/k8s is the closest drop-in that doesn't
		// depend on Bitnami's commercial decisions.
		//
		// A caller can override SwapContainerImage to pin to a digest
		// if reproducibility matters or to point at an ACR mirror.
		opts.SwapContainerImage = "alpine/k8s:1.31.13"
	}

	jobName := "apply-hot-swap-" + randHex(8)
	result.JobName = jobName

	// The build-and-swap deadline lives on the Job, not on this request:
	// activeDeadlineSeconds makes k8s fail the Job (reason DeadlineExceeded)
	// if it overruns, which the finalizer maps to Outcome="timeout". This is
	// why the endpoint can return immediately and still honor timeout_seconds.
	deadlineSeconds := 0
	if opts.Timeout > 0 {
		deadlineSeconds = int(opts.Timeout / time.Second)
		if deadlineSeconds < 1 {
			deadlineSeconds = 1
		}
	}

	spec := renderApplyHotSwapJobSpec(applyHotSwapJobInputs{
		JobName:               jobName,
		JobNamespace:          opts.JobNamespace,
		ServiceAccount:        opts.ServiceAccount,
		Project:               opts.Project,
		SlotName:              opts.SlotName,
		ArtifactKind:          opts.ArtifactKind,
		GitRef:                opts.GitRef,
		RepoURL:               opts.RepoURL,
		RepoToken:             opts.RepoToken,
		BaseRef:               opts.BaseRef,
		HeadRef:               opts.HeadRef,
		ChangedFiles:          opts.ChangedFiles,
		BuilderImage:          art.BuilderImage,
		BuildCommand:          art.BuildCommand,
		FidelityCommand:       opts.Contract.FidelityClassifier.Command,
		SwapContainerImage:    opts.SwapContainerImage,
		Source:                art.Source,
		Target:                art.Target,
		TargetNamespace:       opts.TargetNamespace,
		TargetPodSelector:     art.PodSelector,
		TargetContainer:       art.Container,
		ValidationTarget:      opts.ValidationTarget,
		RestartSignal:         art.Restart,
		CleanTarget:           art.CleanTarget,
		ArtifactFile:          art.ArtifactFile,
		HealthPath:            art.HealthPath,
		HealthPort:            art.HealthPort,
		ActiveDeadlineSeconds: deadlineSeconds,
	})

	applyStart := time.Now()
	if err := k8s.ApplyJob(ctx, opts.JobNamespace, spec); err != nil {
		result.Outcome = "swap_failed"
		result.Error = fmt.Sprintf("apply job: %v", err)
		result.Timings["job_apply"] = time.Since(applyStart).String()
		return result, err
	}
	result.Timings["job_apply"] = time.Since(applyStart).String()

	// Dispatched. The Job runs to completion — or hits its
	// activeDeadlineSeconds — independent of this request; the gated
	// apply-hot-swap finalizer records the terminal outcome durably. Report
	// "running" plus the job handle so the caller can poll for the result.
	result.Outcome = "running"
	return result, nil
}

// finalizeHotSwapInputs is the terminal-Job context the finalizer needs to
// build a structured outcome. The gated apply-hot-swap watcher derives it from
// the Job's labels + name, not from the original request — so finalize is
// independent of whoever (if anyone) is still connected.
type finalizeHotSwapInputs struct {
	JobName          string
	JobNamespace     string
	ArtifactKind     string
	GitRef           string
	ValidationTarget string
}

// finalizeHotSwap turns an already-terminal apply-hot-swap Job into a
// structured outcome: it collects the build + swap log tails and classifies
// the result (persisted | build_failed | swap_failed | timeout). It does NOT
// wait — the caller invokes it only after the apiserver has reported the Job
// terminal, so the durable outcome never depends on a held connection. Pure
// read + classify; the caller owns history append + Job cleanup.
func finalizeHotSwap(ctx context.Context, k8s k8sJobClient, in finalizeHotSwapInputs, succeeded bool, failureReason string) ApplyHotSwapResult {
	result := ApplyHotSwapResult{
		JobName:          in.JobName,
		JobNamespace:     in.JobNamespace,
		ArtifactKind:     in.ArtifactKind,
		GitRef:           in.GitRef,
		ValidationTarget: in.ValidationTarget,
		Timings:          map[string]string{},
	}
	podSelector := "job-name=" + in.JobName
	buildLogs, _ := k8s.GetPodLogs(ctx, in.JobNamespace, podSelector, "build")
	result.BuildLogsTail = tailLog(buildLogs, 4000)
	swapLogs, _ := k8s.GetPodLogs(ctx, in.JobNamespace, podSelector, "swap")
	result.SwapLogsTail = tailLog(swapLogs, 4000)

	if succeeded {
		result.Outcome = "persisted"
		return result
	}
	switch strings.TrimSpace(failureReason) {
	case "DeadlineExceeded":
		result.Outcome = "timeout"
		result.Error = "hot-swap job exceeded its deadline (activeDeadlineSeconds)"
	default:
		// Distinguish a build failure from a swap failure by the logs: if the
		// swap container produced no output, or the build logs carry an error,
		// the build is the likelier culprit.
		if strings.TrimSpace(swapLogs) == "" || strings.Contains(strings.ToLower(buildLogs), "error") {
			result.Outcome = "build_failed"
		} else {
			result.Outcome = "swap_failed"
		}
		reason := strings.TrimSpace(failureReason)
		if reason == "" {
			reason = "job failed"
		}
		result.Error = fmt.Sprintf("hot-swap job failed (reason=%s)", reason)
	}
	return result
}

// resolvedArtifact is the normalized per-kind hot-swap shape the Job
// builder consumes. Runner kinds map their AgentRunnerContract onto it;
// static maps StaticContract; backend maps BackendContract. Static
// introduces two differences: it is served live, so CleanTarget clears
// stale content-hashed assets before extract and Restart is empty (no
// PID-1 signal). Backend introduces two more: ArtifactFile is set (the
// build produces one executable file streamed to Target, not a directory
// extracted into Target), and HealthPath/HealthPort drive a post-restart
// health gate so a crashing binary fails the swap instead of reporting
// success.
type resolvedArtifact struct {
	Enabled      bool
	Source       string
	Target       string
	BuildCommand string
	PodSelector  string
	Container    string
	Restart      string // empty = served live, no restart signal
	BuilderImage string
	CleanTarget  bool
	// ArtifactFile, when non-empty, switches the Job to single-file backend
	// semantics: the build copies this one executable (an absolute path in
	// the builder image) to /work/artifact, and the swap streams it to
	// Target (a file), chmod +x, atomic mv, then restarts. Source is unused
	// in this mode.
	ArtifactFile string
	// HealthPath/HealthPort, when set (backend only), gate the swap: after
	// the restart signal the swap container polls
	// http://127.0.0.1:<HealthPort><HealthPath> inside the target pod until a
	// 2xx or timeout. No 2xx => the swap container exits non-zero => the Job
	// fails => Outcome=swap_failed.
	HealthPath string
	HealthPort int
}

func resolveArtifact(contract hotswap.Contract, artifactKind string) (resolvedArtifact, bool) {
	fromRunner := func(c hotswap.AgentRunnerContract) resolvedArtifact {
		return resolvedArtifact{
			Enabled:      c.Enabled,
			Source:       c.Source,
			Target:       c.Target,
			BuildCommand: c.BuildCommand,
			PodSelector:  c.PodSelector,
			Container:    c.Container,
			Restart:      c.Restart,
			BuilderImage: c.BuilderImage,
		}
	}
	switch artifactKind {
	case "agent_runner":
		return fromRunner(contract.AgentRunner), true
	case "codex_runner":
		return fromRunner(contract.CodexRunner), true
	case "antigravity_runner":
		return fromRunner(contract.AntigravityRunner), true
	case "static":
		s := contract.Static
		return resolvedArtifact{
			Enabled:      s.Enabled,
			Source:       s.Source,
			Target:       s.Target,
			BuildCommand: s.BuildCommand,
			PodSelector:  s.PodSelector,
			Container:    s.Container,
			Restart:      "", // static is served live; no restart
			BuilderImage: s.BuilderImage,
			CleanTarget:  true,
		}, true
	case "backend":
		b := contract.Backend
		return resolvedArtifact{
			Enabled:      b.Enabled,
			Target:       b.Target,
			BuildCommand: b.BuildCommand,
			PodSelector:  b.PodSelector,
			Container:    b.Container,
			Restart:      "SIGHUP", // supervisor re-execs PID 1 from Target
			BuilderImage: b.BuilderImage,
			ArtifactFile: b.Artifact,
			HealthPath:   b.HealthPath,
			HealthPort:   b.HealthPort,
		}, true
	default:
		return resolvedArtifact{}, false
	}
}

type applyHotSwapJobInputs struct {
	JobName            string
	JobNamespace       string
	ServiceAccount     string
	Project            string
	SlotName           string
	ArtifactKind       string
	GitRef             string
	RepoURL            string
	RepoToken          string
	BaseRef            string
	HeadRef            string
	ChangedFiles       []string
	BuilderImage       string
	BuildCommand       string
	FidelityCommand    string
	SwapContainerImage string
	Source             string
	Target             string
	TargetNamespace    string
	TargetPodSelector  string
	TargetContainer    string
	ValidationTarget   string
	RestartSignal      string
	CleanTarget        bool
	// ActiveDeadlineSeconds, when > 0, is set as the Job's
	// spec.activeDeadlineSeconds so Kubernetes enforces the build-and-swap
	// timeout on the durable Job object (reason DeadlineExceeded on overrun).
	ActiveDeadlineSeconds int
	// ArtifactFile (backend only) is the absolute path in the builder image
	// of the single executable to stream to Target. Non-empty switches the
	// build + swap scripts to single-file semantics.
	ArtifactFile string
	// HealthPath/HealthPort (backend only) drive the post-restart health gate
	// the swap container runs inside the target pod.
	HealthPath string
	HealthPort int
}

func renderApplyHotSwapJobSpec(in applyHotSwapJobInputs) map[string]any {
	validationTarget := strings.TrimSpace(in.ValidationTarget)
	if validationTarget == "" {
		validationTarget = "existing_session"
	}
	labels := map[string]any{
		"app.kubernetes.io/name":                 "glimmung-apply-hot-swap",
		"glimmung.io/project":                    in.Project,
		"glimmung.io/apply-hot-swap-kind":        in.ArtifactKind,
		"glimmung.io/hot-swap-validation-target": validationTarget,
	}
	// slot-name is the join key the gated finalizer uses to resolve which
	// leased slot's hot-swap history this Job's terminal outcome belongs to.
	if strings.TrimSpace(in.SlotName) != "" {
		labels["glimmung.io/slot-name"] = in.SlotName
	}
	buildScript := buildScriptFor(in)
	swapScript := swapScriptFor(in)
	podSpec := map[string]any{
		"restartPolicy": "Never",
		"volumes": []any{
			map[string]any{"name": "work", "emptyDir": map[string]any{}},
		},
		"initContainers": []any{
			map[string]any{
				"name":    "build",
				"image":   in.BuilderImage,
				"command": []any{"sh", "-c"},
				"args":    []any{buildScript},
				"env": []any{
					map[string]any{"name": "GIT_REF", "value": in.GitRef},
					map[string]any{"name": "REPO_URL", "value": in.RepoURL},
					map[string]any{"name": "GIT_TOKEN", "value": in.RepoToken},
					map[string]any{"name": "GLIMMUNG_HOT_SWAP_ARTIFACT_KIND", "value": in.ArtifactKind},
					map[string]any{"name": "GLIMMUNG_HOT_SWAP_VALIDATION_TARGET", "value": in.ValidationTarget},
					// Diff context for the fidelity classifier. glimmung
					// resolves these server-side because the shallow build
					// checkout cannot compute a real diff; the classifier
					// prefers GLIMMUNG_CHANGED_FILES over its own git fallback.
					map[string]any{"name": "GLIMMUNG_BASE_REF", "value": in.BaseRef},
					map[string]any{"name": "GLIMMUNG_HEAD_REF", "value": in.HeadRef},
					map[string]any{"name": "GLIMMUNG_CHANGED_FILES", "value": strings.Join(in.ChangedFiles, "\n")},
				},
				"volumeMounts": []any{
					map[string]any{"name": "work", "mountPath": "/work"},
				},
			},
		},
		"containers": []any{
			map[string]any{
				"name":    "swap",
				"image":   in.SwapContainerImage,
				"command": []any{"sh", "-c"},
				"args":    []any{swapScript},
				"volumeMounts": []any{
					map[string]any{"name": "work", "mountPath": "/work"},
				},
			},
		},
	}
	if strings.TrimSpace(in.ServiceAccount) != "" {
		podSpec["serviceAccountName"] = in.ServiceAccount
	}
	jobSpec := map[string]any{
		"backoffLimit":            0,
		"ttlSecondsAfterFinished": 600,
		"template": map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec":     podSpec,
		},
	}
	if in.ActiveDeadlineSeconds > 0 {
		jobSpec["activeDeadlineSeconds"] = in.ActiveDeadlineSeconds
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      in.JobName,
			"namespace": in.JobNamespace,
			"labels":    labels,
		},
		"spec": jobSpec,
	}
}

func buildScriptFor(in applyHotSwapJobInputs) string {
	// Init container: clone the repo at GIT_REF, run the build command,
	// leave the resulting source dir at /work/source.
	//
	// `git` is always required (it's how the source enters the container)
	// but it isn't always preinstalled in minimal builder images — e.g.
	// `node:20-alpine` ships node + npm but no git. Rather than push that
	// concern onto every contract author, the build prelude detects the
	// package manager and installs git if missing. Costs ~5s on first run
	// per builder image; not worth optimizing past that for v1.
	lines := []string{
		"set -e",
		"set -x",
		// Best-effort git install if missing. Handles alpine (apk),
		// debian/ubuntu (apt-get), and the rare yum/dnf builder. If
		// none match, the subsequent git fetch fails with a clear
		// "git: not found" that surfaces to the caller's build_logs_tail.
		`if ! command -v git >/dev/null 2>&1; then`,
		`  if command -v apk >/dev/null 2>&1; then apk add --no-cache git;`,
		`  elif command -v apt-get >/dev/null 2>&1; then apt-get update -qq && apt-get install -y -qq git;`,
		`  elif command -v dnf >/dev/null 2>&1; then dnf install -y -q git;`,
		`  elif command -v yum >/dev/null 2>&1; then yum install -y -q git;`,
		`  fi`,
		`fi`,
		// Authenticate the clone without leaking the token into `set -x`
		// traces or build logs: GIT_ASKPASS feeds $GIT_TOKEN to git from a
		// subprocess, and REPO_URL carries only the x-access-token username
		// (the token is the password, supplied via askpass). A private repo
		// otherwise fails to clone — the URL has no inline credential.
		`if [ -n "${GIT_TOKEN:-}" ]; then`,
		`  printf '%s\n' '#!/bin/sh' 'exec echo "$GIT_TOKEN"' > /tmp/gitaskpass && chmod +x /tmp/gitaskpass`,
		`  export GIT_ASKPASS=/tmp/gitaskpass GIT_TERMINAL_PROMPT=0`,
		`fi`,
		// Fetch the exact ref by branch/tag name OR commit sha. The
		// restricted-mode hot-swap gate pins git_ref to the verified HEAD sha
		// (anti-TOCTOU); `git clone --branch <sha>` is invalid, but fetch
		// accepts a reachable sha (GitHub allowReachableSHA1InWant) or a ref.
		`mkdir -p /work/repo`,
		`cd /work/repo`,
		`git init -q`,
		`git remote add origin "$REPO_URL"`,
		`git fetch --depth=1 origin "$GIT_REF"`,
		`git checkout -q FETCH_HEAD`,
	}
	if strings.TrimSpace(in.FidelityCommand) != "" {
		lines = append(lines,
			`echo "running hot-swap fidelity classifier for ${GLIMMUNG_HOT_SWAP_VALIDATION_TARGET:-existing_session}"`,
			`sh -c `+shellQuote(strings.TrimSpace(in.FidelityCommand)+` --artifact-kind "$GLIMMUNG_HOT_SWAP_ARTIFACT_KIND" --validation-target "$GLIMMUNG_HOT_SWAP_VALIDATION_TARGET" --enforce`),
		)
	}
	if strings.TrimSpace(in.ArtifactFile) != "" {
		// Backend: the build produces a single executable at ArtifactFile
		// (an absolute path inside the builder image). Surface exactly that
		// one file for the swap container; there is no Source directory.
		lines = append(lines,
			in.BuildCommand,
			`cp "`+in.ArtifactFile+`" /work/artifact`,
			`chmod +x /work/artifact`,
			`ls -la /work/artifact`,
		)
		return strings.Join(lines, "\n")
	}
	lines = append(lines,
		in.BuildCommand,
		`cp -R "/work/repo/`+in.Source+`" /work/source`,
		`ls -la /work/source | head`,
	)
	return strings.Join(lines, "\n")
}

func swapScriptFor(in applyHotSwapJobInputs) string {
	// Swap container resolves target pods at run time (rather than the
	// dispatcher resolving them up front — that was the kubectl-in-
	// glimmung-pod bug from the first cut of this endpoint). Then for
	// each pod tar-streams /work/source into Target and signals PID 1.
	//
	// The artifact is delivered with `tar c -C /work source | tar x
	// --strip-components=1`, not `tar c -C /work/source . | tar xf -`.
	// Reason: session-pod runners run as a non-root user (runAsNonRoot, uid
	// 1000) and Target (e.g. /var/run/<runner>-hot) is an fsGroup emptyDir
	// owned root:1000 with the setgid bit. Archiving `.` (the source dir
	// itself) makes the in-pod tar try to chmod/utime Target — which the
	// non-root user cannot do on the root-owned mount dir, so GNU tar (the
	// Debian-based antigravity runner pod) exits non-zero and aborts the swap
	// before the restart signal. Archiving the `source` directory from its
	// parent and stripping one leading path component drops that directory
	// member entirely, so only the artifact files land in Target and Target's
	// own metadata is never touched. tar restores each file's mode, so the
	// runner binary stays executable. Portable across busybox (alpine
	// claude/codex pods) and GNU (Debian antigravity pod) tar — verified on
	// both.
	lines := []string{
		"set -e",
		"set -x",
		`pods=$(kubectl -n ` + shellQuote(in.TargetNamespace) + ` get pods -l ` + shellQuote(in.TargetPodSelector) +
			` -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')`,
		`if [ -z "$pods" ]; then echo "no pods matched selector ` + shellQuote(in.TargetPodSelector) + ` in namespace ` + shellQuote(in.TargetNamespace) + `"; exit 1; fi`,
		`for pod in $pods; do`,
		`  echo "==> swapping into $pod"`,
	}
	if strings.TrimSpace(in.ArtifactFile) != "" {
		lines = append(lines, backendSwapSteps(in)...)
	} else {
		lines = append(lines, dirSwapSteps(in)...)
	}
	lines = append(lines, `done`, `echo done`)
	return strings.Join(lines, "\n")
}

// dirSwapSteps emits the per-pod swap for static + runner artifacts: extract
// the built source directory into Target, optionally clearing it first, then
// optionally SIGHUP. Static is served live (RestartSignal empty); runners
// SIGHUP to re-exec their supervisor.
func dirSwapSteps(in applyHotSwapJobInputs) []string {
	steps := []string{
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("mkdir -p "+in.Target) + ` < /dev/null`,
	}
	if in.CleanTarget {
		// Static assets are content-hash-named, so a stale prior build would
		// otherwise be served alongside the new one. Clear the override dir's
		// contents before extracting — not the dir itself, which is a mount.
		// `|| true` tolerates an already-empty dir (rm over an empty glob).
		steps = append(steps,
			`  kubectl -n `+shellQuote(in.TargetNamespace)+` exec "$pod" -c `+shellQuote(in.TargetContainer)+
				` -- sh -c `+shellQuote(`rm -rf "`+in.Target+`"/* 2>/dev/null || true`)+` < /dev/null`,
		)
	}
	steps = append(steps,
		`  tar c -C /work source | kubectl -n `+shellQuote(in.TargetNamespace)+` exec -i "$pod" -c `+shellQuote(in.TargetContainer)+
			` -- sh -c `+shellQuote("cd "+in.Target+" && tar x --strip-components=1 -f -"),
	)
	if strings.TrimSpace(in.RestartSignal) != "" {
		steps = append(steps,
			`  kubectl -n `+shellQuote(in.TargetNamespace)+` exec "$pod" -c `+shellQuote(in.TargetContainer)+
				` -- `+restartCommandFor(in.RestartSignal),
		)
	}
	return steps
}

// backendSwapSteps emits the per-pod swap for the backend binary: stream the
// single executable (/work/artifact) to Target.next, chmod +x, atomically
// replace Target, SIGHUP PID 1 so the supervisor re-execs the child, then
// gate on health. The health poll runs inside the pod (busybox wget) so a
// binary that fails to come up makes the swap container exit non-zero — the
// Job fails and the endpoint reports swap_failed instead of persisted.
// Target is a file (e.g. /var/run/<app>-hot/<app>), not a directory, so this
// path never `mkdir`s Target itself — only its parent mount dir.
func backendSwapSteps(in applyHotSwapJobInputs) []string {
	targetParent := in.Target
	if i := strings.LastIndex(in.Target, "/"); i > 0 {
		targetParent = in.Target[:i]
	}
	next := in.Target + ".next"
	steps := []string{
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("mkdir -p "+targetParent) + ` < /dev/null`,
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec -i "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("cat > "+next) + ` < /work/artifact`,
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("chmod +x "+next+" && mv -f "+next+" "+in.Target) + ` < /dev/null`,
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- ` + restartCommandFor(in.RestartSignal),
	}
	if strings.TrimSpace(in.HealthPath) != "" && in.HealthPort > 0 {
		healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", in.HealthPort, in.HealthPath)
		poll := fmt.Sprintf(
			`i=0; while [ $i -lt 30 ]; do if wget -q -O /dev/null %s 2>/dev/null; then echo "health ok after restart (%s)"; exit 0; fi; i=$((i+1)); sleep 2; done; echo "post-restart health check failed: no 2xx from %s within 60s"; exit 1`,
			healthURL, healthURL, healthURL,
		)
		steps = append(steps,
			`  kubectl -n `+shellQuote(in.TargetNamespace)+` exec "$pod" -c `+shellQuote(in.TargetContainer)+
				` -- sh -c `+shellQuote(poll)+` < /dev/null`,
		)
	}
	return steps
}

func restartCommandFor(signal string) string {
	switch strings.ToUpper(strings.TrimSpace(signal)) {
	case "SIGHUP":
		return "sh -c 'kill -HUP 1'"
	default:
		return "sh -c 'kill -HUP 1'"
	}
}

func tailLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func randHex(n int) string {
	buf := make([]byte, n/2+1)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:n]
}

// ─────────────────────────────────────────────────────────────────────────────
// Production k8s client (HTTP API, no kubectl) — mirrors the
// KubernetesRunLauncher.request pattern.
// ─────────────────────────────────────────────────────────────────────────────

type httpK8sJobClient struct {
	Settings   Settings
	HTTPClient *http.Client
}

func newHTTPK8sJobClient(settings Settings) *httpK8sJobClient {
	return &httpK8sJobClient{Settings: settings}
}

func (c *httpK8sJobClient) ApplyJob(ctx context.Context, namespace string, spec map[string]any) error {
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs"
	status, _, err := c.request(ctx, http.MethodPost, path, spec)
	if err != nil && status != http.StatusConflict {
		return err
	}
	return nil
}


func (c *httpK8sJobClient) GetPodLogs(ctx context.Context, namespace, labelSelector, container string) (string, error) {
	// First find the pod name(s) matching the selector.
	listPath := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + httpQueryEscape(labelSelector)
	_, list, err := c.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		return "", err
	}
	items, _ := list["items"].([]any)
	if len(items) == 0 {
		return "", nil
	}
	// Take the first pod (Job typically has one pod since backoffLimit=0).
	pod, _ := items[0].(map[string]any)
	metadata, _ := pod["metadata"].(map[string]any)
	podName, _ := metadata["name"].(string)
	if podName == "" {
		return "", nil
	}
	logsPath := "/api/v1/namespaces/" + namespace + "/pods/" + podName + "/log?container=" + httpQueryEscape(container) + "&tailLines=200"
	body, err := c.requestRaw(ctx, http.MethodGet, logsPath)
	if err != nil {
		return "", err
	}
	return body, nil
}

func (c *httpK8sJobClient) DeleteJob(ctx context.Context, namespace, name string) error {
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name + "?propagationPolicy=Background"
	_, _, err := c.request(ctx, http.MethodDelete, path, nil)
	return err
}

func (c *httpK8sJobClient) request(ctx context.Context, method, path string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Settings.K8sAPIHost, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	token, err := os.ReadFile(c.Settings.K8sSATokenPath)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: c.transport()}
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
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

func (c *httpK8sJobClient) requestRaw(ctx context.Context, method, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Settings.K8sAPIHost, "/")+path, nil)
	if err != nil {
		return "", err
	}
	token, err := os.ReadFile(c.Settings.K8sSATokenPath)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: c.transport()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("kubernetes %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func (c *httpK8sJobClient) transport() http.RoundTripper {
	tr := &http.Transport{}
	if c.Settings.K8sCACertPath != "" {
		caCert, err := os.ReadFile(c.Settings.K8sCACertPath)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caCert) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return tr
}

func httpQueryEscape(s string) string {
	// Minimal URL escaping for label selectors and container names. Both
	// contain alphanumerics + a small set of punctuation that's safe-
	// looking enough; we escape just `=` and ` ` which appear in selectors.
	r := strings.NewReplacer(" ", "%20", "=", "%3D")
	return r.Replace(s)
}

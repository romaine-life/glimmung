package server

import "strings"

const (
	EvidenceGateJobID    = "evidence-verification-gate"
	EvidenceGateStepSlug = "evaluate-verdict"
	PRTouchpointJobID    = "pr-touchpoint"
	PRTouchpointStepSlug = "ensure-pr-touchpoint"
	PRMergeJobID         = "pr-merge"
	PRMergeStepSlug      = "merge-pull-request"

	JobPrimitivePRTouchpoint = "pr_touchpoint"
	JobPrimitivePRMerge      = "pr_merge"

	// EvidenceUploadStepSlug is the slug of the Glimmung-owned step that
	// canonicalization appends to every verification-phase job. Unlike
	// pr_touchpoint/pr_merge (which are managed *jobs* in their own phases
	// doing network work), evidence upload must read the verification pod's
	// LOCAL filesystem — the screenshots the project just collected. Each
	// native k8s Job is its own pod with ephemeral, pod-local storage and
	// there is no shared cross-job/cross-phase volume, so a separate upload
	// job or phase could never see the evidence. The primitive is therefore
	// a managed STEP appended to the verification job itself (same pod as the
	// evidence), not a managed job. See
	// docs/design/evidence-upload-primitive.md.
	EvidenceUploadStepSlug = "upload-evidence"

	// JobPrimitiveEvidenceUpload names the managed evidence-upload primitive.
	// It is recorded on the appended step's slug semantics and asserted at
	// registration; it deliberately does NOT appear in NativeJobSpec.Primitive
	// because this primitive is a step, not a standalone job.
	JobPrimitiveEvidenceUpload = "evidence_upload"
)

const evidenceGateRunScript = `set -Eeuo pipefail
raw="${GLIMMUNG_INPUT_VERIFICATION:-}"
if [ -z "${raw}" ]; then
  echo "verification input is empty" >&2
  exit 2
fi
status="$(printf '%s' "${raw}" | jq -er '.status // empty')" || {
  echo "verification input is not valid JSON or missing status" >&2
  exit 2
}
printf "verification.status = '%s'\n" "${status}"
printf '%s' "${raw}" | jq -r '.reasons[]? | "reason: \(.)"'
if [ "${status}" = "pass" ]; then
  exit 0
fi
exit 1
	`

const prMergeRunScript = `set -Eeuo pipefail
if [ -z "${GLIMMUNG_PR_MERGE_URL:-}" ]; then
  echo "GLIMMUNG_PR_MERGE_URL is not configured" >&2
  exit 2
fi
echo "Merging PR for ${GLIMMUNG_RUN_REF:-unknown run}"
response="$(mktemp)"
status="$(curl -sS -o "${response}" -w '%{http_code}' -X POST "${GLIMMUNG_PR_MERGE_URL}")" || {
  code="$?"
  echo "PR merge request failed with curl exit ${code}" >&2
  exit "${code}"
}
cat "${response}" | jq .
if [ "${status}" -lt 200 ] || [ "${status}" -ge 300 ]; then
  echo "PR merge request returned HTTP ${status}" >&2
  exit 1
fi
result_status="$(jq -r '.status // empty' "${response}")"
case "${result_status}" in
  merged)
    echo "PR merged: $(jq -r '.merge_commit_sha // ""' "${response}")"
    ;;
  already_merged)
    echo "PR was already merged (idempotent success)"
    ;;
  *)
    echo "PR merge returned unexpected status '${result_status}'" >&2
    exit 2
    ;;
esac
pr_number="$(jq -r '.pr_number // empty' "${response}")"
merge_commit="$(jq -r '.merge_commit_sha // empty' "${response}")"
{
  if [ -n "${pr_number}" ]; then printf 'pr_number=%s\n' "${pr_number}"; fi
  if [ -n "${merge_commit}" ]; then printf 'merge_commit_sha=%s\n' "${merge_commit}"; fi
  printf 'merge_status=%s\n' "${result_status}"
} >>"${GLIMMUNG_OUTPUT_FILE}"
`

const prTouchpointRunScript = `set -Eeuo pipefail
if [ -z "${GLIMMUNG_PR_TOUCHPOINT_URL:-}" ]; then
  echo "GLIMMUNG_PR_TOUCHPOINT_URL is not configured" >&2
  exit 2
fi
echo "Ensuring PR touchpoint for ${GLIMMUNG_RUN_REF:-unknown run}"
response="$(mktemp)"
status="$(curl -sS -o "${response}" -w '%{http_code}' -X POST "${GLIMMUNG_PR_TOUCHPOINT_URL}")" || {
  code="$?"
  echo "PR touchpoint request failed with curl exit ${code}" >&2
  exit "${code}"
}
cat "${response}" | jq .
if [ "${status}" -lt 200 ] || [ "${status}" -ge 300 ]; then
  echo "PR touchpoint request returned HTTP ${status}" >&2
  exit 1
fi
result_status="$(jq -r '.status // empty' "${response}")"
if [ "${result_status}" = "skipped" ]; then
  echo "PR touchpoint skipped: $(jq -r '.reason // "no reason"' "${response}")"
  exit 0
fi
if [ "${result_status}" != "ensured" ]; then
  echo "PR touchpoint returned unexpected status '${result_status}'" >&2
  exit 2
fi
pr_number="$(jq -r '.pr_number // empty' "${response}")"
touchpoint_ref="$(jq -r '.touchpoint_ref // empty' "${response}")"
html_url="$(jq -r '.html_url // empty' "${response}")"
{
  if [ -n "${pr_number}" ]; then printf 'pr_number=%s\n' "${pr_number}"; fi
  if [ -n "${touchpoint_ref}" ]; then printf 'touchpoint_ref=%s\n' "${touchpoint_ref}"; fi
  if [ -n "${html_url}" ]; then printf 'pr_url=%s\n' "${html_url}"; fi
} >>"${GLIMMUNG_OUTPUT_FILE}"
echo "PR touchpoint ensured: ${touchpoint_ref:-unknown}"
if [ -n "${html_url}" ]; then
  echo "PR URL: ${html_url}"
fi
`

// evidenceUploadRunScript is the rendered command of the managed
// evidence_upload step. It invokes the native runner's own `upload-evidence`
// subcommand (the pod ENTRYPOINT binary, overridable via
// GLIMMUNG_NATIVE_RUNNER_BIN for tests/dev) in the SAME pod as the freshly
// collected evidence. The subcommand authenticates with the Azure SDK
// credential chain (the projected workload-identity federated token) and
// pushes every file under GLIMMUNG_EVIDENCE_DIR to the shared artifacts blob
// store. No `az` CLI, no `az login` — that is exactly the hand-rolled project
// bash this primitive replaces. An empty/absent GLIMMUNG_EVIDENCE_DIR is a
// success no-op inside the subcommand.
const evidenceUploadRunScript = `set -Eeuo pipefail
exec "${GLIMMUNG_NATIVE_RUNNER_BIN:-/app/glimmung-native-runner}" upload-evidence
`

// managedEvidenceUploadStep is the canonical managed step appended to every
// evidence-producing verification job. It is marked managed via its stable
// slug so re-canonicalization is idempotent (jobHasEvidenceUploadStep).
func managedEvidenceUploadStep() NativeStepSpec {
	title := "Upload collected evidence to artifact storage"
	return NativeStepSpec{
		Slug:  EvidenceUploadStepSlug,
		Title: &title,
		Type:  "run",
		Run:   evidenceUploadRunScript,
		Shell: "bash",
	}
}

// jobHasEvidenceUploadStep reports whether the job already carries the managed
// evidence_upload step, so canonicalization does not double-append on a
// previously-canonicalized job (the read path runs CanonicalWorkflow on every
// load). Identity is the stable managed slug, mirroring how managed jobs avoid
// duplication by their stable id/primitive.
func jobHasEvidenceUploadStep(job NativeJobSpec) bool {
	for _, step := range job.Steps {
		if strings.TrimSpace(step.Slug) == EvidenceUploadStepSlug {
			return true
		}
	}
	return false
}

// phaseProducesEvidence reports whether a phase is a Glimmung verification
// phase, i.e. one whose jobs collect and judge evidence (screenshots /
// required_evidence). Every Glimmung verification phase exists to produce and
// verify evidence — that is its defining purpose — so the verification purpose
// (Verify=true, or purpose=verification by declaration/inference) is the
// phase-local signal canonicalization keys on. This is the conservative choice:
// the alternative of statically scanning each job's steps for an evidence
// emitter would miss the real shapes (spirelens/ambience verification jobs that
// emit screenshots at runtime, and bounded case jobs that declare no steps at
// registration), silently dropping the managed upload exactly where it is
// needed. Non-verification phases are never touched.
func phaseProducesEvidence(phase PhaseSpec) bool {
	return phase.Verify || phasePurpose(phase) == PhasePurposeVerification
}

// appendManagedEvidenceUploadStep appends the managed evidence_upload step to a
// verification-phase job, after the project's own evidence-producing steps, so
// the upload runs last in the same pod once the evidence exists. Idempotent.
func appendManagedEvidenceUploadStep(job NativeJobSpec) NativeJobSpec {
	if jobHasEvidenceUploadStep(job) {
		return job
	}
	job.Steps = append(job.Steps, managedEvidenceUploadStep())
	return job
}

func CanonicalWorkflow(wf Workflow) Workflow {
	for i := range wf.Phases {
		wf.Phases[i] = CanonicalNativePhase(wf.Phases[i])
	}
	return wf
}

// CanonicalNativePhase returns the runtime phase shape Glimmung actually
// launches. Evidence gates are a Glimmung-owned primitive, so any project-
// supplied container details are replaced with the managed gate runner while
// preserving a stable job id when one was already registered.
func CanonicalNativePhase(phase PhaseSpec) PhaseSpec {
	if phase.EvidenceVerificationGate {
		phase.Jobs = []NativeJobSpec{canonicalEvidenceGateJob(phase)}
		return phase
	}
	for i := range phase.Jobs {
		phase.Jobs[i] = CanonicalNativeJob(phase.Jobs[i])
	}
	// Evidence upload is a Glimmung-owned managed STEP (not a job/phase):
	// each verification job's pod holds the evidence on its own ephemeral
	// local disk, so the upload must run inside that same pod. Append the
	// managed step to every evidence-producing verification job, after the
	// project's own evidence-producing steps. Idempotent on re-canonicalization.
	if phaseProducesEvidence(phase) {
		for i := range phase.Jobs {
			phase.Jobs[i] = appendManagedEvidenceUploadStep(phase.Jobs[i])
		}
	}
	return phase
}

func CanonicalNativePhaseJobs(phase PhaseSpec) []NativeJobSpec {
	return CanonicalNativePhase(phase).Jobs
}

func CanonicalNativeJob(job NativeJobSpec) NativeJobSpec {
	switch strings.TrimSpace(job.Primitive) {
	case JobPrimitivePRTouchpoint:
		return canonicalPRTouchpointJob(&job)
	case JobPrimitivePRMerge:
		return canonicalPRMergeJob(&job)
	default:
		return job
	}
}

func canonicalEvidenceGateJob(phase PhaseSpec) NativeJobSpec {
	jobID := EvidenceGateJobID
	name := "Evidence verification gate"
	timeout := 60
	if len(phase.Jobs) > 0 {
		existing := phase.Jobs[0]
		if id := strings.TrimSpace(existing.ID); id != "" {
			jobID = id
		}
		if existing.Name != nil && strings.TrimSpace(*existing.Name) != "" {
			name = strings.TrimSpace(*existing.Name)
		}
		if existing.TimeoutSeconds != nil && *existing.TimeoutSeconds > 0 {
			timeout = *existing.TimeoutSeconds
		}
	}
	title := "Evaluate verification verdict"
	return NativeJobSpec{
		ID:             jobID,
		Name:           &name,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []NativeStepSpec{{
			Slug:  EvidenceGateStepSlug,
			Title: &title,
			Type:  "run",
			Run:   evidenceGateRunScript,
			Shell: "bash",
		}},
	}
}

func canonicalPRMergeJob(existing *NativeJobSpec) NativeJobSpec {
	jobID := PRMergeJobID
	name := "PR merge"
	timeout := 120
	if existing != nil {
		if id := strings.TrimSpace(existing.ID); id != "" {
			jobID = id
		}
		if existing.Name != nil && strings.TrimSpace(*existing.Name) != "" {
			name = strings.TrimSpace(*existing.Name)
		}
		if existing.TimeoutSeconds != nil && *existing.TimeoutSeconds > 0 {
			timeout = *existing.TimeoutSeconds
		}
	}
	title := "Idempotently merge the touchpoint PR"
	return NativeJobSpec{
		ID:             jobID,
		Name:           &name,
		Primitive:      JobPrimitivePRMerge,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []NativeStepSpec{{
			Slug:  PRMergeStepSlug,
			Title: &title,
			Type:  "run",
			Run:   prMergeRunScript,
			Shell: "bash",
		}},
	}
}

func canonicalPRTouchpointJob(existing *NativeJobSpec) NativeJobSpec {
	jobID := PRTouchpointJobID
	name := "PR touchpoint"
	timeout := 120
	if existing != nil {
		if id := strings.TrimSpace(existing.ID); id != "" {
			jobID = id
		}
		if existing.Name != nil && strings.TrimSpace(*existing.Name) != "" {
			name = strings.TrimSpace(*existing.Name)
		}
		if existing.TimeoutSeconds != nil && *existing.TimeoutSeconds > 0 {
			timeout = *existing.TimeoutSeconds
		}
	}
	title := "Ensure PR touchpoint"
	return NativeJobSpec{
		ID:             jobID,
		Name:           &name,
		Primitive:      JobPrimitivePRTouchpoint,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []NativeStepSpec{{
			Slug:  PRTouchpointStepSlug,
			Title: &title,
			Type:  "run",
			Run:   prTouchpointRunScript,
			Shell: "bash",
		}},
	}
}

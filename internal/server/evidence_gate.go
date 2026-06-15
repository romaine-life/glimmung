package server

import "strings"

const (
	EvidenceGateJobID    = "evidence-verification-gate"
	EvidenceGateStepSlug = "evaluate-verdict"
	PRReviewJobID    = "pr-review"
	PRReviewStepSlug = "ensure-pr-review"
	PRMergeJobID         = "pr-merge"
	PRMergeStepSlug      = "merge-pull-request"

	JobPrimitivePRReview = "pr_review"
	JobPrimitivePRMerge      = "pr_merge"
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

const prReviewRunScript = `set -Eeuo pipefail
if [ -z "${GLIMMUNG_PR_REVIEW_URL:-}" ]; then
  echo "GLIMMUNG_PR_REVIEW_URL is not configured" >&2
  exit 2
fi
echo "Ensuring PR review for ${GLIMMUNG_RUN_REF:-unknown run}"
response="$(mktemp)"
status="$(curl -sS -o "${response}" -w '%{http_code}' -X POST "${GLIMMUNG_PR_REVIEW_URL}")" || {
  code="$?"
  echo "PR review request failed with curl exit ${code}" >&2
  exit "${code}"
}
cat "${response}" | jq .
if [ "${status}" -lt 200 ] || [ "${status}" -ge 300 ]; then
  echo "PR review request returned HTTP ${status}" >&2
  exit 1
fi
result_status="$(jq -r '.status // empty' "${response}")"
if [ "${result_status}" = "skipped" ]; then
  echo "PR review skipped: $(jq -r '.reason // "no reason"' "${response}")"
  exit 0
fi
if [ "${result_status}" != "ensured" ]; then
  echo "PR review returned unexpected status '${result_status}'" >&2
  exit 2
fi
pr_number="$(jq -r '.pr_number // empty' "${response}")"
review_ref="$(jq -r '.review_ref // empty' "${response}")"
html_url="$(jq -r '.html_url // empty' "${response}")"
{
  if [ -n "${pr_number}" ]; then printf 'pr_number=%s\n' "${pr_number}"; fi
  if [ -n "${review_ref}" ]; then printf 'review_ref=%s\n' "${review_ref}"; fi
  if [ -n "${html_url}" ]; then printf 'pr_url=%s\n' "${html_url}"; fi
} >>"${GLIMMUNG_OUTPUT_FILE}"
echo "PR review ensured: ${review_ref:-unknown}"
if [ -n "${html_url}" ]; then
  echo "PR URL: ${html_url}"
fi
`

func CanonicalWorkflow(wf Workflow) Workflow {
	for i := range wf.Phases {
		wf.Phases[i] = CanonicalRunnerPhase(wf.Phases[i])
	}
	return wf
}

// CanonicalRunnerPhase returns the runtime phase shape Glimmung actually
// launches. Evidence gates are a Glimmung-owned primitive, so any project-
// supplied container details are replaced with the managed gate runner while
// preserving a stable job id when one was already registered.
func CanonicalRunnerPhase(phase PhaseSpec) PhaseSpec {
	if phase.EvidenceVerificationGate {
		phase.Jobs = []RunnerJobSpec{canonicalEvidenceGateJob(phase)}
		return phase
	}
	for i := range phase.Jobs {
		phase.Jobs[i] = CanonicalRunnerJob(phase.Jobs[i])
	}
	return phase
}

func CanonicalRunnerPhaseJobs(phase PhaseSpec) []RunnerJobSpec {
	return CanonicalRunnerPhase(phase).Jobs
}

func CanonicalRunnerJob(job RunnerJobSpec) RunnerJobSpec {
	switch strings.TrimSpace(job.Primitive) {
	case JobPrimitivePRReview:
		return canonicalPRReviewJob(&job)
	case JobPrimitivePRMerge:
		return canonicalPRMergeJob(&job)
	default:
		return job
	}
}

func canonicalEvidenceGateJob(phase PhaseSpec) RunnerJobSpec {
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
	return RunnerJobSpec{
		ID:             jobID,
		Name:           &name,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []RunnerStepSpec{{
			Slug:  EvidenceGateStepSlug,
			Title: &title,
			Type:  "run",
			Run:   evidenceGateRunScript,
			Shell: "bash",
		}},
	}
}

func canonicalPRMergeJob(existing *RunnerJobSpec) RunnerJobSpec {
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
	title := "Idempotently merge the review PR"
	return RunnerJobSpec{
		ID:             jobID,
		Name:           &name,
		Primitive:      JobPrimitivePRMerge,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []RunnerStepSpec{{
			Slug:  PRMergeStepSlug,
			Title: &title,
			Type:  "run",
			Run:   prMergeRunScript,
			Shell: "bash",
		}},
	}
}

func canonicalPRReviewJob(existing *RunnerJobSpec) RunnerJobSpec {
	jobID := PRReviewJobID
	name := "PR review"
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
	title := "Ensure PR review"
	return RunnerJobSpec{
		ID:             jobID,
		Name:           &name,
		Primitive:      JobPrimitivePRReview,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []RunnerStepSpec{{
			Slug:  PRReviewStepSlug,
			Title: &title,
			Type:  "run",
			Run:   prReviewRunScript,
			Shell: "bash",
		}},
	}
}

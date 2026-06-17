package server

import "strings"

const (
	EvidenceGateJobID    = "evidence-verification-gate"
	EvidenceGateStepSlug = "evaluate-verdict"
	PRReviewJobID        = "pr-review"
	PRReviewStepSlug     = "ensure-pr-review"
	PRMergeJobID         = "pr-merge"
	PRMergeStepSlug      = "merge-pull-request"
	VerificationStepSlug = "finalize-verification"

	JobPrimitivePRReview              = "pr_review"
	JobPrimitivePRMerge               = "pr_merge"
	StepPrimitiveVerificationFinalize = "verification_finalize"
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

const verificationFinalizeRunScript = `set -Eeuo pipefail
: "${GLIMMUNG_COMPLETION_FILE:?missing GLIMMUNG_COMPLETION_FILE}"
: "${GLIMMUNG_PROJECT:?missing GLIMMUNG_PROJECT}"
: "${GLIMMUNG_RUN_ID:?missing GLIMMUNG_RUN_ID}"
: "${ARTIFACTS_STORAGE_ACCOUNT:?missing ARTIFACTS_STORAGE_ACCOUNT}"
: "${ARTIFACTS_CONTAINER:?missing ARTIFACTS_CONTAINER}"

working_dir="${GLIMMUNG_WORKING_DIR:-/tmp/glimmung-${GLIMMUNG_RUN_REF:-localdev}}"
artifacts_dir="${GLIMMUNG_ARTIFACTS_DIR:-${working_dir}/artifacts}"
verification_file="${GLIMMUNG_VERIFICATION_FILE:-${artifacts_dir}/verification.json}"
summary_file="${GLIMMUNG_VERIFICATION_MARKDOWN_FILE:-${artifacts_dir}/verification.md}"
screenshots_dir="${GLIMMUNG_SCREENSHOTS_DIR:-${artifacts_dir}/screenshots}"
videos_dir="${GLIMMUNG_VIDEOS_DIR:-${artifacts_dir}/videos}"
evidence_dir="${GLIMMUNG_EVIDENCE_DIR:-${artifacts_dir}/evidence}"

if [ ! -s "${verification_file}" ]; then
  echo "verification finalizer: ${verification_file} is missing or empty" >&2
  exit 2
fi
if ! jq -e 'type == "object"' "${verification_file}" >/dev/null; then
  echo "verification finalizer: ${verification_file} is not a JSON object" >&2
  exit 2
fi

raw_status="$(jq -r '.status // ""' "${verification_file}")"
abort_reason="$(jq -r '.abort_reason // ""' "${verification_file}")"
case "${raw_status}" in
  pass)
    verification_status="pass"
    ;;
  fail|error)
    verification_status="${raw_status}"
    ;;
  abort)
    if [ -z "${abort_reason}" ]; then
      echo "verification finalizer: status=abort requires abort_reason" >&2
      exit 2
    fi
    verification_status="fail"
    ;;
  *)
    echo "verification finalizer: invalid verification status '${raw_status}' (want pass, fail, error, or abort)" >&2
    exit 2
    ;;
esac

has_files() {
  [ -d "$1" ] && find "$1" -type f -print -quit | grep -q .
}

finalizer_reason=""
claimed_screenshot_pass="$(jq -r 'any(.evidence_results[]?; (((.kind // "") | ascii_downcase) == "screenshot") and (.passed == true))' "${verification_file}")"
if [ "${verification_status}" = "pass" ] && [ "${claimed_screenshot_pass}" = "true" ] && ! has_files "${screenshots_dir}"; then
  verification_status="fail"
  finalizer_reason="verification claimed passed screenshot evidence, but no screenshot files were present under ${screenshots_dir}"
fi

refs_file="$(mktemp)"
enriched_file="$(mktemp)"
trap 'rm -f "${refs_file}" "${enriched_file}"' EXIT
: >"${refs_file}"

needs_upload=0
for dir in "${screenshots_dir}" "${videos_dir}" "${evidence_dir}"; do
  if has_files "${dir}"; then
    needs_upload=1
  fi
done

if [ "${needs_upload}" = "1" ]; then
  : "${AZURE_CLIENT_ID:?missing AZURE_CLIENT_ID}"
  : "${AZURE_TENANT_ID:?missing AZURE_TENANT_ID}"
  : "${AZURE_FEDERATED_TOKEN_FILE:?missing AZURE_FEDERATED_TOKEN_FILE}"
  az login --service-principal \
    --username "${AZURE_CLIENT_ID}" \
    --tenant "${AZURE_TENANT_ID}" \
    --federated-token "$(cat "${AZURE_FEDERATED_TOKEN_FILE}")" \
    --allow-no-subscriptions >/dev/null
fi

upload_tree() {
  local kind="$1"
  local source="$2"
  if ! has_files "${source}"; then
    return 0
  fi
  local prefix="runs/${GLIMMUNG_PROJECT}/${GLIMMUNG_RUN_ID}/${kind}"
  az storage blob upload-batch \
    --account-name "${ARTIFACTS_STORAGE_ACCOUNT}" \
    --destination "${ARTIFACTS_CONTAINER}" \
    --destination-path "${prefix}" \
    --source "${source}" \
    --auth-mode login \
    --overwrite true >/dev/null
  find "${source}" -type f -print | sort | while IFS= read -r file; do
    rel="${file#"${source}"/}"
    printf '%s/%s\n' "${prefix}" "${rel}"
  done >>"${refs_file}"
}

upload_tree screenshots "${screenshots_dir}"
upload_tree videos "${videos_dir}"
upload_tree evidence "${evidence_dir}"

jq -nR '[inputs | select(length > 0)] | unique' "${refs_file}" >"${refs_file}.json"
mv "${refs_file}.json" "${refs_file}"

jq \
  --arg status "${verification_status}" \
  --arg raw_status "${raw_status}" \
  --arg abort_reason "${abort_reason}" \
  --arg finalizer_reason "${finalizer_reason}" \
  --slurpfile refs "${refs_file}" '
  def strings_only:
    map(select(type == "string" and length > 0));
  def array_or_empty($value):
    if ($value | type) == "array" then $value else [] end;
  def kind_for($ref):
    if ($ref | test("\\.(png|jpg|jpeg|webp|gif)$"; "i")) then "screenshot"
    elif ($ref | test("\\.(webm|mp4|mov|m4v)$"; "i")) then "video"
    else "artifact"
    end;
  .status = $status
  | .reasons = (
      (array_or_empty(.reasons) + [$finalizer_reason] + (if $raw_status == "abort" then ["verifier reported status=abort reason=" + $abort_reason] else [] end))
      | strings_only
      | unique
    )
  | .evidence_refs = ((array_or_empty(.evidence_refs) + $refs[0]) | strings_only | unique)
  | .evidence = (
      (array_or_empty(.evidence) + ($refs[0] | map({kind: kind_for(.), ref: .})))
      | unique_by((.kind // "") + "\u0000" + (.ref // ""))
    )
' "${verification_file}" >"${enriched_file}"

summary_markdown=""
if [ -s "${summary_file}" ]; then
  summary_markdown="$(cat "${summary_file}")"
else
  summary_markdown="$(jq -r '.notes // ""' "${verification_file}")"
fi
screenshots_markdown="$(jq -r '.[] | select(test("/screenshots/")) | "- [" + (split("/")[-1]) + "](blob://artifacts/" + . + ")"' "${refs_file}")"

jq -n \
  --slurpfile verification "${enriched_file}" \
  --arg summary_markdown "${summary_markdown}" \
  --arg screenshots_markdown "${screenshots_markdown}" '
  {verification: $verification[0]}
  + (if ($summary_markdown | length) > 0 then {summary_markdown: $summary_markdown} else {} end)
  + (if ($screenshots_markdown | length) > 0 then {screenshots_markdown: $screenshots_markdown} else {} end)
' >"${GLIMMUNG_COMPLETION_FILE}"

printf 'verification finalizer: status=%s evidence_refs=%s\n' \
  "${verification_status}" \
  "$(jq -r 'join(",")' "${refs_file}")"
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
		for i := range job.Steps {
			job.Steps[i] = CanonicalRunnerStep(job.Steps[i])
		}
		return job
	}
}

func CanonicalRunnerStep(step RunnerStepSpec) RunnerStepSpec {
	switch strings.TrimSpace(step.Primitive) {
	case StepPrimitiveVerificationFinalize:
		return canonicalVerificationFinalizeStep(&step)
	default:
		return step
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

func canonicalVerificationFinalizeStep(existing *RunnerStepSpec) RunnerStepSpec {
	slug := VerificationStepSlug
	title := "Finalize verification evidence"
	env := map[string]string(nil)
	if existing != nil {
		if value := strings.TrimSpace(existing.Slug); value != "" {
			slug = value
		}
		if existing.Title != nil && strings.TrimSpace(*existing.Title) != "" {
			title = strings.TrimSpace(*existing.Title)
		}
		if len(existing.Env) > 0 {
			env = existing.Env
		}
	}
	return RunnerStepSpec{
		Slug:      slug,
		Title:     &title,
		Primitive: StepPrimitiveVerificationFinalize,
		Type:      "run",
		Run:       verificationFinalizeRunScript,
		Shell:     "bash",
		Env:       env,
	}
}

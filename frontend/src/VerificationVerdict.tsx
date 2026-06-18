/**
 * VerificationVerdict renders the structured "why" of a verification verdict:
 * the expected-vs-observed failure block, the verifier's suspected cause, and
 * any reason lines. It exists so a failed verify job reads as a diagnosis
 * instead of a bare `status=fail` exit code in the runner event stream.
 *
 * The verdict rides the job_completion / attempt verification record (it is
 * job-level, not step-level), so the same panel applies to any step selected
 * within a verify job — including the terse `finalize-*` gate step whose only
 * runner events are `emitted verdict status=fail` / `step_failed`.
 *
 * Contract: docs/features/observability-and-evidence/contract.md — "a feature
 * is incomplete if its bad states can only be understood by rerunning work,
 * reading browser devtools, or guessing". Surfacing this block is how the
 * dashboard stops sending operators to guess at a verification failure.
 */

// VerificationFailure mirrors the server `failure` block emitted by producers
// on a non-pass verdict (internal/server/completion_api.go VerificationFailure).
// SuspectedCause is the verifier's closed-enum classification: code_bug |
// test_expectation_mismatch | environment_config | harness_flake.
export type VerificationFailure = {
  expected?: string | null;
  observed?: string | null;
  where?: string | null;
  suspected_cause?: string | null;
  cause_detail?: string | null;
};

export type VerificationVerdictData = {
  status?: string | null; // "pass" | "fail" | "error"
  failure?: VerificationFailure | null;
  reasons?: string[];
};

function trimmed(value: string | null | undefined): string | null {
  if (typeof value !== "string") return null;
  const out = value.trim();
  return out.length > 0 ? out : null;
}

function failurePopulated(failure: VerificationFailure | null | undefined): boolean {
  if (!failure) return false;
  return Boolean(
    trimmed(failure.expected) ||
      trimmed(failure.observed) ||
      trimmed(failure.where) ||
      trimmed(failure.suspected_cause) ||
      trimmed(failure.cause_detail),
  );
}

// hasVerdict decides whether there is anything worth rendering. Non-verify
// jobs (env-prep, llm-implement) carry no verification status or failure, so
// the panel stays absent for them rather than adding empty chrome.
export function hasVerdict(verdict: VerificationVerdictData | null | undefined): boolean {
  if (!verdict) return false;
  return Boolean(
    trimmed(verdict.status) ||
      failurePopulated(verdict.failure) ||
      (verdict.reasons ?? []).some((r) => trimmed(r)),
  );
}

// effectiveStatus resolves the pill. A populated failure block with no
// explicit status still reads as a failure, so it does not show as neutral.
function effectiveStatus(verdict: VerificationVerdictData): { cls: string; text: string; failed: boolean } {
  const status = trimmed(verdict.status);
  switch (status) {
    case "pass":
      return { cls: "free", text: "pass", failed: false };
    case "fail":
      return { cls: "drain", text: "fail", failed: true };
    case "error":
      return { cls: "drain", text: "error", failed: true };
    default:
      if (failurePopulated(verdict.failure)) {
        return { cls: "drain", text: status ?? "fail", failed: true };
      }
      return { cls: "info", text: status ?? "verdict", failed: false };
  }
}

export function VerificationVerdict({ verdict }: { verdict: VerificationVerdictData }) {
  if (!hasVerdict(verdict)) return null;

  const status = effectiveStatus(verdict);
  const failure = verdict.failure ?? null;
  const expected = trimmed(failure?.expected);
  const observed = trimmed(failure?.observed);
  const where = trimmed(failure?.where);
  const cause = trimmed(failure?.suspected_cause);
  const causeDetail = trimmed(failure?.cause_detail);
  const reasons = (verdict.reasons ?? []).map(trimmed).filter((r): r is string => r !== null);
  const hasFailureBlock = Boolean(expected || observed || cause);

  return (
    <div className={`run-verdict${status.failed ? "" : " pass"}`} role={status.failed ? "alert" : undefined}>
      <div className="run-verdict-head">
        <span className="key">verdict</span>
        <span className={`pill ${status.cls}`}>{status.text}</span>
      </div>
      {hasFailureBlock && (
        <div className="run-verdict-failure">
          {expected && (
            <div>
              <span className="key">expected</span> <span className="mono">{expected}</span>
            </div>
          )}
          {observed && (
            <div>
              <span className="key">observed</span>{" "}
              <span className="mono">
                {observed}
                {where ? ` [${where}]` : ""}
              </span>
            </div>
          )}
          {cause && (
            <div>
              <span className="key">suspected cause</span> <span className="mono">{cause}</span>
              {causeDetail ? <span className="dim"> — {causeDetail}</span> : null}
            </div>
          )}
        </div>
      )}
      {reasons.length > 0 && (
        <ul className="run-verdict-reasons">
          {reasons.map((reason, index) => (
            <li key={index} className="mono dim">
              {reason}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

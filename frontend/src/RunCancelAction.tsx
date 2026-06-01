export type AbortState =
  | { kind: "idle" }
  | { kind: "armed" }
  | { kind: "aborting" }
  | { kind: "error"; message: string };

export const RUN_CANCEL_IDLE_STATE: AbortState = { kind: "idle" };

export function runStateCanCancel(state: string | null | undefined): boolean {
  return state === "in_progress" || state === "queued" || state === "pending";
}

export function runCancelDisabledReason(state: string | null | undefined): string {
  switch (state) {
    case "aborted":
      return "run already aborted";
    case "cancelled":
    case "canceled":
      return "run already cancelled";
    case "passed":
      return "run already passed";
    case "review_required":
    case "needs_review":
      return "run is waiting for review";
    case "failed":
      return "run already failed";
    case "recycled":
      return "run already recycled";
    case "":
    case null:
    case undefined:
      return "run state unavailable";
    default:
      return `run is not cancellable from state ${state}`;
  }
}

export function RunCancelAction({
  signedIn,
  isAdmin,
  runState,
  state,
  onArm,
  onKeep,
  onConfirm,
  className,
}: {
  signedIn: boolean;
  isAdmin: boolean;
  runState: string | null | undefined;
  state: AbortState;
  onArm: () => void;
  onKeep: () => void;
  onConfirm: () => void;
  className?: string;
}) {
  if (!signedIn || !isAdmin) return null;

  const canCancel = runStateCanCancel(runState);
  const disabledReason = canCancel ? undefined : runCancelDisabledReason(runState);

  if (canCancel && (state.kind === "armed" || state.kind === "aborting")) {
    return (
      <span className={className}>
        <span className="confirm">
          <button
            type="button"
            className="link danger-text"
            onClick={onConfirm}
            disabled={state.kind === "aborting"}
          >
            {state.kind === "aborting" ? "cancelling…" : "cancel?"}
          </button>
          <span className="sep">/</span>
          <button
            type="button"
            className="link"
            onClick={onKeep}
            disabled={state.kind === "aborting"}
          >
            keep
          </button>
        </span>
      </span>
    );
  }

  return (
    <span className={className}>
      <button
        type="button"
        className="link danger-text"
        onClick={onArm}
        disabled={!canCancel}
        title={disabledReason}
      >
        cancel run
      </button>
      {state.kind === "error" && (
        <span className="dispatch-error-message" role="alert" title={state.message}>
          cancel error
        </span>
      )}
    </span>
  );
}

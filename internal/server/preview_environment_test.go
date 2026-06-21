package server

import (
	"testing"
	"time"
)

func TestPreviewEnvironmentObservedNotClaimedLifecycle(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	// Provisioned, enabled, no push yet.
	env := PreviewEnvironment{Project: "glimmung", Name: "p1", Enabled: true}.MarkReady()
	if env.State != PreviewStateReady {
		t.Fatalf("state = %q, want ready", env.State)
	}
	if env.PendingObservation() {
		t.Fatalf("no push yet: should not be pending observation")
	}

	// A push RECEIPT is a claim, not yet live. State -> pushed; LiveBuildID set;
	// ObservedBuildID still empty.
	env = env.RecordPushReceipt("build-A", now)
	if env.State != PreviewStatePushed {
		t.Fatalf("after receipt state = %q, want pushed", env.State)
	}
	if env.LiveBuildID != "build-A" || env.PushedAt == nil {
		t.Fatalf("receipt did not record claim: %+v", env)
	}
	if !env.PendingObservation() {
		t.Fatalf("pushed-but-unobserved must be pending observation")
	}

	// Observed read-back confirms the edge serves exactly build-A -> live.
	env = env.RecordObserved(true, "build-A", now.Add(time.Second))
	if env.State != PreviewStateLive {
		t.Fatalf("confirmed state = %q, want live", env.State)
	}
	if env.ObservedBuildID != "build-A" || env.ObservedAt == nil {
		t.Fatalf("observed not recorded: %+v", env)
	}
	if env.PendingObservation() {
		t.Fatalf("confirmed-live must NOT be pending observation")
	}

	// Push build-B; edge still serving A -> STALE (pushed != observed).
	env = env.RecordPushReceipt("build-B", now.Add(2*time.Second))
	if !env.PendingObservation() {
		t.Fatalf("new push must be pending observation")
	}
	env = env.RecordObserved(true, "build-A", now.Add(3*time.Second))
	if env.State != PreviewStateStale {
		t.Fatalf("mismatch state = %q, want stale", env.State)
	}
	if env.ObservedBuildID != "build-A" {
		t.Fatalf("stale observed build = %q, want build-A (what the edge actually serves)", env.ObservedBuildID)
	}
	if env.Detail == "" {
		t.Fatalf("stale must carry a diagnostic detail")
	}

	// Edge catches up to build-B -> live again, detail cleared.
	env = env.RecordObserved(true, "build-B", now.Add(4*time.Second))
	if env.State != PreviewStateLive || env.Detail != "" {
		t.Fatalf("catch-up state = %q detail = %q, want live/empty", env.State, env.Detail)
	}
}

func TestPreviewEnvironmentStaleWhenOverrideInactive(t *testing.T) {
	now := time.Now().UTC()
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true}.
		MarkReady().
		RecordPushReceipt("build-X", now)
	// Edge reports override inactive (DELETE happened / never flipped) while a
	// build was pushed -> stale, observed build empty.
	env = env.RecordObserved(false, "", now.Add(time.Second))
	if env.State != PreviewStateStale {
		t.Fatalf("inactive override with pending push = %q, want stale", env.State)
	}
	if env.ObservedBuildID != "" {
		t.Fatalf("inactive override observed build should be empty, got %q", env.ObservedBuildID)
	}
}

func TestPreviewEnvironmentReadBackWithNoPushStaysReady(t *testing.T) {
	now := time.Now().UTC()
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true}.MarkProvisioning()
	// A read-back before any push (fresh passthrough) keeps the env ready.
	env = env.RecordObserved(false, "", now)
	if env.State != PreviewStateReady {
		t.Fatalf("no-push read-back state = %q, want ready", env.State)
	}
}

func TestPreviewEnvironmentSetEnabledToggle(t *testing.T) {
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true, State: PreviewStateReady}
	env = env.SetEnabled(false)
	if env.Enabled || env.State != PreviewStateDisabled {
		t.Fatalf("disable: enabled=%v state=%q", env.Enabled, env.State)
	}
	// Re-enable with no prior push -> ready.
	env = env.SetEnabled(true)
	if !env.Enabled || env.State != PreviewStateReady {
		t.Fatalf("re-enable (no push): state=%q", env.State)
	}
	// Re-enable with a prior push -> pushed (pending re-observe).
	env = env.RecordPushReceipt("b", time.Now()).SetEnabled(false).SetEnabled(true)
	if env.State != PreviewStatePushed {
		t.Fatalf("re-enable (with push): state=%q, want pushed", env.State)
	}
}

func TestPreviewEnvironmentMarkError(t *testing.T) {
	env := PreviewEnvironment{Project: "p", Name: "n"}.MarkProvisioning().MarkError("boom")
	if env.State != PreviewStateError || env.Detail != "boom" {
		t.Fatalf("mark error: state=%q detail=%q", env.State, env.Detail)
	}
}

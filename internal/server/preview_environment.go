package server

import (
	"strings"
	"time"
)

// LeaseKindPreview is the durable lease type for the live-preview lane. A
// preview lease is a SEPARATE type from a validation/runner lease: it is never
// acquired through the runner-slot checkout path, never carries
// metadata.runner_k8s, and never reserves a runner slot — so it is structurally
// not a validation target. The faithful image-deploy lane is untouched. A
// preview env is served by a SINGLE pod (the chart's live-preview-edge.replicas
// helper fails the render on replicas>1, v1 #1419).
const LeaseKindPreview = "preview"

// Preview-lane lifecycle states. The preview_environment row is the durable
// SOURCE OF TRUTH for the live-preview lane: state is never inferred from
// process memory or a session's optimistic claim. A push is only ever marked
// live (PreviewStateLive) once the observed read-back verifier confirms the edge
// serves exactly the pushed build; until then it is PreviewStatePushed, and a
// confirmed mismatch is the durable, surfaced, counted PreviewStateStale.
const (
	// PreviewStateProvisioning: the provision op (deploy the stable backend +
	// the edge in front) is in flight.
	PreviewStateProvisioning = "provisioning"
	// PreviewStateReady: provisioned and serving the stable backend; no override
	// pushed yet (the edge fresh-passthroughs to the backend).
	PreviewStateReady = "ready"
	// PreviewStatePushed: a session recorded a push receipt for LiveBuildID but
	// the verifier has not yet read it back — a claim, not yet observed live.
	PreviewStatePushed = "pushed"
	// PreviewStateLive: the verifier read the edge back serving exactly
	// LiveBuildID (override_active && observed build == pushed build).
	PreviewStateLive = "live"
	// PreviewStateStale: a build was pushed but the edge is NOT serving it
	// (observed build != pushed build, or the override is inactive). Distinct,
	// durable, surfaced, and counted — the "pushed but not live" trust gap.
	PreviewStateStale = "stale"
	// PreviewStateDisabled: the preview was disabled by the owner/operator.
	PreviewStateDisabled = "disabled"
	// PreviewStateError: provisioning or a control op failed; Detail explains.
	PreviewStateError = "error"
)

// PreviewEnvironment is the durable, Glimmung-owned record for one app's
// live-preview environment — the source of truth for the preview lane. The
// validation slot lane (faithful image-deploy) has its own slots/leases rows
// and is never represented here.
//
// "Observed, not claimed": LiveBuildID/PushedAt record what a session SAID it
// pushed; ObservedBuildID/ObservedAt record what the edge was actually read
// back serving. State is derived from the relationship between them, never from
// the receipt alone.
type PreviewEnvironment struct {
	// Project is the app this preview belongs to.
	Project string `json:"project"`
	// Name is the preview env name — the helm release / slot name / DNS label.
	Name string `json:"name"`
	// LeaseRef is the preview lease (Kind=preview) that owns this env.
	LeaseRef string `json:"lease_ref"`
	// SessionID is the owning Tank session (best-effort attribution).
	SessionID string `json:"session_id"`
	// AuthorizedSubject is the IdP-signed JWT `sub` permitted to push to the
	// edge — the preview owner. The edge matches it exactly; Glimmung records it
	// so a later push receipt / re-provision uses the same owner.
	AuthorizedSubject string `json:"authorized_subject"`
	// Enabled is the owner/operator toggle for the preview lane.
	Enabled bool `json:"enabled"`
	// State is one of the PreviewState* constants.
	State string `json:"state"`
	// URL is the public preview wildcard URL (edge → backend).
	URL string `json:"url"`
	// UpstreamURL is the in-pod app backend base URL the edge proxies to.
	UpstreamURL string `json:"upstream_url"`
	// BackendPrefixes are the edge's reverse-proxy prefixes (from the app's
	// live_preview metadata).
	BackendPrefixes []string `json:"backend_prefixes"`
	// ImageTag is the stable backend image (main's fingerprinted CI image).
	ImageTag string `json:"image_tag"`
	// EdgeImage is the live-preview-edge image ref fronting the backend.
	EdgeImage string `json:"edge_image"`
	// LiveBuildID is the build id of the last push receipt — CLAIMED, not yet
	// confirmed.
	LiveBuildID string `json:"live_build_id"`
	// PushedAt is when the last push receipt was recorded.
	PushedAt *time.Time `json:"pushed_at"`
	// ObservedBuildID is the build the edge was last read back serving —
	// OBSERVED. It is the truth the dashboard reflects.
	ObservedBuildID string `json:"observed_build_id"`
	// ObservedAt is when the last read-back happened.
	ObservedAt *time.Time `json:"observed_at"`
	// Detail carries the latest error/diagnostic (empty on success).
	Detail string `json:"detail"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// etag is the store-provided CAS token (the row's updated_at). Empty when
	// the env came from a list query that doesn't expose per-row etags.
	etag string `json:"-"`
}

// ETag returns the captured CAS token for conditional updates.
func (e PreviewEnvironment) ETag() string { return e.etag }

// WithETag returns a copy of e with tag as its captured etag. Store
// implementations attach the etag at read time; tests use it to build synthetic
// etags.
func (e PreviewEnvironment) WithETag(tag string) PreviewEnvironment { e.etag = tag; return e }

// PreviewEnvDocID is the durable doc id for a preview env row.
func PreviewEnvDocID(project, name string) string {
	return strings.TrimSpace(project) + ":" + strings.TrimSpace(name)
}

// PendingObservation reports whether a push has been recorded that the verifier
// has not yet confirmed live — i.e. there is a build to read back. The verifier
// reconciler uses this to bound which envs it polls (cost story: only envs with
// an unconfirmed push are read back).
func (e PreviewEnvironment) PendingObservation() bool {
	if strings.TrimSpace(e.LiveBuildID) == "" {
		return false
	}
	return e.ObservedBuildID != e.LiveBuildID
}

// RecordPushReceipt records a session's claim that it pushed `build` to the
// edge. It does NOT mark the env live — only the observed read-back does that.
// The env moves to PreviewStatePushed so the verifier knows to read it back.
func (e PreviewEnvironment) RecordPushReceipt(build string, now time.Time) PreviewEnvironment {
	e.LiveBuildID = strings.TrimSpace(build)
	at := now.UTC()
	e.PushedAt = &at
	e.Detail = ""
	if e.Enabled {
		e.State = PreviewStatePushed
	}
	return e
}

// RecordObserved folds an edge read-back into durable state. overrideActive and
// observedBuild come from GET /__live-preview/status. The push is marked live
// ONLY when the edge serves exactly the pushed build; a pushed build the edge
// is not serving becomes the durable stale state.
func (e PreviewEnvironment) RecordObserved(overrideActive bool, observedBuild string, now time.Time) PreviewEnvironment {
	observedBuild = strings.TrimSpace(observedBuild)
	at := now.UTC()
	e.ObservedAt = &at
	if overrideActive {
		e.ObservedBuildID = observedBuild
	} else {
		e.ObservedBuildID = ""
	}
	switch {
	case strings.TrimSpace(e.LiveBuildID) == "":
		// No push recorded yet: the edge is fresh-passthrough to the backend.
		if e.Enabled {
			e.State = PreviewStateReady
		}
		e.Detail = ""
	case overrideActive && observedBuild == e.LiveBuildID:
		e.State = PreviewStateLive
		e.Detail = ""
	default:
		// A build was pushed but the edge is not serving it.
		e.State = PreviewStateStale
		if overrideActive {
			e.Detail = "edge serving build " + observedBuild + ", expected " + e.LiveBuildID
		} else {
			e.Detail = "edge override inactive, expected build " + e.LiveBuildID
		}
	}
	return e
}

// MarkProvisioning moves the env into the in-flight provision state.
func (e PreviewEnvironment) MarkProvisioning() PreviewEnvironment {
	e.State = PreviewStateProvisioning
	e.Detail = ""
	return e
}

// MarkReady marks the env provisioned and serving the stable backend (no
// override yet). Resets any stale push state on a fresh provision.
func (e PreviewEnvironment) MarkReady() PreviewEnvironment {
	e.State = PreviewStateReady
	e.Detail = ""
	return e
}

// MarkError records a provision/control failure.
func (e PreviewEnvironment) MarkError(detail string) PreviewEnvironment {
	e.State = PreviewStateError
	e.Detail = strings.TrimSpace(detail)
	return e
}

// SetEnabled flips the owner/operator toggle. Disabling moves to disabled;
// re-enabling returns to the state implied by the observed/pushed relationship.
func (e PreviewEnvironment) SetEnabled(enabled bool) PreviewEnvironment {
	e.Enabled = enabled
	if !enabled {
		e.State = PreviewStateDisabled
		return e
	}
	switch {
	case e.State == PreviewStateDisabled && strings.TrimSpace(e.LiveBuildID) == "":
		e.State = PreviewStateReady
	case e.State == PreviewStateDisabled:
		e.State = PreviewStatePushed
	}
	return e
}

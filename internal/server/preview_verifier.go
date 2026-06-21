package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// previewStatusPath is the edge's read-back endpoint. It mirrors
// livepreview.ControlPrefix+"status" ("/__live-preview/status"); hardcoded here
// so the control plane does not import the edge data-plane package. The edge
// contract (docs/features/test-slots/live-preview.md) is stable.
const previewStatusPath = "/__live-preview/status"

// PreviewEdgeStatus mirrors the JSON the edge returns from
// GET /__live-preview/status. It reports the LIVE bundle — the build `current`
// resolves to — so a failed push (which never flips `current`) leaves status
// reporting the prior good build.
type PreviewEdgeStatus struct {
	OverrideActive bool   `json:"override_active"`
	Build          string `json:"build"`
	Release        string `json:"release"`
	PushedAt       string `json:"pushed_at"`
}

// PreviewStatusReader reads an edge's live-preview status. Implementations
// authenticate as a service principal (the edge's status route accepts any
// valid auth.romaine.life token, not owner-scoped).
type PreviewStatusReader interface {
	ReadStatus(ctx context.Context, previewURL string) (PreviewEdgeStatus, error)
}

// httpPreviewStatusReader reads GET {previewURL}/__live-preview/status with a
// service-principal bearer token from the injected token source.
type httpPreviewStatusReader struct {
	tokens ServiceTokenSource
	http   *http.Client
}

// NewHTTPPreviewStatusReader builds a status reader. Returns nil when the token
// source is nil (unconfigured), so the verifier reconciler can fail closed
// rather than emit unauthenticated read-backs.
func NewHTTPPreviewStatusReader(tokens ServiceTokenSource, httpClient *http.Client) *httpPreviewStatusReader {
	if tokens == nil {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &httpPreviewStatusReader{tokens: tokens, http: httpClient}
}

func (r *httpPreviewStatusReader) ReadStatus(ctx context.Context, previewURL string) (PreviewEdgeStatus, error) {
	if r == nil || r.tokens == nil {
		return PreviewEdgeStatus{}, fmt.Errorf("preview status reader not configured")
	}
	base := strings.TrimRight(strings.TrimSpace(previewURL), "/")
	if base == "" {
		return PreviewEdgeStatus{}, fmt.Errorf("preview url is empty")
	}
	token, err := r.tokens.Token(ctx)
	if err != nil {
		return PreviewEdgeStatus{}, fmt.Errorf("mint service token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+previewStatusPath, nil)
	if err != nil {
		return PreviewEdgeStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return PreviewEdgeStatus{}, fmt.Errorf("read edge status: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PreviewEdgeStatus{}, fmt.Errorf("edge status returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var status PreviewEdgeStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return PreviewEdgeStatus{}, fmt.Errorf("decode edge status: %w", err)
	}
	return status, nil
}

// PreviewVerifierStore is the durable surface the observed read-back verifier
// needs. Satisfied by the runtime store.
type PreviewVerifierStore interface {
	ListPreviewEnvironments(ctx context.Context) ([]PreviewEnvironment, error)
	UpdatePreviewEnvironmentIfMatch(ctx context.Context, project, name string, mutate func(PreviewEnvironment) (PreviewEnvironment, error)) (PreviewEnvironment, error)
}

// VerifyPreviewEnvironment performs ONE observed read-back: it reads the edge's
// status as a service principal and folds it into durable state. This is the
// load-bearing "observed, not claimed" step — a push is marked live ONLY here,
// never on the receipt. A transient read error is returned without a durable
// flip (the env stays `pushed`/pending and the reconciler retries); a confirmed
// mismatch becomes the durable `stale` state.
//
// Metrics are counted on the STATE TRANSITION (not per poll), so a persistently
// stale env that is re-polled does not inflate the stale counter.
func VerifyPreviewEnvironment(
	ctx context.Context,
	store PreviewVerifierStore,
	reader PreviewStatusReader,
	env PreviewEnvironment,
	now func() time.Time,
	logf func(string, ...any),
) (PreviewEnvironment, error) {
	if now == nil {
		now = time.Now
	}
	status, err := reader.ReadStatus(ctx, env.URL)
	if err != nil {
		if logf != nil {
			logf("preview verify read-back failed project=%s name=%s url=%s err=%v", env.Project, env.Name, env.URL, err)
		}
		return env, err
	}
	var priorState string
	updated, err := store.UpdatePreviewEnvironmentIfMatch(ctx, env.Project, env.Name, func(cur PreviewEnvironment) (PreviewEnvironment, error) {
		priorState = cur.State
		return cur.RecordObserved(status.OverrideActive, status.Build, now()), nil
	})
	if err != nil {
		if logf != nil {
			logf("preview verify durable update failed project=%s name=%s err=%v", env.Project, env.Name, err)
		}
		return env, err
	}
	switch {
	case updated.State == PreviewStateLive && priorState != PreviewStateLive:
		metrics.RecordLivePreviewObservedConfirmed()
		if logf != nil {
			logf("preview observed live project=%s name=%s build=%s", updated.Project, updated.Name, updated.ObservedBuildID)
		}
	case updated.State == PreviewStateStale && priorState != PreviewStateStale:
		metrics.RecordLivePreviewStaleDetected()
		if logf != nil {
			logf("preview STALE project=%s name=%s pushed=%s observed=%s detail=%q",
				updated.Project, updated.Name, updated.LiveBuildID, updated.ObservedBuildID, updated.Detail)
		}
	}
	return updated, nil
}

// previewVerifyWake nudges the verifier reconciler to read back immediately
// (e.g. right after a push receipt) instead of waiting for the poll tick.
var previewVerifyWake atomic.Value // func()

func wakePreviewVerify() {
	if fn, ok := previewVerifyWake.Load().(func()); ok && fn != nil {
		fn()
	}
}

// defaultPreviewVerifyInterval bounds the read-back poll cadence. Cost story:
// each tick reads back only preview envs with an UNCONFIRMED push
// (PendingObservation), which is at most one HTTP GET per active preview whose
// latest push is not yet observed-live — a small, naturally self-limiting set
// (once a push is confirmed live the env stops being polled). A push receipt
// also wakes the loop, so the steady-state cost is ~one read-back per push plus
// a slow re-check of any env stuck stale.
const defaultPreviewVerifyInterval = 15 * time.Second

// previewVerifyReconcilerShouldRun is the deterministic gate for the verifier
// loop, factored out so it is unit-testable without spawning a goroutine. The
// control-plane gate is first: a slot process (ControlPlaneLoopsEnabled=false)
// must NEVER run a background reconciler that mutates shared Postgres rows (Test
// Slots contract). It also requires a preview-capable store and a configured
// status reader.
func previewVerifyReconcilerShouldRun(settings Settings, storeSupported bool, reader PreviewStatusReader, logf func(string, ...any)) bool {
	if !settings.ControlPlaneLoopsEnabled {
		return false
	}
	if !storeSupported {
		if logf != nil {
			logf("preview verify reconciler disabled: store does not support preview environments")
		}
		return false
	}
	if reader == nil {
		if logf != nil {
			logf("preview verify reconciler disabled: no status reader (auth.romaine.life service token source unconfigured)")
		}
		return false
	}
	return true
}

// StartPreviewVerifyReconciler runs the observed read-back loop. It self-gates
// on Settings.ControlPlaneLoopsEnabled: a slot process (any k8s/issue release)
// must NEVER run it — per the Test Slots contract, no background reconciler may
// mutate shared Postgres rows outside the control-plane gate. The prod glimmung
// Deployment leaves the gate at true; every per-issue/slot release sets it
// false, so this returns early there.
func StartPreviewVerifyReconciler(ctx context.Context, settings Settings, store ReadStore, reader PreviewStatusReader, logf func(string, ...any)) {
	verifierStore, ok := store.(PreviewVerifierStore)
	if !previewVerifyReconcilerShouldRun(settings, ok && verifierStore != nil, reader, logf) {
		return
	}
	wakeCh := make(chan struct{}, 128)
	previewVerifyWake.Store(func() {
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	})
	go func() {
		ticker := time.NewTicker(defaultPreviewVerifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wakeCh:
			case <-ticker.C:
			}
			drainPreviewVerify(ctx, verifierStore, reader, logf)
		}
	}()
}

// drainPreviewVerify reads back every enabled preview env with an unconfirmed
// push. Bounded by construction (see defaultPreviewVerifyInterval).
func drainPreviewVerify(ctx context.Context, store PreviewVerifierStore, reader PreviewStatusReader, logf func(string, ...any)) {
	envs, err := store.ListPreviewEnvironments(ctx)
	if err != nil {
		if logf != nil {
			logf("preview verify list failed: %v", err)
		}
		return
	}
	for _, env := range envs {
		if !env.Enabled || !env.PendingObservation() || strings.TrimSpace(env.URL) == "" {
			continue
		}
		_, _ = VerifyPreviewEnvironment(ctx, store, reader, env, time.Now, logf)
	}
}

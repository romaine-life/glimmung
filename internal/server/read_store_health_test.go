package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStoreHealthMonitorStartsNotReady(t *testing.T) {
	m := NewStoreHealthMonitor(func(context.Context) error { return nil }, StoreHealthOptions{})
	if m.Ready() {
		t.Fatal("monitor must start not-ready before any probe completes")
	}
}

func TestStoreHealthMonitorBecomesReadyAfterSuccessThreshold(t *testing.T) {
	m := NewStoreHealthMonitor(
		func(context.Context) error { return nil },
		StoreHealthOptions{SuccessThreshold: 2, FailureThreshold: 3},
	)
	m.checkOnce(context.Background())
	if m.Ready() {
		t.Fatal("not ready expected after 1 success with successThreshold=2")
	}
	m.checkOnce(context.Background())
	if !m.Ready() {
		t.Fatal("ready expected after 2 consecutive successes")
	}
}

// TestStoreHealthMonitorTransientFailureDoesNotFlip is the core incident
// property: a brief read-store slowdown (fewer than failureThreshold
// consecutive failed probes) must NOT flip readiness, so the pod is not yanked
// out of the Service for transient latency.
func TestStoreHealthMonitorTransientFailureDoesNotFlip(t *testing.T) {
	var down bool
	m := NewStoreHealthMonitor(
		func(context.Context) error {
			if down {
				return errors.New("probe down")
			}
			return nil
		},
		StoreHealthOptions{SuccessThreshold: 1, FailureThreshold: 3},
	)
	m.checkOnce(context.Background()) // success -> ready
	if !m.Ready() {
		t.Fatal("ready expected after first success")
	}
	down = true
	m.checkOnce(context.Background()) // fail 1
	m.checkOnce(context.Background()) // fail 2 (still under threshold)
	if !m.Ready() {
		t.Fatal("a transient blip under failureThreshold must not flip readiness")
	}
	m.checkOnce(context.Background()) // fail 3 -> not ready
	if m.Ready() {
		t.Fatal("not-ready expected after failureThreshold consecutive failures")
	}
	down = false
	m.checkOnce(context.Background()) // recover
	if !m.Ready() {
		t.Fatal("ready expected to recover after a success")
	}
}

func TestStoreHealthMonitorTreatsPanicAsFailure(t *testing.T) {
	var boom bool
	m := NewStoreHealthMonitor(
		func(context.Context) error {
			if boom {
				panic("typed nil store")
			}
			return nil
		},
		StoreHealthOptions{SuccessThreshold: 1, FailureThreshold: 1},
	)
	m.checkOnce(context.Background()) // success -> ready
	if !m.Ready() {
		t.Fatal("ready expected after success")
	}
	boom = true
	m.checkOnce(context.Background()) // panicking probe must be a failed probe
	if m.Ready() {
		t.Fatal("a panicking probe must flip readiness to not-ready, not crash the monitor")
	}
}

func TestStoreHealthMonitorStartStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m := NewStoreHealthMonitor(func(context.Context) error { return nil }, StoreHealthOptions{})
	go func() {
		m.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestReadyzReporterContract pins the handler contract: nil reporter is "not
// configured", a not-ready reporter is 503, a ready reporter is 200. The
// handler does no database I/O.
func TestReadyzReporterContract(t *testing.T) {
	cases := []struct {
		name     string
		reporter ReadinessReporter
		want     int
	}{
		{"nil reporter is not configured", nil, http.StatusServiceUnavailable},
		{"not ready is 503", staticReadiness{ready: false}, http.StatusServiceUnavailable},
		{"ready is 200", staticReadiness{ready: true}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			readyz(tc.reporter).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d", rec.Code, tc.want)
			}
		})
	}
}

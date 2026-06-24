package server

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// ReadinessReporter is the cached read-store health that /readyz serves. It is
// read on the hot probe path and must not touch the database.
type ReadinessReporter interface {
	Ready() bool
}

// staticReadiness is the reporter used when no StoreHealthMonitor is wired
// (tests and partial constructors). It never touches a database. Production
// (cmd/glimmung-go) always wires a StoreHealthMonitor; a process given a store
// but no monitor reports ready, and /readyz with no store at all reports "not
// configured" (the reporter is nil there).
type staticReadiness struct{ ready bool }

func (s staticReadiness) Ready() bool { return s.ready }

// Default StoreHealthMonitor tuning. The monitor flips unready only after
// FailureThreshold consecutive failed probes (~Interval*FailureThreshold of
// sustained trouble) and recovers after SuccessThreshold successes, so a
// transient query slowdown does not yank the pod out of the Service. The k8s
// readinessProbe then adds its own periodSeconds*failureThreshold layer on top
// of the cached gauge.
const (
	defaultHealthInterval         = 3 * time.Second
	defaultHealthProbeTimeout     = 2 * time.Second
	defaultHealthFailureThreshold = 3
	defaultHealthSuccessThreshold = 1
)

// StoreHealthOptions configures a StoreHealthMonitor. Zero values fall back to
// the production defaults above.
type StoreHealthOptions struct {
	Interval         time.Duration
	ProbeTimeout     time.Duration
	FailureThreshold int
	SuccessThreshold int
}

// StoreHealthMonitor maintains the cached readiness gauge for the Postgres read
// store. A single background goroutine probes the store on a fixed interval
// with its own bounded context — never a request context — and applies
// hysteresis. /readyz reads Ready() in O(1) with no database access, which is
// the property that keeps a brief DB slowdown from flipping the only replica
// out of rotation (the failure chain in the read-store saturation incident:
// a 1s kubelet probe timeout caught a synchronous SELECT under load and marked
// the sole pod NotReady).
//
// The hysteresis counters are owned exclusively by the Start goroutine (and by
// a single-goroutine test driving checkOnce); Ready() is safe for concurrent
// readers via the atomic.
type StoreHealthMonitor struct {
	probe            func(context.Context) error
	interval         time.Duration
	probeTimeout     time.Duration
	failureThreshold int
	successThreshold int

	ready      atomic.Bool
	consecOK   int
	consecFail int
}

// NewStoreHealthMonitor builds a monitor around probe. probe should run one
// cheap read that proves the store can serve (e.g. ListProjects). The monitor
// starts not-ready; Ready() flips true only after SuccessThreshold consecutive
// successful probes, so the pod does not accept traffic before Postgres is
// reachable.
func NewStoreHealthMonitor(probe func(context.Context) error, opts StoreHealthOptions) *StoreHealthMonitor {
	m := &StoreHealthMonitor{
		probe:            probe,
		interval:         opts.Interval,
		probeTimeout:     opts.ProbeTimeout,
		failureThreshold: opts.FailureThreshold,
		successThreshold: opts.SuccessThreshold,
	}
	if m.interval <= 0 {
		m.interval = defaultHealthInterval
	}
	if m.probeTimeout <= 0 {
		m.probeTimeout = defaultHealthProbeTimeout
	}
	if m.failureThreshold <= 0 {
		m.failureThreshold = defaultHealthFailureThreshold
	}
	if m.successThreshold <= 0 {
		m.successThreshold = defaultHealthSuccessThreshold
	}
	metrics.SetReadStoreReady(false)
	return m
}

// Ready reports the cached readiness state. Safe for concurrent use; does no
// I/O. A nil monitor reports not-ready.
func (m *StoreHealthMonitor) Ready() bool {
	if m == nil {
		return false
	}
	return m.ready.Load()
}

// Start runs the probe loop until ctx is cancelled. It probes once immediately
// so readiness converges within one probe rather than waiting a full interval,
// then ticks every Interval. Intended to run in its own goroutine for the life
// of the process.
func (m *StoreHealthMonitor) Start(ctx context.Context) {
	if m == nil || m.probe == nil {
		return
	}
	m.checkOnce(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkOnce(ctx)
		}
	}
}

// checkOnce runs a single bounded probe and folds the result into the
// hysteresis counters. A panicking probe is treated as a failed probe so a
// typed-nil store can never crash the monitor goroutine.
func (m *StoreHealthMonitor) checkOnce(ctx context.Context) {
	err := m.runProbe(ctx)
	if err != nil {
		metrics.RecordReadStoreProbe("error")
		m.consecFail++
		m.consecOK = 0
		if m.ready.Load() && m.consecFail >= m.failureThreshold {
			m.ready.Store(false)
			metrics.SetReadStoreReady(false)
		}
		return
	}
	metrics.RecordReadStoreProbe("ok")
	m.consecOK++
	m.consecFail = 0
	if !m.ready.Load() && m.consecOK >= m.successThreshold {
		m.ready.Store(true)
		metrics.SetReadStoreReady(true)
	}
}

func (m *StoreHealthMonitor) runProbe(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errProbePanicked
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, m.probeTimeout)
	defer cancel()
	return m.probe(probeCtx)
}

var errProbePanicked = errProbe("read-store probe panicked")

type errProbe string

func (e errProbe) Error() string { return string(e) }

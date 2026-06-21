// Command live-preview-edge is the standalone data plane of Glimmung's live
// frontend preview feature. It runs as the served container in front of a
// stable app backend inside a preview test environment: it reverse-proxies the
// backend by default and, once a developer pushes a freshly-built frontend
// bundle to /__live-preview/push, serves that override first so UI iterates in
// seconds without a CI image build+deploy.
//
// It is the generic, app-agnostic replacement for tank-operator's in-app
// static-override receiver; an app's slot chart wires it in via the reusable
// Helm partial (k8s/live-preview-edge). It is INACTIVE until a chart sets
// livePreview.enabled, so building/shipping it changes no app's behavior.
//
// This is the live-preview lane (scratch, for seeing), never the faithful
// image-deploy validation lane, and it shares no vocabulary with the retired
// hot-swap path. See docs/features/test-slots/live-preview.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/romaine-life/glimmung/internal/auth"
	"github.com/romaine-life/glimmung/internal/livepreview"
	"github.com/romaine-life/glimmung/internal/metrics"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg := livepreview.Config{
		UpstreamURL:       envOr("LIVE_PREVIEW_EDGE_UPSTREAM", ""),
		BackendPrefixes:   splitCSV(os.Getenv("LIVE_PREVIEW_EDGE_BACKEND_PREFIXES")),
		OverrideRoot:      envOr("LIVE_PREVIEW_EDGE_OVERRIDE_ROOT", ""),
		AuthorizedSubject: envOr("LIVE_PREVIEW_EDGE_AUTHORIZED_SUBJECT", ""),
	}
	listen := envOr("LIVE_PREVIEW_EDGE_LISTEN", ":8080")

	edge, err := livepreview.NewEdge(cfg, auth.NewRomaineLifeJWTVerifier(), logger)
	if err != nil {
		logger.Error("live-preview edge misconfigured", "err", err)
		os.Exit(1)
	}

	// Seed the served-build gauge from durable state so a pod restart with an
	// already-active override reports the live build immediately, not zero.
	metrics.SetLivePreviewEdgeServedBuild(edge.Store().Status().Build)

	// The edge is served directly (not via metrics.Middleware, which is built
	// for glimmung's ServeMux and would bucket every edge request under
	// pattern="unmatched"). The edge owns its meaningful instrumentation: push
	// outcomes, serve disposition, proxy errors, and the served-build gauge,
	// all exported at /__live-preview/metrics.
	srv := &http.Server{
		Addr:              listen,
		Handler:           edge,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("live-preview edge serving",
			"listen", listen,
			"upstream", cfg.UpstreamURL,
			"backend_prefixes", cfg.BackendPrefixes,
			"override_root", cfg.OverrideRoot,
			"authorized_subject", cfg.AuthorizedSubject,
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("live-preview edge stopped", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("live-preview edge shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("live-preview edge shutdown error", "err", err)
			os.Exit(1)
		}
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

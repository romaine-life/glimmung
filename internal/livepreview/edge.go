package livepreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/auth"
	"github.com/romaine-life/glimmung/internal/metrics"
)

// ControlPrefix is the reserved path namespace the edge owns. Requests under it
// are ALWAYS handled locally and NEVER proxied to the backend or served from
// the override, so an app's own /api, /healthz, /metrics, or frontend routes
// can never collide with the edge's control + ops surface.
const ControlPrefix = "/__live-preview/"

const (
	pushSuffix    = "push"
	statusSuffix  = "status"
	healthSuffix  = "healthz"
	readySuffix   = "readyz"
	metricsSuffix = "metrics"

	// BuildHeader carries the pusher-supplied build id on PUT push. It is the
	// id /__live-preview/status reports back, so Glimmung's observed-read-back
	// verifier can confirm the edge serves EXACTLY the build that was pushed.
	BuildHeader = "X-Live-Preview-Build"

	statusBodyLimit = 4 << 10
)

// Config parameterizes the edge. All fields are required except BackendPrefixes
// (which may be empty). The standalone binary fills these from env/flags; tests
// construct them directly.
type Config struct {
	// UpstreamURL is the app backend base URL the edge reverse-proxies to
	// (e.g. http://127.0.0.1:8000). The backend listens internally; the edge
	// is the pod's served port.
	UpstreamURL string
	// BackendPrefixes are request path prefixes always reverse-proxied to the
	// backend (e.g. /api, /healthz). They keep the stable backend's API
	// reachable through the edge regardless of override state.
	BackendPrefixes []string
	// OverrideRoot is the directory the edge writes/serves overrides from (the
	// per-pod emptyDir the chart mounts).
	OverrideRoot string
	// AuthorizedSubject is the auth.romaine.life JWT `sub` permitted to push or
	// delete an override — the verified, IdP-signed per-owner subject of the
	// session/lease that owns this preview env. Any other subject is rejected:
	// "a pod may only write its own preview". It is matched exactly.
	AuthorizedSubject string
}

// Edge is the live-preview data plane: a reverse proxy in front of a stable app
// backend with an override-first receiver. It is an http.Handler.
type Edge struct {
	store             *Store
	verifier          *auth.RomaineLifeJWTVerifier
	proxy             *httputil.ReverseProxy
	prefixes          []string
	authorizedSubject string
	logger            *slog.Logger
	metricsHandler    http.Handler
	startedAt         time.Time
	// maxCompressed defaults to MaxPushCompressedBytes; tests lower it so the
	// 413/too_large path can be exercised without a 64 MiB upload.
	maxCompressed int64
}

type proxyDispositionKey struct{}

// NewEdge validates the config and wires the edge. The verifier is injected so
// tests can supply a stub-JWKS verifier; production passes
// auth.NewRomaineLifeJWTVerifier(). logger defaults to slog.Default().
func NewEdge(cfg Config, verifier *auth.RomaineLifeJWTVerifier, logger *slog.Logger) (*Edge, error) {
	if strings.TrimSpace(cfg.OverrideRoot) == "" {
		return nil, errors.New("override root is required")
	}
	if strings.TrimSpace(cfg.UpstreamURL) == "" {
		return nil, errors.New("upstream URL is required")
	}
	if strings.TrimSpace(cfg.AuthorizedSubject) == "" {
		return nil, errors.New("authorized subject is required")
	}
	if verifier == nil {
		return nil, errors.New("auth verifier is required")
	}
	target, err := url.Parse(strings.TrimSpace(cfg.UpstreamURL))
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("upstream URL must be absolute (scheme://host): %q", cfg.UpstreamURL)
	}
	prefixes, err := normalizePrefixes(cfg.BackendPrefixes)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	e := &Edge{
		store:             NewStore(cfg.OverrideRoot),
		verifier:          verifier,
		prefixes:          prefixes,
		authorizedSubject: strings.TrimSpace(cfg.AuthorizedSubject),
		logger:            logger,
		metricsHandler:    metrics.Handler(),
		startedAt:         time.Now(),
		maxCompressed:     MaxPushCompressedBytes,
	}
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)     // route to upstream, preserving the inbound path
			pr.SetXForwarded()    // X-Forwarded-For / -Host / -Proto
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			disp, _ := r.Context().Value(proxyDispositionKey{}).(string)
			metrics.RecordLivePreviewEdgeProxyError(disp)
			e.logger.Error("live-preview edge upstream proxy failed",
				"disposition", disp, "path", r.URL.Path, "err", err)
			http.Error(w, "upstream backend unavailable", http.StatusBadGateway)
		},
	}
	return e, nil
}

// Store exposes the underlying release store (used by the binary to seed the
// served-build gauge at startup).
func (e *Edge) Store() *Store { return e.store }

// ServeHTTP routes each request through the fixed precedence:
//  1. /__live-preview/* control + ops routes — local, never proxied.
//  2. configured backend prefixes — reverse-proxied to the app backend.
//  3. otherwise (frontend/asset paths):
//     - override active + file exists  -> serve the file
//     - override active + file missing -> SPA-fallback to the override index.html
//     - no override active             -> proxy the backend (fresh preview shows
//     the stable app's own frontend until the first push)
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, ControlPrefix) {
		e.serveControl(w, r, strings.TrimPrefix(p, ControlPrefix))
		return
	}
	if e.matchesBackendPrefix(p) {
		e.proxyTo(w, r, metrics.LivePreviewServeBackendProxy)
		return
	}
	if distDir, ok := e.store.ActiveDistDir(); ok {
		e.serveOverride(w, r, distDir)
		return
	}
	e.proxyTo(w, r, metrics.LivePreviewServeFreshPassthrough)
}

// serveControl dispatches the reserved /__live-preview/* surface.
func (e *Edge) serveControl(w http.ResponseWriter, r *http.Request, suffix string) {
	switch suffix {
	case healthSuffix:
		writePlain(w, http.StatusOK, "ok")
	case readySuffix:
		writePlain(w, http.StatusOK, "ready")
	case metricsSuffix:
		e.metricsHandler.ServeHTTP(w, r)
	case statusSuffix:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		e.handleStatus(w, r)
	case pushSuffix:
		switch r.Method {
		case http.MethodPut:
			e.handlePush(w, r)
		case http.MethodDelete:
			e.handleDelete(w, r)
		default:
			methodNotAllowed(w, http.MethodPut, http.MethodDelete)
		}
	default:
		http.NotFound(w, r)
	}
}

// handlePush accepts a gzipped tar of a built frontend dist/ and atomically
// activates it. Service-principal + authorized-subject only.
func (e *Edge) handlePush(w http.ResponseWriter, r *http.Request) {
	user, ok := e.requireAuthorizedPusher(w, r)
	if !ok {
		return
	}
	build := strings.TrimSpace(r.Header.Get(BuildHeader))
	if build == "" {
		metrics.RecordLivePreviewEdgePush(metrics.LivePreviewPushOutcomeBadArchive)
		e.logger.Warn("live-preview push rejected: missing build id",
			"header", BuildHeader, "actor", user.ActorEmail)
		writeError(w, http.StatusBadRequest, "missing "+BuildHeader+" header")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, e.maxCompressed)
	res, err := e.store.Push(r.Body, build, time.Now())
	if err != nil {
		status, outcome := classifyPushErr(err)
		metrics.RecordLivePreviewEdgePush(outcome)
		e.logger.Warn("live-preview push failed",
			"outcome", outcome, "build", build, "actor", user.ActorEmail, "err", err)
		writeError(w, status, fmt.Sprintf("push rejected (%s): %v", outcome, err))
		return
	}

	metrics.RecordLivePreviewEdgePush(metrics.LivePreviewPushOutcomeOK)
	metrics.SetLivePreviewEdgeServedBuild(res.Meta.Build)
	e.logger.Info("live-preview override activated",
		"build", res.Meta.Build, "release", res.Meta.Release,
		"files", res.Files, "bytes", res.Bytes, "actor", user.ActorEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"build":     res.Meta.Build,
		"release":   res.Meta.Release,
		"files":     res.Files,
		"bytes":     res.Bytes,
		"pushed_at": res.Meta.PushedAt.Format(time.RFC3339),
		"by":        user.ActorEmail,
	})
}

// handleDelete reverts the edge to backend passthrough. Service-principal +
// authorized-subject only.
func (e *Edge) handleDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := e.requireAuthorizedPusher(w, r)
	if !ok {
		return
	}
	wasActive, err := e.store.Delete()
	if err != nil {
		metrics.RecordLivePreviewEdgePush(metrics.LivePreviewPushOutcomeError)
		e.logger.Error("live-preview revert failed", "actor", user.ActorEmail, "err", err)
		writeError(w, http.StatusInternalServerError, "revert failed: "+err.Error())
		return
	}
	metrics.RecordLivePreviewEdgePush(metrics.LivePreviewPushOutcomeReverted)
	metrics.SetLivePreviewEdgeServedBuild("")
	e.logger.Info("live-preview override reverted", "was_active", wasActive, "actor", user.ActorEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "reverted",
		"was_active": wasActive,
	})
}

// handleStatus returns the read-back contract: whether an override is active,
// the LIVE build id, release name, and pushed_at. Authenticated (any accepted
// auth.romaine.life role) but NOT owner-scoped: the next-stage observed
// read-back verifier (a service principal) and the owning developer both read
// it, and it exposes only non-sensitive liveness metadata — so it is gated
// behind a valid token (not open to the public preview URL) yet not confined to
// the push subject.
func (e *Edge) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := e.requireAuthenticated(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, e.store.Status())
}

// serveOverride serves a file from the active override, SPA-falling-back to the
// override's index.html when the requested file does not exist.
func (e *Edge) serveOverride(w http.ResponseWriter, r *http.Request, distDir string) {
	full, ok := resolveServePath(distDir, r.URL.Path)
	if ok && serveFile(w, r, full, filepath.Base(full)) {
		metrics.RecordLivePreviewEdgeServe(metrics.LivePreviewServeOverrideFile)
		return
	}
	idx := filepath.Join(distDir, "index.html")
	if serveFile(w, r, idx, "index.html") {
		metrics.RecordLivePreviewEdgeServe(metrics.LivePreviewServeOverrideSPA)
		return
	}
	// The override is active but its index.html is unreadable. Push requires a
	// root index.html, so this is on-disk corruption, not a routing case.
	e.logger.Error("live-preview override active but index.html unreadable", "dist", distDir)
	writeError(w, http.StatusInternalServerError, "override index unavailable")
}

// proxyTo reverse-proxies the request to the backend, tagging the disposition
// so the proxy ErrorHandler attributes a failure correctly.
func (e *Edge) proxyTo(w http.ResponseWriter, r *http.Request, disposition string) {
	metrics.RecordLivePreviewEdgeServe(disposition)
	ctx := context.WithValue(r.Context(), proxyDispositionKey{}, disposition)
	e.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// requireAuthorizedPusher enforces the push/delete auth model: a valid
// auth.romaine.life service-principal JWT whose verified subject equals the
// configured authorized subject. Records the unauthorized push outcome on any
// rejection.
func (e *Edge) requireAuthorizedPusher(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	token := bearerToken(r)
	if token == "" {
		return nil, e.denyPush(w, http.StatusUnauthorized, "missing bearer token", "")
	}
	user, err := e.verifier.Decode(r.Context(), token)
	if err != nil {
		status, msg := authStatus(err)
		return nil, e.denyPush(w, status, msg, "")
	}
	if !user.IsService() {
		return nil, e.denyPush(w, http.StatusForbidden, "push requires role=service", user.ActorEmail)
	}
	if !subjectAuthorized(user.Sub, e.authorizedSubject) {
		return nil, e.denyPush(w, http.StatusForbidden,
			"push subject is not authorized for this preview", user.ActorEmail)
	}
	return &user, true
}

func (e *Edge) denyPush(w http.ResponseWriter, status int, msg, actor string) bool {
	metrics.RecordLivePreviewEdgePush(metrics.LivePreviewPushOutcomeUnauthorized)
	e.logger.Warn("live-preview push unauthorized", "status", status, "reason", msg, "actor", actor)
	writeError(w, status, msg)
	return false
}

// requireAuthenticated accepts any valid auth.romaine.life token (the closed
// admin/user/service role set the verifier already enforces).
func (e *Edge) requireAuthenticated(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	user, err := e.verifier.Decode(r.Context(), token)
	if err != nil {
		status, msg := authStatus(err)
		writeError(w, status, msg)
		return nil, false
	}
	return &user, true
}

func (e *Edge) matchesBackendPrefix(p string) bool {
	for _, prefix := range e.prefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// --- helpers ----------------------------------------------------------------

// resolveServePath maps a request path to a file under distDir. path.Clean
// neutralizes any ".." so the result can never escape distDir; the Rel check is
// defense in depth. ok=false routes the caller to the SPA fallback.
func resolveServePath(distDir, urlPath string) (string, bool) {
	clean := path.Clean("/" + urlPath)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		rel = "index.html"
	}
	full := filepath.Join(distDir, filepath.FromSlash(rel))
	r, err := filepath.Rel(distDir, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// serveFile opens and serves a regular file with http.ServeContent (which sets
// the content type from name's extension and handles range/conditional
// requests). Returns false when the path is missing or a directory, so the
// caller can SPA-fall-back.
func serveFile(w http.ResponseWriter, r *http.Request, fullPath, name string) bool {
	f, err := os.Open(fullPath)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return false
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
	return true
}

// normalizePrefixes trims, requires a leading slash, drops empties, and rejects
// "/" (which would proxy every frontend path and disable the override).
func normalizePrefixes(in []string) ([]string, error) {
	var out []string
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimRight(p, "/")
		if p == "" {
			return nil, errors.New(`backend prefix "/" would proxy all frontend paths; remove it`)
		}
		out = append(out, p)
	}
	return out, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// subjectAuthorized reports whether a verified JWT subject is the configured
// authorized pusher. Exact match on the unforgeable, IdP-signed subject — no
// prefix or fallback matching.
func subjectAuthorized(sub, authorized string) bool {
	sub = strings.TrimSpace(sub)
	return sub != "" && sub == strings.TrimSpace(authorized)
}

// authStatus extracts the HTTP status + message from an auth.AuthError, falling
// back to 401 for anything else.
func authStatus(err error) (int, string) {
	var ae auth.AuthError
	if errors.As(err, &ae) {
		return ae.Status, ae.Message
	}
	return http.StatusUnauthorized, "invalid token"
}

// classifyPushErr maps a Store.Push error to (HTTP status, push outcome). Order
// matters: a compressed-size overflow is an http.MaxBytesError wrapped inside
// the ErrBadArchive chain, so it must be detected before the bad-archive arm.
func classifyPushErr(err error) (int, string) {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, metrics.LivePreviewPushOutcomeTooLarge
	case errors.Is(err, ErrNoIndex), errors.Is(err, ErrBadArchive):
		return http.StatusBadRequest, metrics.LivePreviewPushOutcomeBadArchive
	default:
		return http.StatusInternalServerError, metrics.LivePreviewPushOutcomeError
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

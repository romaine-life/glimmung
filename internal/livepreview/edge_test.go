package livepreview

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/romaine-life/glimmung/internal/auth"
)

const ownerSubject = "svc:preview:owner-1"

// --- JWKS fixture (mirrors internal/auth's test fixture, kept local because
// that one is package-private) ------------------------------------------------

type jwksFixture struct {
	t      *testing.T
	priv   *rsa.PrivateKey
	kid    string
	server *httptest.Server
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	f := &jwksFixture{t: t, priv: priv, kid: "edge-test-kid"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{map[string]any{"kid": f.kid, "kty": "RSA", "alg": "RS256", "use": "sig", "n": n, "e": e}},
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *jwksFixture) verifier() *auth.RomaineLifeJWTVerifier {
	return auth.NewRomaineLifeJWTVerifierForTesting(f.server.URL, f.server.URL+"/api/auth/jwks", f.server.Client())
}

func (f *jwksFixture) sign(claims jwt.MapClaims) string {
	f.t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signed, err := tok.SignedString(f.priv)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return signed
}

func (f *jwksFixture) serviceToken(sub, actor string) string {
	return f.sign(jwt.MapClaims{
		"iss": f.server.URL, "sub": sub, "role": auth.RomaineRoleService,
		"actor_email": actor, "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func (f *jwksFixture) userToken(email string) string {
	return f.sign(jwt.MapClaims{
		"iss": f.server.URL, "sub": "user-1", "role": auth.RomaineRoleUser,
		"email": email, "exp": time.Now().Add(time.Hour).Unix(),
	})
}

// --- harness ----------------------------------------------------------------

type harness struct {
	edge    *Edge
	fixture *jwksFixture
	backend *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "backend")
		_, _ = io.WriteString(w, "backend:"+r.URL.Path)
	}))
	t.Cleanup(backend.Close)

	f := newJWKSFixture(t)
	edge, err := NewEdge(Config{
		UpstreamURL:       backend.URL,
		BackendPrefixes:   []string{"/api", "/healthz"},
		OverrideRoot:      t.TempDir(),
		AuthorizedSubject: ownerSubject,
	}, f.verifier(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	return &harness{edge: edge, fixture: f, backend: backend}
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.edge.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(target string) *httptest.ResponseRecorder {
	return h.do(httptest.NewRequest(http.MethodGet, target, nil))
}

// pushAs PUTs a bundle with the given token + build header.
func (h *harness) pushAs(t *testing.T, data []byte, build, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, ControlPrefix+"push", bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if build != "" {
		req.Header.Set(BuildHeader, build)
	}
	return h.do(req)
}

func (h *harness) ownerToken() string {
	return h.fixture.serviceToken(ownerSubject, "owner@example.com")
}

// --- routing + serving ------------------------------------------------------

func TestEdgeFreshPreviewProxiesBackend(t *testing.T) {
	h := newHarness(t)
	// No override pushed yet: a frontend path passes through to the stable
	// backend so a fresh preview shows the app's own frontend.
	rec := h.get("/")
	if rec.Code != http.StatusOK || rec.Body.String() != "backend:/" {
		t.Fatalf("fresh / = %d %q, want 200 backend:/", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Served-By") != "backend" {
		t.Error("fresh preview should be served by the backend")
	}
}

func TestEdgeBackendPrefixAlwaysProxied(t *testing.T) {
	h := newHarness(t)
	// Even after an override is active, configured backend prefixes proxy.
	if rec := h.pushAs(t, bundle(t), "b1", h.ownerToken()); rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/api", "/api/things/1", "/healthz"} {
		rec := h.get(p)
		if rec.Code != http.StatusOK || rec.Body.String() != "backend:"+p {
			t.Errorf("%s = %d %q, want backend passthrough", p, rec.Code, rec.Body.String())
		}
	}
	// A path that only shares a prefix string is NOT a backend prefix.
	if rec := h.get("/apiary"); rec.Body.String() == "backend:/apiary" {
		t.Error("/apiary must not match the /api backend prefix")
	}
}

func TestEdgeServesOverrideAndSPAFallback(t *testing.T) {
	h := newHarness(t)
	data := bundle(t, reg("assets/app.js", "APP_JS"), reg("index.html", "OVERRIDE_INDEX"))
	if rec := h.pushAs(t, data, "b1", h.ownerToken()); rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}

	// Root serves the override index (not the backend).
	if rec := h.get("/"); rec.Body.String() != "OVERRIDE_INDEX" {
		t.Errorf("/ = %q, want OVERRIDE_INDEX", rec.Body.String())
	}
	// An existing asset is served directly.
	if rec := h.get("/assets/app.js"); rec.Body.String() != "APP_JS" {
		t.Errorf("/assets/app.js = %q, want APP_JS", rec.Body.String())
	}
	// A client-side route that is not a file SPA-falls-back to index.html.
	rec := h.get("/some/client/route")
	if rec.Code != http.StatusOK || rec.Body.String() != "OVERRIDE_INDEX" {
		t.Errorf("SPA fallback = %d %q, want 200 OVERRIDE_INDEX", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Served-By") == "backend" {
		t.Error("an active override must not fall through to the backend for frontend paths")
	}
}

// --- push / status / delete lifecycle ---------------------------------------

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) Status {
	t.Helper()
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, rec.Body.String())
	}
	return st
}

func (h *harness) statusAs(token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, ControlPrefix+"status", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return h.do(req)
}

func TestEdgePushStatusReadBackAndReplace(t *testing.T) {
	h := newHarness(t)

	// Before any push: status reports no override.
	st := decodeStatus(t, h.statusAs(h.ownerToken()))
	if st.OverrideActive {
		t.Fatal("status before push should report override_active=false")
	}

	// First push.
	if rec := h.pushAs(t, bundle(t, reg("v", "one")), "build-1", h.ownerToken()); rec.Code != http.StatusOK {
		t.Fatalf("push 1: %d %s", rec.Code, rec.Body.String())
	}
	st = decodeStatus(t, h.statusAs(h.ownerToken()))
	if !st.OverrideActive || st.Build != "build-1" {
		t.Fatalf("status after push 1 = %+v, want active build-1", st)
	}
	if st.Release == "" || st.PushedAt == "" {
		t.Errorf("status missing release/pushed_at: %+v", st)
	}

	// Second push must REPLACE (flip), not just first-install.
	if rec := h.pushAs(t, bundle(t, reg("v", "two")), "build-2", h.ownerToken()); rec.Code != http.StatusOK {
		t.Fatalf("push 2: %d %s", rec.Code, rec.Body.String())
	}
	st = decodeStatus(t, h.statusAs(h.ownerToken()))
	if st.Build != "build-2" {
		t.Fatalf("status after push 2 build = %q, want build-2", st.Build)
	}
	if rec := h.get("/v"); rec.Body.String() != "two" {
		t.Errorf("served /v = %q, want two", rec.Body.String())
	}

	// DELETE reverts to backend passthrough.
	req := httptest.NewRequest(http.MethodDelete, ControlPrefix+"push", nil)
	req.Header.Set("Authorization", "Bearer "+h.ownerToken())
	if rec := h.do(req); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	st = decodeStatus(t, h.statusAs(h.ownerToken()))
	if st.OverrideActive {
		t.Error("status after delete should report override_active=false")
	}
	if rec := h.get("/"); rec.Body.String() != "backend:/" {
		t.Errorf("after delete / = %q, want backend:/ passthrough", rec.Body.String())
	}
}

// --- auth -------------------------------------------------------------------

func TestEdgePushAuthModel(t *testing.T) {
	h := newHarness(t)
	good := bundle(t)

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"user role", h.fixture.userToken("dev@example.com"), http.StatusForbidden},
		{"service wrong subject", h.fixture.serviceToken("svc:preview:someone-else", "x@example.com"), http.StatusForbidden},
		{"service right subject", h.ownerToken(), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.pushAs(t, good, "build-x", c.token)
			if rec.Code != c.want {
				t.Fatalf("push %s = %d, want %d (body=%s)", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestEdgeStatusRequiresAuthButNotOwner(t *testing.T) {
	h := newHarness(t)
	// Unauthenticated status is rejected (the read-back is token-gated).
	if rec := h.statusAs(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status no token = %d, want 401", rec.Code)
	}
	// A non-owner but valid caller (the verifier role, a different subject) may
	// read status — it is authenticated, not owner-scoped.
	verifierTok := h.fixture.serviceToken("svc:glimmung:read-back-verifier", "verifier@example.com")
	if rec := h.statusAs(verifierTok); rec.Code != http.StatusOK {
		t.Errorf("status as non-owner verifier = %d, want 200", rec.Code)
	}
	// A signed-in human (user role) may also read it.
	if rec := h.statusAs(h.fixture.userToken("dev@example.com")); rec.Code != http.StatusOK {
		t.Errorf("status as user = %d, want 200", rec.Code)
	}
}

// --- negative push paths don't change the served bundle ---------------------

func TestEdgeBadPushesDoNotChangeServedBundle(t *testing.T) {
	h := newHarness(t)
	// Establish a known-good live bundle.
	if rec := h.pushAs(t, bundle(t, reg("v", "good")), "build-good", h.ownerToken()); rec.Code != http.StatusOK {
		t.Fatalf("seed push: %d %s", rec.Code, rec.Body.String())
	}

	assertStillGood := func(t *testing.T) {
		t.Helper()
		if st := decodeStatus(t, h.statusAs(h.ownerToken())); st.Build != "build-good" {
			t.Errorf("live build = %q, want build-good (a rejected push must not flip)", st.Build)
		}
		if rec := h.get("/v"); rec.Body.String() != "good" {
			t.Errorf("served /v = %q, want good", rec.Body.String())
		}
	}

	t.Run("missing build header", func(t *testing.T) {
		rec := h.pushAs(t, bundle(t, reg("v", "new")), "", h.ownerToken())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("missing build = %d, want 400", rec.Code)
		}
		assertStillGood(t)
	})

	t.Run("missing index.html", func(t *testing.T) {
		rec := h.pushAs(t, tarGz(t, reg("v", "new")), "build-bad", h.ownerToken())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("missing index = %d, want 400", rec.Code)
		}
		assertStillGood(t)
	})

	t.Run("non-gzip body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, ControlPrefix+"push", bytes.NewReader([]byte("not a tarball")))
		req.Header.Set("Authorization", "Bearer "+h.ownerToken())
		req.Header.Set(BuildHeader, "build-bad")
		if rec := h.do(req); rec.Code != http.StatusBadRequest {
			t.Fatalf("non-gzip = %d, want 400", rec.Code)
		}
		assertStillGood(t)
	})

	t.Run("oversize compressed body", func(t *testing.T) {
		h.edge.maxCompressed = 16 // bytes; any real bundle exceeds this
		defer func() { h.edge.maxCompressed = MaxPushCompressedBytes }()
		rec := h.pushAs(t, bundle(t, reg("v", "new")), "build-bad", h.ownerToken())
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize = %d, want 413", rec.Code)
		}
		assertStillGood(t)
	})

	t.Run("unauthorized does not flip", func(t *testing.T) {
		rec := h.pushAs(t, bundle(t, reg("v", "new")), "build-bad", h.fixture.serviceToken("svc:preview:intruder", "x@example.com"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("intruder push = %d, want 403", rec.Code)
		}
		assertStillGood(t)
	})
}

// --- ops surface ------------------------------------------------------------

func TestEdgeOpsRoutesUnauthenticated(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{ControlPrefix + "healthz", ControlPrefix + "readyz"} {
		if rec := h.get(p); rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, rec.Code)
		}
	}
	rec := h.get(ControlPrefix + "metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("glimmung_live_preview_edge_push_total")) {
		t.Error("metrics output should include the edge push counter family")
	}
}

func TestEdgeControlRoutesMethodGuards(t *testing.T) {
	h := newHarness(t)
	// POST to push (only PUT/DELETE allowed).
	req := httptest.NewRequest(http.MethodPost, ControlPrefix+"push", nil)
	req.Header.Set("Authorization", "Bearer "+h.ownerToken())
	if rec := h.do(req); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST push = %d, want 405", rec.Code)
	}
	// POST to status (only GET allowed).
	if rec := h.do(httptest.NewRequest(http.MethodPost, ControlPrefix+"status", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
	// Unknown control route.
	if rec := h.get(ControlPrefix + "nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown control route = %d, want 404", rec.Code)
	}
}

// --- config validation ------------------------------------------------------

func TestNewEdgeValidatesConfig(t *testing.T) {
	v := newJWKSFixture(t).verifier()
	base := Config{UpstreamURL: "http://127.0.0.1:9", OverrideRoot: "/tmp/x", AuthorizedSubject: "s"}
	cases := map[string]Config{
		"missing upstream":   {OverrideRoot: "/tmp/x", AuthorizedSubject: "s"},
		"missing root":       {UpstreamURL: "http://127.0.0.1:9", AuthorizedSubject: "s"},
		"missing subject":    {UpstreamURL: "http://127.0.0.1:9", OverrideRoot: "/tmp/x"},
		"relative upstream":  {UpstreamURL: "127.0.0.1:9", OverrideRoot: "/tmp/x", AuthorizedSubject: "s"},
		"root backend prefix": func() Config { c := base; c.BackendPrefixes = []string{"/"}; return c }(),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewEdge(cfg, v, nil); err == nil {
				t.Fatalf("NewEdge(%s) = nil error, want validation error", name)
			}
		})
	}
	if _, err := NewEdge(base, v, nil); err != nil {
		t.Fatalf("NewEdge(valid) = %v, want nil", err)
	}
	if _, err := NewEdge(base, nil, nil); err == nil {
		t.Fatal("NewEdge(nil verifier) should error")
	}
}

package remotehost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/agentcost"
	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// fakeRunner records invocations and returns canned output per command name.
type fakeRunner struct {
	calls      [][]string
	outputs    map[string][]byte
	errs       map[string]error
	streamOut  map[string][]byte // canned stdout bytes for runStreaming, per command
	streamErr  map[string][]byte // canned stderr bytes for runStreaming, per command
	streamCode map[string]int    // exit code returned by runStreaming on error
}

func (f *fakeRunner) run(_ context.Context, _ string, _ []byte, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if err, ok := f.errs[name]; ok {
		return f.outputs[name], err
	}
	return f.outputs[name], nil
}

func (f *fakeRunner) runBackground(_ context.Context, name string, args ...string) (int, error) {
	f.calls = append(f.calls, append([]string{name + "(bg)"}, args...))
	if err, ok := f.errs[name]; ok {
		return 0, err
	}
	return 4242, nil
}

func (f *fakeRunner) runStreaming(_ context.Context, _ string, _ []byte, stdout, stderr io.Writer, name string, args ...string) (int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if out, ok := f.streamOut[name]; ok {
		_, _ = stdout.Write(out)
	}
	if errOut, ok := f.streamErr[name]; ok {
		_, _ = stderr.Write(errOut)
	}
	if err, ok := f.errs[name]; ok {
		code := 1
		if c, ok := f.streamCode[name]; ok {
			code = c
		}
		return code, err
	}
	return 0, nil
}

func (f *fakeRunner) lastCallTo(name string) []string {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i][0] == name {
			return f.calls[i]
		}
	}
	return nil
}

func baseConfig(t *testing.T, fr *fakeRunner) Config {
	t.Helper()
	return Config{
		WorkingDir:   t.TempDir(),
		RunID:        "11111111-2222-3333-4444-555555555555",
		AttemptToken: "tok-abc",
		SSHUser:      "hostuser",
		RemoteBinary: `C:/app/host.exe`,
		run:          fr,
	}
}

func TestMintAndConnectHappyPath(t *testing.T) {
	var sshCertReqs, authkeyReqs int
	mint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Glimmung-Attempt-Token") != "tok-abc" {
			t.Errorf("missing attempt token header on %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/ssh-cert":
			sshCertReqs++
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			if !strings.Contains(req["public_key"], "PUBKEY") {
				t.Errorf("ssh-cert request missing public_key, got %s", body)
			}
			_, _ = w.Write([]byte(`{"certificate":"ssh-ed25519-cert-v01 AAAA"}`))
		case "/tailscale-authkey":
			authkeyReqs++
			if strings.TrimSpace(string(body)) != "{}" {
				t.Errorf("authkey request body = %q, want {}", body)
			}
			_, _ = w.Write([]byte(`{"authkey":"tskey-auth-xyz"}`))
		}
	}))
	defer mint.Close()

	fr := &fakeRunner{outputs: map[string][]byte{
		"tailscale": []byte(`{"Peer":{"n":{"Tags":["tag:spirelens-host"],"TailscaleIPs":["100.64.0.7","fd7a::1"]}}}`),
	}}
	cfg := baseConfig(t, fr)
	cfg.SSHCertURL = mint.URL + "/ssh-cert"
	cfg.TailscaleAuthkeyURL = mint.URL + "/tailscale-authkey"

	// ssh-keygen writes a .pub file the cert mint reads.
	origRunner := fr
	fr2 := &fakeRunnerWithKeygen{fakeRunner: origRunner, pubPath: cfg.keyPath() + ".pub"}
	cfg.run = fr2

	conn, lerr := MintAndConnect(context.Background(), cfg, "tag:spirelens-host")
	if lerr != nil {
		t.Fatalf("MintAndConnect: %v", lerr)
	}
	if conn.HostIP() != "100.64.0.7" {
		t.Fatalf("host ip = %q, want 100.64.0.7", conn.HostIP())
	}
	if sshCertReqs != 1 || authkeyReqs != 1 {
		t.Fatalf("mint reqs ssh-cert=%d authkey=%d, want 1/1", sshCertReqs, authkeyReqs)
	}
	// tailscale up must carry the userspace + authkey + DNS-label hostname.
	up := fr2.lastCallTo("tailscale")
	_ = up
	upCall := findCallWith(fr2.calls, "tailscale", "up")
	if upCall == nil {
		t.Fatal("no tailscale up call")
	}
	joined := strings.Join(upCall, " ")
	if !strings.Contains(joined, "--authkey=tskey-auth-xyz") {
		t.Fatalf("tailscale up missing authkey: %v", upCall)
	}
	if !strings.Contains(joined, "--hostname=glimmung-"+cfg.RunID) {
		t.Fatalf("tailscale up hostname not the run-id DNS label: %v", upCall)
	}
	// ssh_config must route through the userspace proxy.
	sc, err := os.ReadFile(cfg.sshConfigPath())
	if err != nil {
		t.Fatalf("read ssh_config: %v", err)
	}
	if !strings.Contains(string(sc), "ProxyCommand tailscale --socket="+cfg.socketPath()+" nc %h %p") {
		t.Fatalf("ssh_config missing tailscale nc ProxyCommand:\n%s", sc)
	}
}

func TestHostUnreachableIsHostLayer(t *testing.T) {
	fr := &fakeRunnerWithKeygen{
		fakeRunner: &fakeRunner{outputs: map[string][]byte{
			"tailscale": []byte(`{"Peer":{}}`), // no peer tagged
		}},
	}
	cfg := baseConfig(t, fr.fakeRunner)
	fr.pubPath = cfg.keyPath() + ".pub"
	cfg.run = fr

	mint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ssh-cert":
			_, _ = w.Write([]byte(`{"certificate":"c"}`))
		default:
			_, _ = w.Write([]byte(`{"authkey":"k"}`))
		}
	}))
	defer mint.Close()
	cfg.SSHCertURL = mint.URL + "/ssh-cert"
	cfg.TailscaleAuthkeyURL = mint.URL + "/authkey"

	_, lerr := MintAndConnect(context.Background(), cfg, "tag:spirelens-host")
	if lerr == nil {
		t.Fatal("expected host-unreachable error")
	}
	if lerr.Layer != steperr.LayerHost {
		t.Fatalf("layer = %q, want host", lerr.Layer)
	}
	if lerr.Code != "host_unreachable" {
		t.Fatalf("code = %q, want host_unreachable", lerr.Code)
	}
}

func TestRunSelfArgs(t *testing.T) {
	fr := &fakeRunner{}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	if lerr := conn.RunSelf(context.Background(), "sync-host", "--ref", "main"); lerr != nil {
		t.Fatalf("RunSelf: %v", lerr)
	}
	call := fr.lastCallTo("ssh")
	joined := strings.Join(call, " ")
	want := "ssh -n -F " + cfg.sshConfigPath() + " hostuser@100.64.0.7 C:/app/host.exe sync-host --ref main"
	if joined != want {
		t.Fatalf("RunSelf args:\n got: %s\nwant: %s", joined, want)
	}
}

func TestScpPushTreeArgs(t *testing.T) {
	fr := &fakeRunner{}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	if lerr := conn.ScpPushTree(context.Background(), "/local/dir", "C:/remote/dir"); lerr != nil {
		t.Fatalf("ScpPushTree: %v", lerr)
	}
	call := fr.lastCallTo("scp")
	joined := strings.Join(call, " ")
	want := "scp -r -F " + cfg.sshConfigPath() + " /local/dir hostuser@100.64.0.7:C:/remote/dir"
	if joined != want {
		t.Fatalf("ScpPushTree args:\n got: %s\nwant: %s", joined, want)
	}
}

func TestScpFailureIsHostLayer(t *testing.T) {
	fr := &fakeRunner{errs: map[string]error{"scp": errTest}}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	lerr := conn.ScpPull(context.Background(), "remote", "local")
	if lerr == nil || lerr.Layer != steperr.LayerHost {
		t.Fatalf("scp failure should be host-layer, got %v", lerr)
	}
}

func TestSyncCheckoutRequiresSHA(t *testing.T) {
	conn := &Conn{cfg: baseConfig(t, &fakeRunner{}), hostIP: "100.64.0.7"}
	if lerr := conn.SyncCheckout(context.Background(), "/repo", "https://github.com/o/r.git", "", "tok"); lerr == nil {
		t.Fatal("empty sha must be rejected")
	}
}

// errTest is a sentinel used to force runner errors.
var errTest = io.ErrUnexpectedEOF

// fakeRunnerWithKeygen writes a fake .pub file when ssh-keygen is invoked so
// the cert mint can read it.
type fakeRunnerWithKeygen struct {
	*fakeRunner
	pubPath string
}

func (f *fakeRunnerWithKeygen) run(ctx context.Context, dir string, stdin []byte, name string, args ...string) ([]byte, error) {
	if name == "ssh-keygen" {
		_ = os.WriteFile(f.pubPath, []byte("ssh-ed25519 AAAAPUBKEY glimmung\n"), 0o644)
	}
	return f.fakeRunner.run(ctx, dir, stdin, name, args...)
}

// recordingWriter captures each Write as a separate entry so a test can assert
// the stream is delivered line-by-line, not coalesced into one blob.
type recordingWriter struct{ writes []string }

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.writes = append(r.writes, string(p)) // string() copies the bytes out of p
	return len(p), nil
}

// TestRunSelfStreamsStdoutLineUnbuffered proves gap #1: the remote process's
// stdout reaches the pod step's stdout one line at a time, in order, so an
// agentcost-style reader prices each forwarded `usage` line across the ssh hop.
func TestRunSelfStreamsStdoutLineUnbuffered(t *testing.T) {
	usageLine := `{"type":"result","usage":{"input_tokens":1000000,"output_tokens":500000}}`
	remoteOut := "starting phase\n" + usageLine + "\nphase complete\n"
	fr := &fakeRunner{streamOut: map[string][]byte{"ssh": []byte(remoteOut)}}
	rec := &recordingWriter{}
	cfg := baseConfig(t, fr)
	cfg.Stdout = rec
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}

	if lerr := conn.RunSelf(context.Background(), "run-phase", "--phase", "verification"); lerr != nil {
		t.Fatalf("RunSelf: %v", lerr)
	}
	// Each line is forwarded as its own write, in order — never merged or split.
	want := []string{"starting phase\n", usageLine + "\n", "phase complete\n"}
	if !reflect.DeepEqual(rec.writes, want) {
		t.Fatalf("stream not delivered line-by-line:\n got: %q\nwant: %q", rec.writes, want)
	}
	// The forwarded usage line must be priceable: a swallowed/buffered stdout
	// would defeat the $0-cost misattribution guard across the hop.
	rate := agentcost.Rate{CatalogRef: "c", InputPerMillionUSD: 3, CachedInputPerMillionUSD: 0.3, OutputPerMillionUSD: 15}
	var priced float64
	for _, line := range rec.writes {
		obs, ok, err := agentcost.FromJSONLogLine(line, rate)
		if ok && err == nil {
			priced += obs.CostUSD
		}
	}
	if priced <= 0 {
		t.Fatalf("forwarded usage line was not priceable (cost=%v); $0-cost guard defeated across the ssh hop", priced)
	}
}

// TestRunSelfStreamsTrailingPartialLine proves a final line without a newline
// is still forwarded (flushed) rather than stranded in the line buffer.
func TestRunSelfStreamsTrailingPartialLine(t *testing.T) {
	fr := &fakeRunner{streamOut: map[string][]byte{"ssh": []byte("line1\nno-newline-tail")}}
	rec := &recordingWriter{}
	cfg := baseConfig(t, fr)
	cfg.Stdout = rec
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	if lerr := conn.RunSelf(context.Background(), "probe", "--out", "x"); lerr != nil {
		t.Fatalf("RunSelf: %v", lerr)
	}
	want := []string{"line1\n", "no-newline-tail"}
	if !reflect.DeepEqual(rec.writes, want) {
		t.Fatalf("trailing partial line not flushed:\n got: %q\nwant: %q", rec.writes, want)
	}
}

// TestRunSelfNonZeroExitIsHostLayer proves the honest exit-code contract is
// preserved: a non-zero remote exit surfaces as a host-layer error carrying the
// code.
func TestRunSelfNonZeroExitIsHostLayer(t *testing.T) {
	fr := &fakeRunner{
		errs:       map[string]error{"ssh": errTest},
		streamCode: map[string]int{"ssh": 7},
	}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	lerr := conn.RunSelf(context.Background(), "run-phase")
	if lerr == nil || lerr.Layer != steperr.LayerHost {
		t.Fatalf("non-zero remote exit must be host-layer, got %v", lerr)
	}
	if !strings.Contains(lerr.Message, "exit 7") {
		t.Fatalf("error should carry the remote exit code, got %q", lerr.Message)
	}
}

// idempotencyRunner simulates a tailnet that stays up across steps: once
// `tailscale up` runs, `tailscale status` reports BackendState Running so a
// per-step reconnect reuses the daemon, and ssh-keygen writes a keypair so a
// second connect reuses it on disk.
type idempotencyRunner struct {
	*fakeRunner
	keyPath          string
	up               bool
	keygens          int
	upCalls          int
	tailscaledStarts int
}

func (r *idempotencyRunner) run(_ context.Context, _ string, _ []byte, name string, args ...string) ([]byte, error) {
	r.fakeRunner.calls = append(r.fakeRunner.calls, append([]string{name}, args...))
	switch name {
	case "ssh-keygen":
		r.keygens++
		_ = os.WriteFile(r.keyPath, []byte("PRIV"), 0o600)
		_ = os.WriteFile(r.keyPath+".pub", []byte("ssh-ed25519 AAAAPUBKEY glimmung\n"), 0o644)
		return nil, nil
	case "tailscale":
		if hasArg(args, "up") {
			r.upCalls++
			r.up = true
			return nil, nil
		}
		if r.up {
			return []byte(`{"BackendState":"Running","Peer":{"n":{"Tags":["tag:spirelens-host"],"TailscaleIPs":["100.64.0.7"]}}}`), nil
		}
		return []byte(`{"BackendState":"Stopped","Peer":{}}`), nil
	}
	return nil, nil
}

func (r *idempotencyRunner) runBackground(ctx context.Context, name string, args ...string) (int, error) {
	if name == "tailscaled" {
		r.tailscaledStarts++
	}
	return r.fakeRunner.runBackground(ctx, name, args...)
}

func hasArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

// TestMintAndConnectIsStepIdempotent proves gap #2: a second connect within the
// same pod reuses the keypair and the running tailscaled (no re-keygen, no
// second tailscaled, no fresh authkey, no re-`up`) and only refreshes the
// short-TTL ssh cert.
func TestMintAndConnectIsStepIdempotent(t *testing.T) {
	var sshCertReqs, authkeyReqs int
	mint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ssh-cert":
			sshCertReqs++
			_, _ = w.Write([]byte(`{"certificate":"ssh-ed25519-cert-v01 AAAA"}`))
		case "/authkey":
			authkeyReqs++
			_, _ = w.Write([]byte(`{"authkey":"tskey-auth-xyz"}`))
		}
	}))
	defer mint.Close()

	base := &fakeRunner{}
	cfg := baseConfig(t, base)
	cfg.SSHCertURL = mint.URL + "/ssh-cert"
	cfg.TailscaleAuthkeyURL = mint.URL + "/authkey"
	ir := &idempotencyRunner{fakeRunner: base, keyPath: cfg.keyPath()}
	cfg.run = ir

	for i := 0; i < 2; i++ {
		conn, lerr := MintAndConnect(context.Background(), cfg, "tag:spirelens-host")
		if lerr != nil {
			t.Fatalf("MintAndConnect call %d: %v", i+1, lerr)
		}
		if conn.HostIP() != "100.64.0.7" {
			t.Fatalf("call %d host ip = %q, want 100.64.0.7", i+1, conn.HostIP())
		}
	}

	if ir.keygens != 1 {
		t.Fatalf("ssh-keygen ran %d times across two connects, want 1 (keypair reused)", ir.keygens)
	}
	if ir.tailscaledStarts != 1 {
		t.Fatalf("tailscaled started %d times, want 1 (running daemon reused, no socket conflict)", ir.tailscaledStarts)
	}
	if ir.upCalls != 1 {
		t.Fatalf("tailscale up ran %d times, want 1 (node already up on the second connect)", ir.upCalls)
	}
	if authkeyReqs != 1 {
		t.Fatalf("tailscale authkey minted %d times, want 1 (only on bring-up)", authkeyReqs)
	}
	if sshCertReqs != 2 {
		t.Fatalf("ssh cert minted %d times, want 2 (short-TTL cert refreshed every step)", sshCertReqs)
	}
}

func TestScpPushArgs(t *testing.T) {
	fr := &fakeRunner{}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	if lerr := conn.ScpPush(context.Background(), "/local/file.exe", "C:/remote/file.exe"); lerr != nil {
		t.Fatalf("ScpPush: %v", lerr)
	}
	joined := strings.Join(fr.lastCallTo("scp"), " ")
	want := "scp -F " + cfg.sshConfigPath() + " /local/file.exe hostuser@100.64.0.7:C:/remote/file.exe"
	if joined != want {
		t.Fatalf("ScpPush args:\n got: %s\nwant: %s", joined, want)
	}
}

func TestScpPullTreeArgs(t *testing.T) {
	fr := &fakeRunner{}
	cfg := baseConfig(t, fr)
	conn := &Conn{cfg: cfg, hostIP: "100.64.0.7"}
	if lerr := conn.ScpPullTree(context.Background(), "C:/remote/dir", "/local/dir"); lerr != nil {
		t.Fatalf("ScpPullTree: %v", lerr)
	}
	joined := strings.Join(fr.lastCallTo("scp"), " ")
	want := "scp -r -F " + cfg.sshConfigPath() + " hostuser@100.64.0.7:C:/remote/dir /local/dir"
	if joined != want {
		t.Fatalf("ScpPullTree args:\n got: %s\nwant: %s", joined, want)
	}
}

func findCallWith(calls [][]string, name string, contains string) []string {
	for _, call := range calls {
		if call[0] != name {
			continue
		}
		for _, a := range call {
			if strings.Contains(a, contains) {
				return call
			}
		}
	}
	return nil
}

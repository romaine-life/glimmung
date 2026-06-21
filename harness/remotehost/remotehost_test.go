package remotehost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// fakeRunner records invocations and returns canned output per command name.
type fakeRunner struct {
	calls   [][]string
	outputs map[string][]byte
	errs    map[string]error
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

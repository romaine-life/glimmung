// Package remotehost is the typed Go port of spirelens'
// scripts/glimmung-native/lib.sh remote-host venue: mint an ssh certificate
// and a tailscale auth key from the run callbacks, bring up tailscaled in
// userspace mode, dial the capability-matched host through `tailscale nc`, and
// run the SAME binary's host face over ssh instead of a pwsh-over-ssh here-doc.
//
// Every failure to reach or set up the venue is attributed to the host layer
// (step.HostError → steperr.LayerHost): an unreachable or misconfigured host
// is never the change under test's fault, and never a model failure. The
// package shells out to the ssh/scp/tailscale/ssh-keygen/curl binaries already
// present in the runner image; those calls go through an injectable runner so
// the arg construction and mint shapes are testable without a real tailnet.
package remotehost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/harness/step"
)

func basicAuth(user, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
}

// Config is the venue wiring, normally built from the GLIMMUNG_* environment
// via FromContext.
type Config struct {
	WorkingDir          string // GLIMMUNG_WORKING_DIR — holds keys, the tailscaled socket, ssh_config
	RunID               string // GLIMMUNG_RUN_ID — the tailscale hostname label (a valid DNS label)
	SSHCertURL          string // GLIMMUNG_SSH_CERT_URL
	TailscaleAuthkeyURL string // GLIMMUNG_TAILSCALE_AUTHKEY_URL
	AttemptToken        string // GLIMMUNG_ATTEMPT_TOKEN — X-Glimmung-Attempt-Token header
	SSHUser             string // remote ssh user
	RemoteBinary        string // path of the app binary's host face on the remote host (for RunSelf)
	HTTPClient          *http.Client
	// run executes a local command; nil uses the real os/exec runner.
	run commandRunner
}

// FromContext builds a Config from a step.Context's GLIMMUNG_* environment.
func FromContext(c *step.Context, sshUser, remoteBinary string) Config {
	return Config{
		WorkingDir:          c.WorkingDir(),
		RunID:               c.RunID(),
		SSHCertURL:          c.Env("GLIMMUNG_SSH_CERT_URL"),
		TailscaleAuthkeyURL: c.Env("GLIMMUNG_TAILSCALE_AUTHKEY_URL"),
		AttemptToken:        c.Env("GLIMMUNG_ATTEMPT_TOKEN"),
		SSHUser:             sshUser,
		RemoteBinary:        remoteBinary,
	}
}

func (cfg Config) httpClient() *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (cfg Config) runner() commandRunner {
	if cfg.run != nil {
		return cfg.run
	}
	return execRunner{}
}

func (cfg Config) socketPath() string    { return filepath.Join(cfg.WorkingDir, "ts.sock") }
func (cfg Config) stateDir() string      { return filepath.Join(cfg.WorkingDir, "ts") }
func (cfg Config) keyPath() string       { return filepath.Join(cfg.WorkingDir, "id_ed25519") }
func (cfg Config) certPath() string      { return filepath.Join(cfg.WorkingDir, "id_ed25519-cert.pub") }
func (cfg Config) sshConfigPath() string { return filepath.Join(cfg.WorkingDir, "ssh_config") }

// Conn is an established connection to a remote host through the tailnet.
type Conn struct {
	cfg    Config
	hostIP string
}

// HostIP returns the tailnet IP the connection dials.
func (c *Conn) HostIP() string { return c.hostIP }

// MintAndConnect performs the full venue bring-up: generate a keypair, mint the
// ssh certificate and tailscale auth key, start tailscaled in userspace mode,
// bring the node up, resolve the host tagged hostTag, and write the ssh_config
// used by every subsequent RunSelf/Scp call. Any failure is a host-layer error.
func MintAndConnect(ctx context.Context, cfg Config, hostTag string) (*Conn, *step.LayeredError) {
	if strings.TrimSpace(cfg.WorkingDir) == "" {
		return nil, step.HostError("remotehost_misconfigured", "GLIMMUNG_WORKING_DIR is required for remote-host execution", nil)
	}
	if err := os.MkdirAll(cfg.WorkingDir, 0o755); err != nil {
		return nil, step.HostError("remotehost_workdir", "create working dir", err)
	}
	if err := cfg.generateKeypair(ctx); err != nil {
		return nil, err
	}
	if err := cfg.mintSSHCert(ctx); err != nil {
		return nil, err
	}
	authkey, err := cfg.mintTailscaleAuthkey(ctx)
	if err != nil {
		return nil, err
	}
	if err := cfg.tailscaleUp(ctx, authkey); err != nil {
		return nil, err
	}
	hostIP, err := cfg.hostIP(ctx, hostTag)
	if err != nil {
		return nil, err
	}
	if err := cfg.writeSSHConfig(); err != nil {
		return nil, err
	}
	return &Conn{cfg: cfg, hostIP: hostIP}, nil
}

func (cfg Config) generateKeypair(ctx context.Context) *step.LayeredError {
	// Remove any stale key so ssh-keygen does not prompt to overwrite.
	_ = os.Remove(cfg.keyPath())
	_ = os.Remove(cfg.keyPath() + ".pub")
	if _, err := cfg.runner().run(ctx, "", nil, "ssh-keygen", "-t", "ed25519", "-N", "", "-f", cfg.keyPath()); err != nil {
		return step.HostError("ssh_keygen", "generate ssh keypair", err)
	}
	return nil
}

func (cfg Config) mintSSHCert(ctx context.Context) *step.LayeredError {
	pub, err := os.ReadFile(cfg.keyPath() + ".pub")
	if err != nil {
		return step.HostError("ssh_pubkey", "read generated public key", err)
	}
	body, _ := json.Marshal(map[string]string{"public_key": strings.TrimSpace(string(pub))})
	var resp struct {
		Certificate string `json:"certificate"`
	}
	if lerr := cfg.postJSON(ctx, cfg.SSHCertURL, body, &resp); lerr != nil {
		return lerr
	}
	if strings.TrimSpace(resp.Certificate) == "" {
		return step.HostError("ssh_cert", "ssh-cert response did not include a certificate", nil)
	}
	if err := os.WriteFile(cfg.certPath(), []byte(resp.Certificate+"\n"), 0o644); err != nil {
		return step.HostError("ssh_cert", "write ssh certificate", err)
	}
	return nil
}

func (cfg Config) mintTailscaleAuthkey(ctx context.Context) (string, *step.LayeredError) {
	var resp struct {
		Authkey string `json:"authkey"`
	}
	if lerr := cfg.postJSON(ctx, cfg.TailscaleAuthkeyURL, []byte(`{}`), &resp); lerr != nil {
		return "", lerr
	}
	if strings.TrimSpace(resp.Authkey) == "" {
		return "", step.HostError("tailscale_authkey", "tailscale-authkey response did not include an authkey", nil)
	}
	return strings.TrimSpace(resp.Authkey), nil
}

func (cfg Config) tailscaleUp(ctx context.Context, authkey string) *step.LayeredError {
	if err := os.MkdirAll(cfg.stateDir(), 0o755); err != nil {
		return step.HostError("tailscale_statedir", "create tailscale state dir", err)
	}
	// Start tailscaled in userspace mode (the pod has no NET_ADMIN/dev/net/tun).
	if _, err := cfg.runner().runBackground(ctx, "tailscaled",
		"--tun=userspace-networking",
		"--statedir="+cfg.stateDir(),
		"--socket="+cfg.socketPath(),
	); err != nil {
		return step.HostError("tailscaled_start", "start tailscaled", err)
	}
	// Bring the node up. Ephemerality is a property of the minted authkey, not
	// a CLI flag; the hostname must be a valid DNS label (RunID is a UUID).
	if _, err := cfg.runner().run(ctx, "", nil, "tailscale",
		"--socket="+cfg.socketPath(), "up",
		"--authkey="+authkey,
		"--hostname=glimmung-"+cfg.RunID,
		"--accept-routes=false",
		"--accept-dns=false",
	); err != nil {
		return step.HostError("tailscale_up", "tailscale up", err)
	}
	return nil
}

// hostIP resolves the single tailnet 100.x peer tagged hostTag. Zero or
// multiple matches is a host-layer error — the venue is ambiguous.
func (cfg Config) hostIP(ctx context.Context, hostTag string) (string, *step.LayeredError) {
	out, err := cfg.runner().run(ctx, "", nil, "tailscale", "--socket="+cfg.socketPath(), "status", "--json")
	if err != nil {
		return "", step.HostError("tailscale_status", "query tailscale status", err)
	}
	var status struct {
		Peer map[string]struct {
			Tags         []string `json:"Tags"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", step.HostError("tailscale_status", "parse tailscale status json", err)
	}
	matches := []string{}
	for _, peer := range status.Peer {
		if !containsString(peer.Tags, hostTag) {
			continue
		}
		for _, ip := range peer.TailscaleIPs {
			if strings.HasPrefix(ip, "100.") {
				matches = append(matches, ip)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", step.HostError("host_unreachable", fmt.Sprintf("no tailnet host tagged %q", hostTag), nil)
	default:
		return "", step.HostError("host_ambiguous", fmt.Sprintf("multiple tailnet hosts tagged %q: %s", hostTag, strings.Join(matches, ", ")), nil)
	}
}

// writeSSHConfig emits the ssh_config that routes ssh/scp through the userspace
// tailscale proxy. The ProxyCommand value has spaces, so it must live in a
// config file rather than inline -o flags.
func (cfg Config) writeSSHConfig() *step.LayeredError {
	content := strings.Join([]string{
		"Host *",
		"  IdentityFile " + cfg.keyPath(),
		"  CertificateFile " + cfg.certPath(),
		"  UserKnownHostsFile /dev/null",
		"  StrictHostKeyChecking accept-new",
		"  LogLevel ERROR",
		"  ProxyCommand tailscale --socket=" + cfg.socketPath() + " nc %h %p",
		"",
	}, "\n")
	if err := os.WriteFile(cfg.sshConfigPath(), []byte(content), 0o644); err != nil {
		return step.HostError("ssh_config", "write ssh config", err)
	}
	return nil
}

// sshArgs returns the leading ssh/scp args (the -F config flag).
func (cfg Config) sshArgs() []string { return []string{"-F", cfg.sshConfigPath()} }

// RunSelf runs the same binary's host face on the remote host over ssh:
// `<RemoteBinary> <subcmd> <args...>`. It replaces the pwsh-over-ssh here-doc.
func (c *Conn) RunSelf(ctx context.Context, subcmd string, args ...string) *step.LayeredError {
	if strings.TrimSpace(c.cfg.RemoteBinary) == "" {
		return step.HostError("remotehost_misconfigured", "RemoteBinary is not configured for RunSelf", nil)
	}
	remoteCmd := append([]string{c.cfg.RemoteBinary, subcmd}, args...)
	full := append(append([]string{"-n"}, c.cfg.sshArgs()...), c.cfg.SSHUser+"@"+c.hostIP)
	full = append(full, remoteCmd...)
	if _, err := c.cfg.runner().run(ctx, "", nil, "ssh", full...); err != nil {
		return step.HostError("ssh_run", fmt.Sprintf("ssh-run %q on host", subcmd), err)
	}
	return nil
}

// ScpPull copies a single remote file to a local path.
func (c *Conn) ScpPull(ctx context.Context, remote, local string) *step.LayeredError {
	args := append(c.cfg.sshArgs(), c.cfg.SSHUser+"@"+c.hostIP+":"+remote, local)
	if _, err := c.cfg.runner().run(ctx, "", nil, "scp", args...); err != nil {
		return step.HostError("scp_pull", "scp pull from host", err)
	}
	return nil
}

// ScpPushTree recursively copies a local directory tree to the remote host.
func (c *Conn) ScpPushTree(ctx context.Context, localDir, remoteDir string) *step.LayeredError {
	args := append([]string{"-r"}, c.cfg.sshArgs()...)
	args = append(args, localDir, c.cfg.SSHUser+"@"+c.hostIP+":"+remoteDir)
	if _, err := c.cfg.runner().run(ctx, "", nil, "scp", args...); err != nil {
		return step.HostError("scp_push", "scp push tree to host", err)
	}
	return nil
}

// SyncCheckout fetches and hard-resets the remote checkout at repoPath to an
// exact commit SHA from remoteURL, authenticating with githubToken via a
// per-invocation http.extraheader (never written to the remote git config).
// It runs the git sequence over ssh through RunCommand so the remote always
// matches the pod's exact commit.
func (c *Conn) SyncCheckout(ctx context.Context, repoPath, remoteURL, sha, githubToken string) *step.LayeredError {
	if strings.TrimSpace(sha) == "" {
		return step.HostError("sync_checkout", "sync target commit is empty", nil)
	}
	header := "AUTHORIZATION: basic " + basicAuth("x-access-token", githubToken)
	steps := [][]string{
		{"git", "-C", repoPath, "remote", "set-url", "origin", remoteURL},
		{"git", "-C", repoPath, "-c", "http." + remoteURL + ".extraheader=" + header, "fetch", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"},
		{"git", "-C", repoPath, "cat-file", "-e", sha + "^{commit}"},
		{"git", "-C", repoPath, "checkout", "--force", "--detach", sha},
		{"git", "-C", repoPath, "reset", "--hard", sha},
	}
	for _, s := range steps {
		if err := c.RunCommand(ctx, s...); err != nil {
			return step.HostError("sync_checkout", "remote git "+strings.Join(s[3:], " "), err)
		}
	}
	return nil
}

// RunCommand runs an arbitrary command on the remote host over ssh.
func (c *Conn) RunCommand(ctx context.Context, command ...string) error {
	full := append(append([]string{"-n"}, c.cfg.sshArgs()...), c.cfg.SSHUser+"@"+c.hostIP)
	full = append(full, command...)
	_, err := c.cfg.runner().run(ctx, "", nil, "ssh", full...)
	return err
}

func (cfg Config) postJSON(ctx context.Context, url string, body []byte, out any) *step.LayeredError {
	if strings.TrimSpace(url) == "" {
		return step.HostError("remotehost_misconfigured", "mint URL is not configured", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return step.HostError("mint_request", "build mint request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.AttemptToken != "" {
		req.Header.Set("X-Glimmung-Attempt-Token", cfg.AttemptToken)
	}
	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return step.HostError("mint_request", "mint request failed", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode >= 400 {
		return step.HostError("mint_request", fmt.Sprintf("mint %s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(buf.String())), nil)
	}
	if out != nil && buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			return step.HostError("mint_request", "decode mint response", err)
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// commandRunner abstracts local command execution for testability.
type commandRunner interface {
	run(ctx context.Context, dir string, stdin []byte, name string, args ...string) ([]byte, error)
	runBackground(ctx context.Context, name string, args ...string) (int, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (execRunner) runBackground(ctx context.Context, name string, args ...string) (int, error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

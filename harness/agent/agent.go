// Package agent runs an agent CLI (claude, codex, …) as a child process on
// behalf of an SDK-driven workflow step. It is the ONLY place in the harness
// SDK where a model-layer failure may originate: Invoke returns a
// *step.LayeredError with Layer == steperr.LayerModel when the agent process
// fails to run, and never anything else. A harness or host failure is a
// structurally different code path (the caller returns a harness/host
// LayeredError directly), so a crash before the model runs can never be
// mislabelled as a model failure.
//
// Invoke streams the agent's stdout LINE-UNBUFFERED to the forwarding sink
// (the runner's log stream by default). Preserving line boundaries is
// load-bearing: the runner's streamLogs prices each agent `usage` JSON line
// through internal/domain/agentcost.FromJSONLogLine exactly as it does for a
// shell-launched agent today. Merging or splitting lines would strand the
// usage object and reintroduce the $0-cost bug, so Invoke writes each line as
// a single write and never buffers across the whole stream.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/romaine-life/glimmung/harness/step"
	"github.com/romaine-life/glimmung/internal/domain/agentcost"
)

// Spec describes one agent invocation.
type Spec struct {
	// Name is the agent executable (e.g. "claude", "codex").
	Name string
	// Args are the executable arguments.
	Args []string
	// Dir is the working directory; empty uses the current directory.
	Dir string
	// Stdin, when set, is piped to the agent (e.g. the prompt).
	Stdin io.Reader
	// Env is the child environment; nil uses the parent's os.Environ().
	Env []string
	// Pricing is the resolved rate used to price observed `usage` lines into
	// the returned Outcome. The zero value disables in-process pricing (the
	// runner's stream still prices the forwarded lines).
	Pricing agentcost.Rate
	// Stdout/Stderr are the forwarding sinks. nil defaults to os.Stdout /
	// os.Stderr — i.e. straight into the runner's captured log stream.
	Stdout io.Writer
	Stderr io.Writer
}

// Outcome reports whether the model RAN and what it cost. It deliberately
// carries no verification verdict — that belongs to the glimmung-owned
// verification_finalize primitive, never an agent invocation.
type Outcome struct {
	// Ran is true once the agent process started (regardless of exit code).
	Ran bool
	// ExitCode is the agent process exit code (0 on success).
	ExitCode int
	// Usage is the priced usage observations parsed from the agent's stdout
	// (empty when Spec.Pricing is the zero value).
	Usage []agentcost.Observation
	// CostUSD is the summed cost of Usage.
	CostUSD float64
	// LastStdoutLine is the final non-empty stdout line, a convenience for
	// callers that surface the agent's last message.
	LastStdoutLine string
}

// Invoke runs the agent described by spec to completion. It returns a
// model-layer *step.LayeredError if the process fails to start or exits
// non-zero, and nil on a clean exit. The returned Outcome is always populated
// (Ran reflects whether the process started).
func Invoke(ctx context.Context, spec Spec) (Outcome, *step.LayeredError) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return Outcome{}, step.ModelError("agent_misconfigured", "agent Spec.Name is empty", nil)
	}
	stdoutSink := spec.Stdout
	if stdoutSink == nil {
		stdoutSink = os.Stdout
	}
	stderrSink := spec.Stderr
	if stderrSink == nil {
		stderrSink = os.Stderr
	}

	cmd := exec.CommandContext(ctx, name, spec.Args...)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	if spec.Stdin != nil {
		cmd.Stdin = spec.Stdin
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{}, step.ModelError("agent_pipe_failed", fmt.Sprintf("open agent %q stdout pipe", name), err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Outcome{}, step.ModelError("agent_pipe_failed", fmt.Sprintf("open agent %q stderr pipe", name), err)
	}

	if err := cmd.Start(); err != nil {
		return Outcome{}, step.ModelError("agent_start_failed", fmt.Sprintf("start agent %q", name), err)
	}

	out := Outcome{Ran: true}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	// stdout: forward line-unbuffered and price usage lines.
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			// One write per line preserves the boundary the runner's stream
			// parser relies on to price each usage object.
			_, _ = io.WriteString(stdoutSink, line+"\n")
			if strings.TrimSpace(line) != "" {
				mu.Lock()
				out.LastStdoutLine = line
				mu.Unlock()
			}
			if spec.Pricing.CatalogRef == "" {
				continue
			}
			obs, observed, perr := agentcost.FromJSONLogLine(line, spec.Pricing)
			if !observed || perr != nil {
				continue
			}
			mu.Lock()
			out.Usage = append(out.Usage, obs)
			out.CostUSD += obs.CostUSD
			mu.Unlock()
		}
	}()

	// stderr: forward line-unbuffered.
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			_, _ = io.WriteString(stderrSink, scanner.Text()+"\n")
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	if waitErr == nil {
		out.ExitCode = 0
		return out, nil
	}
	var exitErr *exec.ExitError
	if asExit(waitErr, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		return out, step.ModelError(
			"agent_exit_nonzero",
			fmt.Sprintf("agent %q exited with code %d", name, out.ExitCode),
			waitErr,
		)
	}
	out.ExitCode = 1
	return out, step.ModelError("agent_run_failed", fmt.Sprintf("agent %q failed", name), waitErr)
}

func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

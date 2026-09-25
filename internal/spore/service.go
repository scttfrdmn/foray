// Copyright 2026 Scott Friedman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Server wraps `spawn service` — spawn's verb for running a long-lived HTTP
// service on an instance and tunneling to it (spawn#409). This is how foray
// reaches the worker (issue #66).
//
// The posture this buys is the reason to use it rather than open a port: the
// service binds the instance's **loopback** and is reachable only through an
// ssh -L forward, so the worker is never exposed to the internet, and no VPC,
// interface endpoint or NAT is involved — the control plane stays at ~$0. An open
// security-group port plus a bearer token would be strictly weaker for the same
// money, and would reimplement what spawn already does (CLAUDE.md §"Reuse").
type Server interface {
	// Serve starts the service on an already-running instance and opens the
	// tunnel. It returns once the service has announced itself ready; the caller
	// must Stop the returned Service to close the tunnel.
	Serve(ctx context.Context, spec ServeSpec) (*Service, error)
}

// ServeSpec is one worker's service invocation.
type ServeSpec struct {
	// InstanceID is the instance to run on (--host). It must already be running:
	// foray launches the GPU through the brain's executor on Go, so the instance
	// exists before the tunnel does. spawn does NOT terminate an instance it was
	// merely given, which is what we want — foray owns that lifecycle.
	InstanceID string

	// Command is the service argv. spawn shell-quotes it and appends the listen
	// address (see AddrArgs), so env can ride in front of the interpreter, e.g.
	// {"env", "FORAY_MODEL_URI=gpt2", "python3", "-m", "worker.serve"}.
	Command []string

	// LocalPort is the local end of the forward; 0 lets spawn pick a free one,
	// which is the right default when several sessions may overlap.
	LocalPort int

	// BootTimeout bounds the wait for the readiness line. Weight streaming (GDS)
	// dominates it, so it is generous by default (DefaultServeBootTimeout).
	BootTimeout time.Duration
}

// Service is a live tunnel to a ready worker.
type Service struct {
	// URL is where the worker accepts requests, as spawn reports it (local_url).
	// It is a loopback address and carries the access token in its query when the
	// worker requires one — gateway.HTTPWorker splits those apart.
	URL string
	// Addr is the same endpoint without the credential, for logging.
	Addr string
	// InstanceID echoes the instance the service runs on.
	InstanceID string

	stop func() error
}

// Stop closes the tunnel and ends the remote service. Safe to call more than
// once. Per spawn's own caveat, stopping is a request rather than a guarantee —
// the instance's TTL is the guarantee, which is why foray always launches with
// one.
func (s *Service) Stop() error {
	if s == nil || s.stop == nil {
		return nil
	}
	return s.stop()
}

// DefaultServeBootTimeout bounds the wait for the worker's readiness line. The
// dominant cost is streaming weights S3→HBM on boot, so this is minutes, not
// seconds.
const DefaultServeBootTimeout = 10 * time.Minute

// serviceResult is the JSON `spawn service -o json` prints on one line when the
// service is ready (spawn cmd/service.go reportServiceReady). spawn keeps the
// workload's own chatter on stderr so this stays parseable.
type serviceResult struct {
	InstanceID string `json:"instance_id"`
	Region     string `json:"region"`
	LocalURL   string `json:"local_url"`
	LocalAddr  string `json:"local_addr"`
	RemoteAddr string `json:"remote_addr"`
	TTL        string `json:"ttl,omitempty"`
}

// Proc is a started long-lived child process. `spawn service` holds the tunnel
// open until it is stopped, so it cannot go through Runner (which waits for exit
// and returns the whole output). This is the streaming equivalent of that seam:
// real implementation over os/exec, stub in tests.
type Proc interface {
	// Stdout is the process's stdout, read incrementally.
	Stdout() io.Reader
	// Stop asks the process to exit and releases its resources.
	Stop() error
}

// Starter starts a long-lived process. Injected so the service adapter is
// testable without spawn, ssh, or an instance.
type Starter interface {
	Start(ctx context.Context, name string, args ...string) (Proc, error)
}

// server is the real adapter over the spawn binary.
type server struct{ start Starter }

// NewServer returns a Server backed by the real spawn binary.
func NewServer(s Starter) Server { return server{start: s} }

// ErrServiceNotReady is returned when spawn exits, or the boot timeout expires,
// before the worker announces itself.
var ErrServiceNotReady = errors.New("spore: service never became ready")

func (sv server) Serve(ctx context.Context, spec ServeSpec) (*Service, error) {
	if spec.InstanceID == "" || len(spec.Command) == 0 {
		return nil, fmt.Errorf("spawn service: InstanceID and Command are required")
	}
	boot := spec.BootTimeout
	if boot <= 0 {
		boot = DefaultServeBootTimeout
	}

	// spawn's own flags go first and the service command after a "--" terminator.
	// spawn does not disable flag interspersion, so a command carrying a dash
	// argument — `python3 -m worker.serve`, exactly our case — would otherwise be
	// parsed as spawn's own flags and rejected ("unknown shorthand flag: 'm'").
	args := []string{"service", "--host", spec.InstanceID, "-o", "json"}
	if spec.LocalPort > 0 {
		args = append(args, "--local-port", strconv.Itoa(spec.LocalPort))
	}
	args = append(args, "--boot-timeout", durStr(boot), "--")
	args = append(args, spec.Command...)

	// The child outlives this call, so it must not be bound to a ctx that ends
	// with it. Its lifetime is the returned Service's — Stop() ends it.
	proc, err := sv.start.Start(context.WithoutCancel(ctx), "spawn", args...)
	if err != nil {
		return nil, fmt.Errorf("spawn service: %w", err)
	}

	res, err := awaitServiceReady(ctx, proc.Stdout(), boot)
	if err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("spawn service on %s: %w", spec.InstanceID, err)
	}
	if res.LocalURL == "" {
		_ = proc.Stop()
		return nil, fmt.Errorf("spawn service on %s: %w: no local_url in result", spec.InstanceID, ErrServiceNotReady)
	}
	return &Service{
		URL:        res.LocalURL,
		Addr:       res.LocalAddr,
		InstanceID: orDefault(res.InstanceID, spec.InstanceID),
		stop:       proc.Stop,
	}, nil
}

// awaitServiceReady reads stdout until spawn's one-line JSON result arrives.
//
// Non-JSON lines are skipped rather than treated as failure: spawn documents
// stdout as reserved for the result, but being tolerant here costs nothing and
// means a future banner line does not break foray. Running out of output means
// spawn exited without ever printing a result.
func awaitServiceReady(ctx context.Context, stdout io.Reader, boot time.Duration) (serviceResult, error) {
	type outcome struct {
		res serviceResult
		err error
	}
	done := make(chan outcome, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		// A local_url with a token can be long-ish; give the scanner room so a
		// legitimate result is never truncated into unparseable JSON.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var res serviceResult
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				continue
			}
			done <- outcome{res: res}
			return
		}
		if err := scanner.Err(); err != nil {
			done <- outcome{err: fmt.Errorf("read spawn service output: %w", err)}
			return
		}
		done <- outcome{err: ErrServiceNotReady}
	}()

	timer := time.NewTimer(boot)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.res, o.err
	case <-timer.C:
		return serviceResult{}, fmt.Errorf("%w: no readiness line within %s", ErrServiceNotReady, boot)
	case <-ctx.Done():
		return serviceResult{}, ctx.Err()
	}
}

// --- the real process starter ----------------------------------------------

// execStarter starts real child processes. It mirrors execRunner's LookPath
// behavior so a missing spawn binary reports the same actionable error.
type execStarter struct{}

// NewExecStarter returns a Starter backed by os/exec.
func NewExecStarter() Starter { return execStarter{} }

// execProc is a running child process.
type execProc struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
}

func (p *execProc) Stdout() io.Reader { return p.stdout }

// Stop ends the process. Closing stdin is how spawn's remote wrapper learns to
// kill the service (WrapRemoteCommand kills on stdin EOF), so the kill is the
// blunt backstop, not the first move.
func (p *execProc) Stop() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.stdout.Close()
	// Reap the child so it does not linger as a zombie. The error is the kill's
	// own "signal: killed", which is expected and not worth surfacing.
	_ = p.cmd.Wait()
	return nil
}

func (execStarter) Start(ctx context.Context, name string, args ...string) (Proc, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%w: %s (install it or run with FORAY_FAKE=1)", ErrToolNotFound, name)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdout pipe: %w", name, err)
	}
	// spawn writes its progress and the workload's own output to stderr. Forward
	// it rather than discard it: a boot that never reaches readiness is diagnosed
	// from exactly this, and our stdout stays the user's result surface.
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	return &execProc{cmd: cmd, stdout: stdout}, nil
}

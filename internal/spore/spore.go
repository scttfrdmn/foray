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

// Package spore holds thin adapters over the spore.host binaries — truffle
// (pricing/quota/discovery), spawn (launch/TTL/idle/hibernate), and lagotto
// (capacity watcher). These tools are a dependency, not a thing to rebuild: if
// you find yourself writing an instance launcher or a price fetcher, stop and
// call the tool (CLAUDE.md §"Reuse — do not reimplement", ARCHITECTURE.md §6.6).
//
// # How foray depends on spore.host (resolves issue #41)
//
// We shell out to the installed binaries via os/exec; we do NOT import them as
// Go modules. The spore.host tools are not published as importable modules under
// this account, and the contract is explicitly to "wrap/shell out to them," not
// to link against them. The binaries already speak `-o json`, so the adapter
// surface is: build args, exec, parse JSON. This keeps go.mod free of an
// unpublished dependency and keeps the adapters honestly thin. If the tools are
// ever published as modules, a Runner that calls the library in-process can drop
// in behind the same interfaces without touching callers.
//
// Each tool gets an interface (Truffle, Spawn, Lagotto) with two implementations:
// a real one that execs the CLI and parses its JSON, and a fake (fake.go) that
// returns canned data with zero AWS for FORAY_FAKE=1 — the dev/rehearse path and
// the CI gate, exactly like internal/export and internal/brain.
package spore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrToolNotFound is returned when a spore.host binary is not on PATH. Callers
// surface the wrapped message so the user knows to install it (brew install
// spore.host/tap/<tool>) or to run with FORAY_FAKE=1.
var ErrToolNotFound = errors.New("spore.host tool not found on PATH")

// Runner executes a spore.host binary and returns its stdout. It is the single
// seam between the adapters and the outside world: the real implementation execs
// the CLI; tests inject a stub that returns canned JSON and records the args, so
// the adapters' arg-construction and JSON-parsing are exercised with no AWS and
// no dependency on an installed binary.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner runs the real binary via os/exec. stderr is captured and folded
// into the error so a tool's own diagnostic (a quota message, an auth hint)
// reaches the user verbatim, the way Cedar deny reasons do elsewhere.
type execRunner struct{}

// NewExecRunner returns a Runner backed by os/exec against the installed
// spore.host binaries.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%w: %q (install it, or run with FORAY_FAKE=1)", ErrToolNotFound, name)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, msg)
		}
		return nil, fmt.Errorf("%s %v: %w", name, args, err)
	}
	return stdout.Bytes(), nil
}

// joinCSV renders a region/AZ list as the comma-separated value truffle and
// lagotto expect for --regions.
func joinCSV(xs []string) string { return strings.Join(xs, ",") }

// durStr renders a duration the way the spore.host CLIs accept it (e.g. "8h",
// "5m") — Go's Duration.String, which already emits that form.
func durStr(d time.Duration) string { return d.String() }

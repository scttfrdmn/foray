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

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/sizing"
	"github.com/scttfrdmn/foray/internal/spore"
)

// failingWorker stands in for a worker that rejects the trace, so the failure path
// through runRung can be exercised.
type failingWorker struct{}

func (failingWorker) Run(context.Context, string, gateway.Graph) (gateway.TraceResult, error) {
	return gateway.TraceResult{}, errors.New("graph rejected")
}

// newRungFixture builds the offline deps and launches one instance, returning the
// session id to run a rung against.
func newRungFixture(t *testing.T) (*deps, string) {
	t.Helper()
	t.Setenv("FORAY_FAKE", "1")
	d, err := buildFakeDeps(0)
	if err != nil {
		t.Fatalf("buildFakeDeps: %v", err)
	}
	inst, err := d.spawn.Launch(context.Background(), spore.LaunchSpec{
		Name:         "foray-rung0-gpt2",
		InstanceType: "g7e.xlarge",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return d, inst.ID
}

func stateOf(t *testing.T, d *deps, sid string) string {
	t.Helper()
	inst, err := d.spawn.Status(context.Background(), sid)
	if err != nil {
		t.Fatalf("Status %s: %v", sid, err)
	}
	return inst.State
}

// A finished rung must terminate its instance. spawn's idle timeout only *stops*
// an instance — a stopped instance keeps billing EBS until TTL — so termination is
// what actually reaches $0 (issue #80).
func TestRunRungTerminatesInstance(t *testing.T) {
	d, sid := newRungFixture(t)
	rung := &brain.Rung{Model: sizing.Model{Name: "openai-community/gpt2"}, NNSight: "pass"}

	if _, err := runRung(context.Background(), d, sid, rung, false); err != nil {
		t.Fatalf("runRung: %v", err)
	}
	if got := stateOf(t, d, sid); got != "terminated" {
		t.Errorf("instance state = %q, want terminated — an idle instance keeps billing EBS", got)
	}
}

// The important half: a rung that FAILS must still reap its instance. runLoop
// reports errors through die(), which calls os.Exit and runs no defers, so if
// cleanup did not complete inside runRung a failed trace would leak a GPU until
// TTL.
func TestRunRungTerminatesOnTraceFailure(t *testing.T) {
	d, sid := newRungFixture(t)
	d.tracer.gw.Worker = failingWorker{}
	rung := &brain.Rung{Model: sizing.Model{Name: "openai-community/gpt2"}, NNSight: "pass"}

	if _, err := runRung(context.Background(), d, sid, rung, false); err == nil {
		t.Fatal("want an error from the failing worker")
	}
	if got := stateOf(t, d, sid); got != "terminated" {
		t.Errorf("instance state = %q after a failed trace, want terminated — the GPU would leak", got)
	}
}

// --keep is the escape hatch: leave the box up deliberately. Idle and TTL remain
// the backstops.
func TestRunRungKeepLeavesInstanceRunning(t *testing.T) {
	d, sid := newRungFixture(t)
	rung := &brain.Rung{Model: sizing.Model{Name: "openai-community/gpt2"}, NNSight: "pass"}

	if _, err := runRung(context.Background(), d, sid, rung, true); err != nil {
		t.Fatalf("runRung: %v", err)
	}
	if got := stateOf(t, d, sid); got != "running" {
		t.Errorf("instance state = %q with --keep, want running", got)
	}
}

// The tunnel must be closed by the time the rung returns, on success and on
// failure alike — a forward outliving its session is a leak of a different kind.
func TestRunRungClosesTunnel(t *testing.T) {
	tests := []struct {
		name   string
		worker gateway.Worker
	}{
		{"successful trace", nil},
		{"failed trace", failingWorker{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, sid := newRungFixture(t)
			if tt.worker != nil {
				d.tracer.gw.Worker = tt.worker
			}
			rung := &brain.Rung{Model: sizing.Model{Name: "openai-community/gpt2"}, NNSight: "pass"}
			_, _ = runRung(context.Background(), d, sid, rung, false)

			if d.tracer.svc != nil {
				t.Error("tracer still holds a tunnel after the rung ended")
			}
		})
	}
}

// stdlib flag stops at the first positional, so flags written after the question —
// how a person types it, how the README shows it, and how `make demo-fake` invokes
// it — were silently dropped. `--yes` in particular never took effect; the gate
// passed only because an unreadable stdin read as approval.
func TestParseWithPositionalsAcceptsFlagsAfterTheQuestion(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantQuestion string
		wantYes      bool
		wantKeep     bool
		wantBudget   float64
	}{
		{
			name:         "flags before the question",
			args:         []string{"--yes", "--keep", "why does it refuse X?"},
			wantQuestion: "why does it refuse X?",
			wantYes:      true,
			wantKeep:     true,
		},
		{
			name:         "flags after the question (the regression)",
			args:         []string{"why does it refuse X?", "--yes", "--keep"},
			wantQuestion: "why does it refuse X?",
			wantYes:      true,
			wantKeep:     true,
		},
		{
			name:         "flags on both sides, with a value flag",
			args:         []string{"--budget", "2.50", "why?", "--yes"},
			wantQuestion: "why?",
			wantYes:      true,
			wantBudget:   2.50,
		},
		{
			name:         "question only",
			args:         []string{"why?"},
			wantQuestion: "why?",
		},
		{
			name:         "no arguments at all",
			args:         nil,
			wantQuestion: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			yes := fs.Bool("yes", false, "")
			keep := fs.Bool("keep", false, "")
			budget := fs.Float64("budget", 0, "")

			got := parseWithPositionals(fs, tt.args)
			if got != tt.wantQuestion {
				t.Errorf("question = %q, want %q", got, tt.wantQuestion)
			}
			if *yes != tt.wantYes {
				t.Errorf("--yes = %v, want %v", *yes, tt.wantYes)
			}
			if *keep != tt.wantKeep {
				t.Errorf("--keep = %v, want %v", *keep, tt.wantKeep)
			}
			if *budget != tt.wantBudget {
				t.Errorf("--budget = %v, want %v", *budget, tt.wantBudget)
			}
		})
	}
}

// The Go gate must never read an absent human as an approving one. An unreadable
// stdin returns io.EOF with an empty string — the same value a bare Enter gives —
// so discarding the error made "nobody is there" mean "yes", and an unattended
// `foray run` launched GPUs with no acceptance node (issue #87). CLAUDE.md:
// "the human at Go is the acceptance node".
func TestConfirmRefusesWhenStdinHasNothingToGive(t *testing.T) {
	tests := []struct {
		name  string
		input io.Reader
		want  bool
	}{
		// The money-spending case: no input available at all.
		{"empty reader (closed stdin / no tty)", strings.NewReader(""), false},
		{"eof-ing reader", iotest.ErrReader(io.EOF), false},
		{"read error", iotest.ErrReader(errors.New("stdin broke")), false},

		// A person at a terminal is unaffected: bare Enter still defaults to yes.
		{"bare enter", strings.NewReader("\n"), true},
		{"y", strings.NewReader("y\n"), true},
		{"yes", strings.NewReader("yes\n"), true},
		{"uppercase Y", strings.NewReader("Y\n"), true},
		{"padded yes", strings.NewReader("  yes  \n"), true},

		{"n", strings.NewReader("n\n"), false},
		{"no", strings.NewReader("no\n"), false},
		{"anything else", strings.NewReader("maybe\n"), false},

		// A final line without a trailing newline still counts as an answer: the
		// read errors with EOF, but it returned content, so a human did answer.
		{"answer without trailing newline", strings.NewReader("y"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := confirmFrom(tt.input, "  Go?"); got != tt.want {
				t.Errorf("confirmFrom = %v, want %v", got, tt.want)
			}
		})
	}
}

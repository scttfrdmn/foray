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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Task wraps `spawn task run` — spawn's stage → run → durable-completion-record
// verb, and the right primitive for foray's data plane (issue #103).
//
// foray had been launching a bare instance and hand-rolling the parts around it: an
// S3 handoff for the graph, a `--command` to start the worker, an explicit terminate
// at end of rung. `spawn task run` already does all of that, and three things foray
// did not:
//
//   - **Container delivery.** `container` runs a private-ECR image, and for a GPU
//     instance type spawn auto-selects the GPU DLAMI with NVIDIA drivers. That was
//     the gap that made no real trace runnable: nothing put the worker on the box.
//   - **Inputs staged before exec.** The graph is a local file by the time the
//     container starts, so the worker never waits for its own input.
//   - **A durable completion record written even on failure.** `completion.json` and
//     `.exitcode` land in S3 regardless of exit code, which is how "the container
//     died without producing a result" becomes reportable instead of an endless poll.
//
// CLAUDE.md: the spore.host suite is a dependency, not a thing to rebuild. This is
// that rule applied late — the handoff in internal/gateway was a partial
// reimplementation of this verb.
type Task interface {
	// Run launches the task. The caller chooses spec.TaskID, so the session's identity
	// exists before the instance does — which is what lets the graph be written first.
	Run(ctx context.Context, spec TaskSpec) (TaskRun, error)
	// Status reports the task's durable completion record. Pending until the task
	// finishes, then carries the exit code — including for a failure.
	//
	// The record is written *after* stage-out, which is the ordering that makes it a
	// race-free death signal: if a completion record exists and the declared output does
	// not, the output is never coming. Verified in spawn's wrapper, not assumed — the
	// stage-out block precedes the completion heredoc.
	Status(ctx context.Context, taskID string) (TaskCompletion, error)
}

// TaskSpec is the subset of spawn's TaskSpec foray sets (spawn
// pkg/taskproto/taskspec.go). Mirrored rather than imported because the spore.host
// tools are not published as Go modules under this account and the contract is to
// shell out (see the package doc).
//
// Field names and nesting are spawn's; TestTaskSpecMarshalsToSpawnsContract pins
// them against a spec verified with `spawn task run --dry-run` against a real
// account.
type TaskSpec struct {
	TaskID    string            `json:"task_id"`
	Command   []string          `json:"command"`
	Container string            `json:"container,omitempty"`
	Resources TaskResources     `json:"resources"`
	Inputs    []TaskFile        `json:"inputs,omitempty"`
	Outputs   []TaskFile        `json:"outputs,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Lifecycle TaskLifecycle     `json:"lifecycle"`
}

// TaskResources pins the hardware. foray always sets InstanceType, because
// internal/device and internal/sizing have already chosen it — spawn's sizer is
// bypassed rather than second-guessed.
type TaskResources struct {
	InstanceType string `json:"instance_type,omitempty"`
	Purchase     string `json:"purchase,omitempty"` // spot | on_demand
	Fallback     string `json:"fallback,omitempty"` // on_demand when spot is unavailable
	DiskGiB      int32  `json:"disk_gib,omitempty"` // root volume; 0 = the AMI default
	// S3ReadWrite lists s3://bucket[/prefix] URIs the task needs scoped access to
	// *beyond* what Inputs/Outputs imply. foray needs it: the worker does its own S3 I/O
	// for saved activations, which no manifest declares, so without this the instance
	// role can stage the graph in and the result out but the saves themselves fail.
	// spawn grants it bucket-scoped (the prefix is advisory) because plugins do
	// bucket-level ListBucket.
	S3ReadWrite []string `json:"s3_read_write,omitempty"`
}

// TaskFile is one staged path. spawn's wrapper copies inputs in before the container
// starts and outputs out after it exits, using grants implied by these manifests — so
// no extra permission is needed for either.
//
// Destinations must be flat paths in /tmp. spawn bind-mounts a staged path's parent as
// the instance user while `docker run` gets no --user, and an output path's parent is
// created as root — so anything outside /tmp (mode 1777) is unwritable by the image's
// own user. spawn's own /data + /work example cannot work on this path
// (spore-host/spawn#555, documented in scientific-codes-cookbook).
type TaskFile struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// TaskLifecycle bounds the task. OnComplete fires on the *existence* of the
// completion signal rather than on a zero exit code, so `terminate` reaps a failed
// task too — which is what lets foray's own end-of-rung terminate become a backstop
// rather than the mechanism.
type TaskLifecycle struct {
	TTL        string  `json:"ttl"`
	OnComplete string  `json:"on_complete"`          // terminate | stop | hibernate
	CostLimit  float64 `json:"cost_limit,omitempty"` // a second budget belt, inside spawn
}

// TaskRun is what a launch yields. TaskID echoes what the caller chose; it is also the
// instance's Name tag (spawn sets `Name: spec.TaskID`), which is why Status, List and
// Terminate all resolve from it.
type TaskRun struct {
	TaskID string
}

// TaskCompletion is a task's durable completion record (spawn
// pkg/taskproto.CompletionRecord).
type TaskCompletion struct {
	TaskID   string
	State    string // completed | failed
	ExitCode int
	// RetryClass is spawn's classification of *what* failed, in its own priority order:
	// staging in, the command itself, then output delivery. Worth carrying because the
	// three are very different reports to a user — a missing graph object, a trace that
	// raised, and a trace that ran but whose result never reached S3.
	RetryClass string
	// Complete is false while the task is still running — spawn reports "no completion
	// record yet" rather than an error, and so does this.
	Complete bool
}

// Task lifecycle defaults, matching brain's launch defaults.
const (
	TaskOnCompleteTerminate = "terminate"
	// TaskOnCompleteStop leaves the instance stopped instead of gone. Its root volume
	// survives for inspection, and keeps billing until TTL — which is why terminate is
	// the default and this is only what `foray run --keep` asks for.
	TaskOnCompleteStop   = "stop"
	TaskPurchaseSpot     = "spot"
	TaskFallbackOnDemand = "on_demand"
)

// task is the real adapter over the spawn binary.
type task struct{ run Runner }

// NewTask returns a Task backed by the real spawn binary.
func NewTask(r Runner) Task { return task{run: r} }

// ErrTaskSpec marks a spec that cannot be launched, reported before any AWS call.
var ErrTaskSpec = errors.New("spore: invalid task spec")

func (t task) Run(ctx context.Context, spec TaskSpec) (TaskRun, error) {
	if err := spec.validate(); err != nil {
		return TaskRun{}, err
	}

	// spawn takes the spec as a file (`--spec <path>`), so write one. In a temp dir
	// rather than the working directory: a deploy may run from a read-only checkout,
	// and the spec is ephemeral.
	dir, err := os.MkdirTemp("", "foray-task-")
	if err != nil {
		return TaskRun{}, fmt.Errorf("spawn task run %s: temp dir: %w", spec.TaskID, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	body, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return TaskRun{}, fmt.Errorf("spawn task run %s: marshal spec: %w", spec.TaskID, err)
	}
	path := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return TaskRun{}, fmt.Errorf("spawn task run %s: write spec: %w", spec.TaskID, err)
	}

	// stdout is deliberately not parsed. `spawn task run` prints a human summary and
	// ignores `-o json` (verified against the real binary), and there is nothing in it
	// foray needs: the caller chose the task id, and everything downstream resolves
	// from that. Depending on an unconfirmed output shape is the mistake that produced
	// the adapter-drift bugs in #79.
	if _, err := t.run.Run(ctx, "spawn", "task", "run", "--spec", path); err != nil {
		return TaskRun{}, fmt.Errorf("spawn task run %s: %w", spec.TaskID, err)
	}
	return TaskRun{TaskID: spec.TaskID}, nil
}

func (t task) Status(ctx context.Context, taskID string) (TaskCompletion, error) {
	if taskID == "" {
		return TaskCompletion{}, fmt.Errorf("%w: Status needs a task id", ErrTaskSpec)
	}
	out, err := t.run.Run(ctx, "spawn", "task", "status", taskID, "-o", "json")
	if err != nil {
		return TaskCompletion{}, fmt.Errorf("spawn task status %s: %w", taskID, err)
	}
	return parseTaskStatus(taskID, out)
}

// parseTaskStatus reads a completion record, tolerating the "still running" answer.
//
// `spawn task status -o json` emits JSON **only once a record exists**; while the task
// is running it prints a human line ("Task <id>: running (no completion record yet)")
// and exits 0. So a decode failure is not an error here — it is the pending case.
// Verified against the real binary; reported upstream as an inconsistency worth
// smoothing, but this is the contract today.
func parseTaskStatus(taskID string, out []byte) (TaskCompletion, error) {
	trimmed := strings.TrimSpace(string(out))
	if !strings.HasPrefix(trimmed, "{") {
		return TaskCompletion{TaskID: taskID, Complete: false}, nil
	}
	var rec struct {
		TaskID     string `json:"task_id"`
		ExitCode   int    `json:"exit_code"`
		State      string `json:"state"`
		RetryClass string `json:"retry_class"`
	}
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		return TaskCompletion{}, fmt.Errorf("spawn task status %s: parse completion record: %w", taskID, err)
	}
	if rec.State == "" {
		// JSON without a state is not a completion record; treat it as pending rather
		// than inventing an outcome.
		return TaskCompletion{TaskID: taskID, Complete: false}, nil
	}
	return TaskCompletion{
		TaskID:     orDefault(rec.TaskID, taskID),
		State:      rec.State,
		ExitCode:   rec.ExitCode,
		RetryClass: rec.RetryClass,
		Complete:   true,
	}, nil
}

// Failed reports whether the task ended badly. Both signals are checked because either
// alone has been wrong: spawn has recorded state=completed with exit_code=0 while a
// declared output never staged (spore-host/spawn#561), which is why foray trusts the
// *artifact* and uses this only to detect a task that died without producing one.
func (c TaskCompletion) Failed() bool {
	return c.Complete && (c.ExitCode != 0 || c.State == "failed")
}

func (s TaskSpec) validate() error {
	var missing []string
	if strings.TrimSpace(s.TaskID) == "" {
		missing = append(missing, "TaskID")
	}
	if len(s.Command) == 0 {
		missing = append(missing, "Command")
	}
	if strings.TrimSpace(s.Lifecycle.TTL) == "" {
		// A task with no hard deadline is the one failure this project treats as
		// existential: nothing in-instance has anything to enforce.
		missing = append(missing, "Lifecycle.TTL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s required", ErrTaskSpec, strings.Join(missing, ", "))
	}
	for _, f := range append(append([]TaskFile{}, s.Inputs...), s.Outputs...) {
		// Enforce the /tmp rule at the boundary rather than trusting every caller to
		// remember it: a destination elsewhere fails at run time, on a GPU, after the
		// image has already been pulled.
		if d := f.Destination; strings.HasPrefix(d, "/") && !strings.HasPrefix(d, "/tmp/") {
			return fmt.Errorf("%w: staged destination %q must be under /tmp (spawn bind-mounts a "+
				"staged path's parent as the instance user while docker run gets no --user; only "+
				"/tmp is mode 1777)", ErrTaskSpec, d)
		}
	}
	return nil
}

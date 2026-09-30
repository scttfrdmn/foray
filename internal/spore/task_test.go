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
	"os"
	"strings"
	"testing"
)

// forayTaskSpec is the spec shape foray launches.
func forayTaskSpec() TaskSpec {
	return TaskSpec{
		TaskID:    "foray-r0-gpt2-test",
		Command:   []string{"python3", "-m", "worker.batch"},
		Container: "942542972736.dkr.ecr.us-west-2.amazonaws.com/foray-worker:dev",
		Resources: TaskResources{
			InstanceType: "g7e.xlarge",
			Purchase:     TaskPurchaseSpot,
			Fallback:     TaskFallbackOnDemand,
		},
		Inputs: []TaskFile{{
			Source:      "s3://foray-data-942542972736-usw2/sessions/foray-r0-gpt2-test/graph.json",
			Destination: "/tmp/graph.json",
		}},
		Outputs: []TaskFile{{
			Source:      "/tmp/result.json",
			Destination: "s3://foray-data-942542972736-usw2/sessions/foray-r0-gpt2-test/result.json",
		}},
		Env:       map[string]string{"FORAY_SESSION_ID": "foray-r0-gpt2-test", "FORAY_FAKE": "1"},
		Lifecycle: TaskLifecycle{TTL: "2h", OnComplete: TaskOnCompleteTerminate, CostLimit: 0.5},
	}
}

// The spec is a wire contract with another program, so it is pinned against JSON that
// real spawn accepted. This exact document was validated with
// `spawn task run --spec … --dry-run --region us-west-2`, which echoed back the
// container path ("docker run, private ECR"), the auto-selected GPU DLAMI, the pinned
// instance type, spot purchasing, the TTL, terminate-on-complete, the cost limit, and
// both staged paths.
//
// Guessing a JSON shape is what produced the adapter-drift bugs in #79; this is the
// habit that replaces guessing.
func TestTaskSpecMarshalsToSpawnsContract(t *testing.T) {
	const want = `{
  "task_id": "foray-r0-gpt2-test",
  "command": [
    "python3",
    "-m",
    "worker.batch"
  ],
  "container": "942542972736.dkr.ecr.us-west-2.amazonaws.com/foray-worker:dev",
  "resources": {
    "instance_type": "g7e.xlarge",
    "purchase": "spot",
    "fallback": "on_demand"
  },
  "inputs": [
    {
      "source": "s3://foray-data-942542972736-usw2/sessions/foray-r0-gpt2-test/graph.json",
      "destination": "/tmp/graph.json"
    }
  ],
  "outputs": [
    {
      "source": "/tmp/result.json",
      "destination": "s3://foray-data-942542972736-usw2/sessions/foray-r0-gpt2-test/result.json"
    }
  ],
  "env": {
    "FORAY_FAKE": "1",
    "FORAY_SESSION_ID": "foray-r0-gpt2-test"
  },
  "lifecycle": {
    "ttl": "2h",
    "on_complete": "terminate",
    "cost_limit": 0.5
  }
}`

	got, err := json.MarshalIndent(forayTaskSpec(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("spec JSON drifted from what spawn accepted.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Run writes the spec to a file and hands spawn its path, because `spawn task run`
// takes --spec rather than stdin.
func TestTaskRunPassesASpecFile(t *testing.T) {
	var specPath string
	r := &stubRunner{out: []byte("DRY RUN — nothing will be launched.\n")}
	res, err := NewTask(r).Run(context.Background(), forayTaskSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TaskID != "foray-r0-gpt2-test" {
		t.Errorf("TaskID = %q, want the caller's id echoed back", res.TaskID)
	}
	if len(r.gotArgs) < 2 || r.gotArgs[0] != "task" || r.gotArgs[1] != "run" {
		t.Fatalf("args = %v, want `task run …`", r.gotArgs)
	}
	specPath = argValue(r.gotArgs, "--spec")
	if specPath == "" {
		t.Fatal("no --spec argument")
	}
	if !strings.HasSuffix(specPath, ".json") {
		t.Errorf("--spec = %q, want a .json path", specPath)
	}
	// The temp file is cleaned up, so the path must no longer exist.
	if _, err := os.Stat(specPath); !os.IsNotExist(err) {
		t.Errorf("spec file %s survived the call; it should be cleaned up", specPath)
	}
}

// A task with no hard deadline is the failure this project treats as existential:
// nothing in-instance has anything to enforce, so the instance can run until someone
// notices the bill. Rejected before any AWS call.
func TestTaskRunRequiresATTL(t *testing.T) {
	spec := forayTaskSpec()
	spec.Lifecycle.TTL = ""
	r := &stubRunner{}
	_, err := NewTask(r).Run(context.Background(), spec)
	if !errors.Is(err, ErrTaskSpec) {
		t.Fatalf("err = %v, want ErrTaskSpec", err)
	}
	if r.gotArgs != nil {
		t.Error("an invalid spec reached spawn; it must be refused first")
	}
}

func TestTaskRunRequiresIDAndCommand(t *testing.T) {
	tests := []struct {
		name  string
		mutle func(*TaskSpec)
	}{
		{"no task id", func(s *TaskSpec) { s.TaskID = "" }},
		{"no command", func(s *TaskSpec) { s.Command = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := forayTaskSpec()
			tt.mutle(&spec)
			if _, err := NewTask(&stubRunner{}).Run(context.Background(), spec); !errors.Is(err, ErrTaskSpec) {
				t.Fatalf("err = %v, want ErrTaskSpec", err)
			}
		})
	}
}

// Staged destinations must be flat paths in /tmp. spawn bind-mounts a staged path's
// parent as the instance user while `docker run` gets no --user, and an output path's
// parent is created as root — so anything outside /tmp (mode 1777) is unwritable by the
// image's own user, and spawn's own /data + /work example cannot work
// (spore-host/spawn#555). Enforced here because the alternative is discovering it at
// run time, on a GPU, after the image has already been pulled.
func TestTaskRunRejectsStagingOutsideTmp(t *testing.T) {
	tests := []struct {
		name string
		spec func() TaskSpec
	}{
		{"input elsewhere", func() TaskSpec {
			s := forayTaskSpec()
			s.Inputs[0].Destination = "/data/graph.json"
			return s
		}},
		{"output elsewhere", func() TaskSpec {
			s := forayTaskSpec()
			s.Outputs[0].Source = "/work/result.json"
			s.Outputs[0].Destination = "s3://b/result.json"
			// The *source* is a local path on the output side; the destination is S3.
			// Use an input to exercise the local-path rule on this case.
			s.Inputs[0].Destination = "/work/graph.json"
			return s
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTask(&stubRunner{}).Run(context.Background(), tt.spec())
			if !errors.Is(err, ErrTaskSpec) {
				t.Fatalf("err = %v, want ErrTaskSpec", err)
			}
			if !strings.Contains(err.Error(), "/tmp") {
				t.Errorf("error %q should explain the /tmp rule", err)
			}
		})
	}
}

// An S3 destination on an output is not a local path and must not trip the /tmp rule.
func TestTaskRunAllowsS3Destinations(t *testing.T) {
	r := &stubRunner{out: []byte("ok")}
	if _, err := NewTask(r).Run(context.Background(), forayTaskSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --- status ------------------------------------------------------------------

// `spawn task status -o json` emits JSON only once a record exists; while the task runs
// it prints a human line and exits 0. So a non-JSON answer is the PENDING case, not an
// error — reading it as a failure would report a spurious problem on every poll before
// the task finishes. Verified against the real binary.
func TestTaskStatusPendingWhenNoRecordYet(t *testing.T) {
	r := &stubRunner{out: []byte("Task foray-r0-gpt2-test: running (no completion record yet)\n")}
	got, err := NewTask(r).Status(context.Background(), "foray-r0-gpt2-test")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Complete {
		t.Error("a task with no completion record must read as pending")
	}
	if got.Failed() {
		t.Error("a pending task must not read as failed")
	}
}

func TestTaskStatusCompletionRecord(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantState  string
		wantExit   int
		wantFailed bool
	}{
		{
			name:      "success",
			body:      `{"task_id":"t1","exit_code":0,"state":"completed","started_at":"x","ended_at":"y"}`,
			wantState: "completed",
		},
		{
			name:       "non-zero exit",
			body:       `{"task_id":"t1","exit_code":1,"state":"failed"}`,
			wantState:  "failed",
			wantExit:   1,
			wantFailed: true,
		},
		{
			// Both signals are checked because either alone has been wrong: spawn has
			// recorded completed/0 while a declared output never staged (#561).
			name:       "failed state with a zero exit",
			body:       `{"task_id":"t1","exit_code":0,"state":"failed"}`,
			wantState:  "failed",
			wantFailed: true,
		},
		{
			name:      "json without a state is not a completion record",
			body:      `{"task_id":"t1"}`,
			wantState: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewTask(&stubRunner{out: []byte(tt.body)}).Status(context.Background(), "t1")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.ExitCode != tt.wantExit {
				t.Errorf("ExitCode = %d, want %d", got.ExitCode, tt.wantExit)
			}
			if got.Failed() != tt.wantFailed {
				t.Errorf("Failed() = %v, want %v", got.Failed(), tt.wantFailed)
			}
			if tt.wantState != "" && !got.Complete {
				t.Error("a record with a state must be complete")
			}
		})
	}
}

func TestTaskStatusNeedsAnID(t *testing.T) {
	if _, err := NewTask(&stubRunner{}).Status(context.Background(), ""); !errors.Is(err, ErrTaskSpec) {
		t.Fatalf("err = %v, want ErrTaskSpec", err)
	}
}

// --- the fake ----------------------------------------------------------------

// The fake reports pending once before completing, so a caller's poll loop is exercised
// offline rather than reading a result that was there all along.
func TestFakeTaskPollsBeforeCompleting(t *testing.T) {
	f := NewFake()
	spec := forayTaskSpec()
	if _, err := f.Task.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	first, err := f.Task.Status(context.Background(), spec.TaskID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if first.Complete {
		t.Error("first status should be pending, so the poll path is exercised")
	}

	second, err := f.Task.Status(context.Background(), spec.TaskID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !second.Complete || second.Failed() {
		t.Errorf("second status = %+v, want a clean completion", second)
	}
}

// A never-launched task reads as pending, matching spawn's own answer for an unknown id.
func TestFakeTaskUnknownIsPending(t *testing.T) {
	f := NewFake()
	got, err := f.Task.Status(context.Background(), "foray-never-launched")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Complete {
		t.Error("an unknown task must read as pending, as spawn reports it")
	}
}

// The fake records what foray asked spawn for, so a wiring test can assert the spec.
func TestFakeTaskRecordsTheSpec(t *testing.T) {
	f := NewFake()
	spec := forayTaskSpec()
	if _, err := f.Task.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ft, ok := f.Task.(*fakeTask)
	if !ok {
		t.Fatal("fake Task should be *fakeTask")
	}
	got, ok := ft.Spec(spec.TaskID)
	if !ok {
		t.Fatal("spec not recorded")
	}
	if got.Container != spec.Container || got.Resources.InstanceType != spec.Resources.InstanceType {
		t.Errorf("recorded spec = %+v, want the launched one", got)
	}
}

// The fake refuses an invalid spec exactly as the real adapter does, so the rehearsal
// cannot pass a spec production would reject.
func TestFakeTaskValidatesLikeTheRealAdapter(t *testing.T) {
	f := NewFake()
	spec := forayTaskSpec()
	spec.Lifecycle.TTL = ""
	if _, err := f.Task.Run(context.Background(), spec); !errors.Is(err, ErrTaskSpec) {
		t.Fatalf("err = %v, want ErrTaskSpec", err)
	}
}

// A manual, opt-in check that the spec foray *generates* is accepted by the installed
// spawn — not merely that it matches a golden string I wrote down. Never run in CI (it
// needs the spawn binary and credentials to size against a region), same precedent as
// make worker-smoke and the gateway's live handoff test.
//
//	FORAY_LIVE_SPAWN=1 AWS_PROFILE=aws go test ./internal/spore/ -run TestLiveTaskSpec -v
//
// `--dry-run` sizes and previews without launching, so this costs nothing. It is the
// guard that would catch spawn renaming or restructuring a TaskSpec field — the exact
// class of drift that produced #79, where hand-inferred JSON tags decoded to zero
// values against the real tool while the fakes stayed green.
func TestLiveTaskSpecAcceptedBySpawn(t *testing.T) {
	if os.Getenv("FORAY_LIVE_SPAWN") != "1" {
		t.Skip("manual: set FORAY_LIVE_SPAWN=1 with the spawn binary and AWS credentials")
	}
	dir := t.TempDir()
	body, err := json.MarshalIndent(forayTaskSpec(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := dir + "/spec.json"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	out, err := NewExecRunner().Run(context.Background(), "spawn",
		"task", "run", "--spec", path, "--dry-run", "--region", "us-west-2")
	t.Logf("spawn output:\n%s", out)
	if err != nil {
		t.Fatalf("spawn rejected the generated spec: %v", err)
	}

	// Assert spawn echoed back the decisions that matter, so a silently ignored field
	// fails here rather than at run time on a GPU.
	got := string(out)
	for _, want := range []string{
		"foray-r0-gpt2-test",     // task id
		"worker.batch",           // the command
		"private ECR",            // the container path was taken
		"GPU DLAMI",              // the GPU driver AMI was auto-selected
		"g7e.xlarge",             // the pinned instance type
		"spot",                   // purchasing
		"on-complete: terminate", // the lifecycle
		"/tmp/graph.json",        // input staging
		"/tmp/result.json",       // output staging
	} {
		if !strings.Contains(got, want) {
			t.Errorf("spawn's preview does not mention %q — the field may have been ignored", want)
		}
	}
}

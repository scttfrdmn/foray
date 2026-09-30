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

package brain

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/scttfrdmn/foray/internal/sizing"
	"github.com/scttfrdmn/foray/internal/spore"
)

// recordingTask captures the spec foray hands `spawn task run`.
type recordingTask struct {
	spec spore.TaskSpec
	runs int
	err  error
}

func (r *recordingTask) Run(_ context.Context, spec spore.TaskSpec) (spore.TaskRun, error) {
	r.runs++
	r.spec = spec
	return spore.TaskRun{TaskID: spec.TaskID}, r.err
}

func (r *recordingTask) Status(context.Context, string) (spore.TaskCompletion, error) {
	return spore.TaskCompletion{}, nil
}

func testRung() *Rung {
	return &Rung{
		Index:       0,
		Engine:      sizing.EngineEager,
		Model:       sizing.Model{Name: "openai-community/gpt2", ParamsB: 0.124, BytesPer: 2},
		ModelSource: "hf",
		Chosen:      sizing.Option{InstanceType: "g7e.xlarge"},
		NNSight:     "pass",
	}
}

func testExecutor(task spore.Task) SpawnExecutor {
	return SpawnExecutor{
		Task:        task,
		WorkerImage: "123456789012.dkr.ecr.us-west-2.amazonaws.com/foray-worker:dev",
		DataBucket:  "foray-data-usw2",
		Device:      "cuda",
		Region:      "us-west-2",
		Spot:        true,
	}
}

// The spec is what actually launches a GPU, so every field that bounds cost or delivers
// the work is pinned. A silently-dropped field here is an instance that runs without a
// worker, without a deadline, or without the graph it was launched to run.
func TestExecuteBuildsTheTaskSpec(t *testing.T) {
	task := &recordingTask{}
	const sid = "foray-r0-gpt2-20260930T120000-abcd1234"

	if err := testExecutor(task).Execute(context.Background(), Question{Text: "q"}, testRung(), sid); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := task.spec

	if got.TaskID != sid {
		t.Errorf("TaskID = %q, want the caller's session id %q — it is also the instance's Name tag", got.TaskID, sid)
	}
	if got.Container == "" {
		t.Error("no container: the instance would boot with no worker on it, which is the gap #103 exists to close")
	}
	if got.Resources.InstanceType != "g7e.xlarge" {
		t.Errorf("InstanceType = %q, want the chosen type verbatim — sizing already decided it",
			got.Resources.InstanceType)
	}
	if got.Lifecycle.TTL == "" {
		t.Error("no TTL: nothing in-instance would have a deadline to enforce")
	}
	if got.Lifecycle.OnComplete != spore.TaskOnCompleteTerminate {
		t.Errorf("OnComplete = %q, want terminate — a stopped instance keeps billing EBS (#80)",
			got.Lifecycle.OnComplete)
	}

	// Staging: the graph in from the session's prefix, the result back out to it.
	if len(got.Inputs) != 1 || got.Inputs[0].Source != "s3://foray-data-usw2/sessions/"+sid+"/graph.json" {
		t.Errorf("Inputs = %+v, want the session's graph object", got.Inputs)
	}
	if got.Inputs[0].Destination != workerGraphPath {
		t.Errorf("graph staged to %q, want %q", got.Inputs[0].Destination, workerGraphPath)
	}
	if len(got.Outputs) != 1 || got.Outputs[0].Source != workerResultPath {
		t.Errorf("Outputs = %+v, want the worker's result file", got.Outputs)
	}
	if got.Outputs[0].Destination != "s3://foray-data-usw2/sessions/"+sid+"/result.json" {
		t.Errorf("result staged to %q, want the session's result key", got.Outputs[0].Destination)
	}

	// The worker's own saves are S3 I/O no manifest declares, so they need their own
	// grant — without it the trace runs and then fails writing the activations it was for.
	if len(got.Resources.S3ReadWrite) != 1 || !strings.HasPrefix(got.Resources.S3ReadWrite[0], "s3://foray-data-usw2") {
		t.Errorf("S3ReadWrite = %v, want the data bucket: the worker writes its saves itself",
			got.Resources.S3ReadWrite)
	}

	// The session's configuration rides in env, since the container's command is fixed.
	for k, want := range map[string]string{
		"FORAY_SESSION_ID":     sid,
		"FORAY_MODEL_URI":      "openai-community/gpt2",
		"FORAY_DEFAULT_ENGINE": "eager",
		"FORAY_SAVE_BUCKET":    "foray-data-usw2",
		"FORAY_DEVICE":         "cuda",
		"FORAY_GRAPH_PATH":     workerGraphPath,
		"FORAY_RESULT_PATH":    workerResultPath,
	} {
		if got.Env[k] != want {
			t.Errorf("Env[%s] = %q, want %q", k, got.Env[k], want)
		}
	}
}

// Spot with an on-demand fallback, not spot alone: the human already said Go, and a spot
// shortage is not a reason to lose the approval.
func TestExecuteSpotFallsBackToOnDemand(t *testing.T) {
	task := &recordingTask{}
	e := testExecutor(task)
	if err := e.Execute(context.Background(), Question{}, testRung(), "foray-r0-x"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if task.spec.Resources.Purchase != spore.TaskPurchaseSpot {
		t.Errorf("Purchase = %q, want spot", task.spec.Resources.Purchase)
	}
	if task.spec.Resources.Fallback != spore.TaskFallbackOnDemand {
		t.Errorf("Fallback = %q, want on_demand — a spot shortage must not discard a Go", task.spec.Resources.Fallback)
	}

	// Without --spot, neither is set and spawn's own default (on-demand) applies.
	e.Spot = false
	task2 := &recordingTask{}
	e.Task = task2
	if err := e.Execute(context.Background(), Question{}, testRung(), "foray-r0-x"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if task2.spec.Resources.Purchase != "" || task2.spec.Resources.Fallback != "" {
		t.Errorf("purchasing = %+v, want unset", task2.spec.Resources)
	}
}

// --keep reaches the launch only through OnComplete. Terminating by default is the
// ephemerality invariant; nothing else in Execute may override it.
func TestExecuteHonorsOnComplete(t *testing.T) {
	task := &recordingTask{}
	e := testExecutor(task)
	e.OnComplete = spore.TaskOnCompleteStop
	if err := e.Execute(context.Background(), Question{}, testRung(), "foray-r0-x"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if task.spec.Lifecycle.OnComplete != spore.TaskOnCompleteStop {
		t.Errorf("OnComplete = %q, want stop", task.spec.Lifecycle.OnComplete)
	}
}

// A launch that cannot possibly work must be refused before it bills: no instance type,
// no worker image (the instance would boot with nothing on it), no bucket (nowhere to
// stage the graph from or the result to).
func TestExecuteRefusesAnUnrunnableLaunch(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SpawnExecutor, *Rung)
		wantMsg string
	}{
		{"no instance type", func(_ *SpawnExecutor, r *Rung) { r.Chosen.InstanceType = "" }, "instance type"},
		{"no worker image", func(e *SpawnExecutor, _ *Rung) { e.WorkerImage = "" }, "worker image"},
		{"no data bucket", func(e *SpawnExecutor, _ *Rung) { e.DataBucket = "" }, "data bucket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &recordingTask{}
			e, r := testExecutor(task), testRung()
			tt.mutate(&e, r)

			err := e.Execute(context.Background(), Question{}, r, "foray-r0-x")
			if err == nil {
				t.Fatal("want a refusal before anything launches")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, should mention %q", err, tt.wantMsg)
			}
			if task.runs != 0 {
				t.Error("an unrunnable launch reached spawn")
			}
		})
	}
}

// The root disk must hold the weights. spawn floors the request at the AMI's own snapshot
// size, so asking cannot fail a launch — but leaving it unset means spawn's 20 GiB default
// and whatever the AMI floor happens to be, which says nothing about room for a 70B
// download.
func TestRootDiskGiBGrowsWithTheModel(t *testing.T) {
	small := rootDiskGiB(sizing.Model{Name: "gpt2", ParamsB: 0.124, BytesPer: 2})
	large := rootDiskGiB(sizing.Model{Name: "llama-70b", ParamsB: 70, BytesPer: 2})

	if small <= 0 {
		t.Fatalf("gpt2 asked for %d GiB", small)
	}
	if large <= small {
		t.Errorf("70B asked for %d GiB, not more than gpt2's %d — the weights would not fit", large, small)
	}
	// Two copies of 140 GB of weights plus a base, so comfortably past 300 GiB.
	if large < 300 {
		t.Errorf("70B asked for %d GiB; the download and the loaded copy can coexist", large)
	}
	// An unset BytesPer must not collapse the estimate to the base alone.
	if got := rootDiskGiB(sizing.Model{Name: "x", ParamsB: 70}); got < 300 {
		t.Errorf("with BytesPer unset, asked for %d GiB; bf16 is the sizer's assumption", got)
	}
}

// The staged paths are a cross-language contract: brain names them in the task spec's
// manifests and worker/config.py defaults to them. If they drift, spawn stages the graph
// somewhere the worker never looks and the trace fails with a file-not-found on a GPU.
func TestStagedPathsMatchTheWorker(t *testing.T) {
	b, err := os.ReadFile("../../worker/config.py")
	if err != nil {
		t.Fatalf("read worker/config.py: %v", err)
	}
	src := string(b)
	for _, want := range []string{workerGraphPath, workerResultPath} {
		if !strings.Contains(src, `"`+want+`"`) {
			t.Errorf("worker/config.py does not default to %s — the worker would look in the wrong place", want)
		}
	}
	// And they must be in /tmp: spawn bind-mounts a staged path's parent as the instance
	// user while `docker run` gets no --user, so nothing else is reliably writable
	// (spore-host/spawn#555). internal/spore refuses a spec that breaks this, but failing
	// here says why.
	for _, p := range []string{workerGraphPath, workerResultPath} {
		if !strings.HasPrefix(p, "/tmp/") {
			t.Errorf("staged path %s is outside /tmp; the image's own user could not write it", p)
		}
	}
}

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
	"fmt"
	"math"
	"time"

	"github.com/scttfrdmn/foray/internal/sizing"
	"github.com/scttfrdmn/foray/internal/spore"
)

// SpawnExecutor launches an approved rung via the spawn adapter. It is the real
// Executor seam: Approve (the human's Go) is the only caller, so an instance is
// summoned only after acceptance — never during planning. The ephemerality
// guardrails (TTL + idle) ride along so cost stays per-session, not per-hour.
type SpawnExecutor struct {
	// Task is `spawn task run` — the whole data-plane launch (#103).
	Task spore.Task
	// Spawn remains for the lifecycle verbs the CLI still uses (list, terminate, status).
	Spawn spore.Spawn

	// WorkerImage is the ECR image holding the nnsight worker. Required: without it the
	// instance has no worker, which is the gap that made no real trace runnable.
	WorkerImage string
	// DataBucket is the user's in-region bucket. The graph is staged from it and the
	// result reference staged back to it.
	DataBucket string
	// Device is the accelerator target passed to the worker ("cuda" now; "neuron" when
	// TorchNeuron GAs). Empty leaves the worker's own default.
	Device string

	Region string        // optional; empty ⇒ spawn picks by availability/price
	Spot   bool          // Spot for the cheap path, falling back to on-demand
	TTL    time.Duration // hard auto-terminate ceiling
	// OnComplete is what happens to the instance when the trace ends. Empty ⇒ terminate,
	// which is the ephemerality invariant. `foray run --keep` sets stop instead, trading
	// EBS billing until TTL for a root volume you can restart and look at.
	OnComplete   string
	CostLimitUSD float64 // per-task ceiling enforced inside spawn; 0 = unset
}

// DefaultTTL bounds a session when the caller does not set one. It is the hard
// deadline, not a target: a rung runs one trace and `on_complete: terminate` reaps the
// instance as soon as the trace finishes, so the TTL only catches what escapes that.
const DefaultTTL = 2 * time.Hour

// Execute implements Executor: run the approved rung as a `spawn task run` task.
//
// This is the reuse the project's own rule asks for (CLAUDE.md: "if you find yourself
// writing an instance launcher, stop — call the tool"). `spawn task run` delivers the
// worker image from ECR, auto-selects the GPU driver AMI for a GPU instance type, stages
// the graph in before the container starts, stages the result reference back out, and
// terminates the instance on completion — including on failure. foray previously launched
// a bare instance and hand-rolled the middle three (issue #103).
//
// The graph is already at sessions/<sessionID>/graph.json by the time this is called:
// that is why the caller owns the id.
func (e SpawnExecutor) Execute(ctx context.Context, q Question, r *Rung, sessionID string) error {
	_ = q // the question is the invariant the rung serves, not an input to the launch
	if r.Chosen.InstanceType == "" {
		return fmt.Errorf("execute rung %d: no instance type chosen", r.Index)
	}
	if e.WorkerImage == "" {
		return fmt.Errorf("execute rung %d: no worker image configured "+
			"(set FORAY_WORKER_IMAGE to the ECR image `make worker-push` published)", r.Index)
	}
	if e.DataBucket == "" {
		return fmt.Errorf("execute rung %d: no data bucket configured (set FORAY_DATA_BUCKET)", r.Index)
	}
	ttl := e.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	onComplete := e.OnComplete
	if onComplete == "" {
		onComplete = spore.TaskOnCompleteTerminate
	}

	purchase, fallback := "", ""
	if e.Spot {
		// Fall back to on-demand rather than fail the rung: the human already said Go,
		// and a spot shortage is not a reason to lose the approval.
		purchase, fallback = spore.TaskPurchaseSpot, spore.TaskFallbackOnDemand
	}

	spec := spore.TaskSpec{
		TaskID:    sessionID,
		Command:   []string{"python3", "-m", "worker.batch"},
		Container: e.WorkerImage,
		Resources: spore.TaskResources{
			// The type is already chosen by internal/device + internal/sizing, so spawn's
			// sizer is bypassed rather than second-guessed.
			InstanceType: r.Chosen.InstanceType,
			Purchase:     purchase,
			Fallback:     fallback,
			DiskGiB:      rootDiskGiB(r.Model),
			// The staging manifests only cover graph.json and result.json. The worker's own
			// saves are S3 I/O spawn never sees, so they need an explicit grant — without
			// it the trace runs and then fails writing the very activations it was for.
			S3ReadWrite: []string{"s3://" + e.DataBucket + "/sessions"},
		},
		Inputs:  []spore.TaskFile{{Source: graphURI(e.DataBucket, sessionID), Destination: workerGraphPath}},
		Outputs: []spore.TaskFile{{Source: workerResultPath, Destination: resultURI(e.DataBucket, sessionID)}},
		Env:     e.workerEnv(r, sessionID),
		Lifecycle: spore.TaskLifecycle{
			TTL: durationString(ttl),
			// terminate by default, not stop: spawn's idle action only stops an instance
			// and a stopped instance keeps billing EBS (#80). on_complete fires on the
			// completion signal's existence, so this reaps a failed task too.
			OnComplete: onComplete,
			// A second budget belt, enforced inside spawn. Cedar's per-session ceiling and
			// the brain's per-question envelope still hold; this bounds the instance even
			// if the control plane goes away mid-rung.
			CostLimit: e.CostLimitUSD,
		},
	}
	if _, err := e.Task.Run(ctx, spec); err != nil {
		return fmt.Errorf("execute rung %d: %w", r.Index, err)
	}
	return nil
}

// Staged paths. Flat, and in /tmp: spawn bind-mounts a staged path's parent as the
// instance user while `docker run` gets no --user, and an output path's parent is created
// as root — so only /tmp (mode 1777) is reliably writable by the image's own user
// (spore-host/spawn#555). spawn's own /data + /work example cannot work here.
const (
	workerGraphPath  = "/tmp/graph.json"
	workerResultPath = "/tmp/result.json"
)

func graphURI(bucket, sessionID string) string {
	return fmt.Sprintf("s3://%s/sessions/%s/graph.json", bucket, sessionID)
}

func resultURI(bucket, sessionID string) string {
	return fmt.Sprintf("s3://%s/sessions/%s/result.json", bucket, sessionID)
}

// workerEnv is the session's configuration, passed to the container. The worker reads
// its graph from a staged local file, so it needs no bucket credentials for the handoff —
// only the save bucket it writes activations to.
func (e SpawnExecutor) workerEnv(r *Rung, sessionID string) map[string]string {
	env := map[string]string{
		"FORAY_SESSION_ID":     sessionID,
		"FORAY_MODEL_URI":      r.Model.Name,
		"FORAY_DEFAULT_ENGINE": string(r.Engine),
		"FORAY_SAVE_BUCKET":    e.DataBucket,
		"FORAY_GRAPH_PATH":     workerGraphPath,
		"FORAY_RESULT_PATH":    workerResultPath,
	}
	if e.Region != "" {
		env["FORAY_SAVE_REGION"] = e.Region
	}
	if e.Device != "" {
		env["FORAY_DEVICE"] = e.Device
	}
	return env
}

// durationString renders a TTL the way spawn's lifecycle accepts it.
func durationString(d time.Duration) string { return d.String() }

// rootDiskGiB sizes the root volume for the model's weights.
//
// Asking is safe in both directions: spawn floors the request at the AMI's own root
// snapshot size, so an under-request can never fail the launch, and an over-request is
// gp3 for the length of one rung — cents. Leaving it unset is the unsafe option, since
// spawn's own default is 20 GiB and only the AMI floor saves it, which says nothing about
// how much room is left for a 70B download.
//
// The estimate is the weights twice over (the download and the loaded copy can coexist)
// plus a base for the DLAMI, the worker image, and the HF cache's own scratch.
func rootDiskGiB(m sizing.Model) int32 {
	const baseGiB = 80
	bytesPer := m.BytesPer
	if bytesPer <= 0 {
		bytesPer = 2 // bf16, the default the sizer assumes
	}
	weightsGiB := m.ParamsB * float64(bytesPer) * 1e9 / (1 << 30)
	return int32(math.Ceil(baseGiB + 2*weightsGiB))
}

// sanitize reduces a model id to a spawn-name-safe token.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == '-' || c == '.':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return string(out)
}

// Config carries everything NewReal needs to wire the real brain. The Invoker
// and Spawn come from already-configured collaborators (a Bedrock client, the
// spore adapters) so this package stays free of SDK construction and credential
// handling.
type Config struct {
	Invoker    Invoker       // Bedrock Converse (see BedrockInvoker)
	Truffle    spore.Truffle // backs $/session pricing
	Task       spore.Task    // runs approved rungs as `spawn task run` tasks
	Spawn      spore.Spawn   // session lifecycle: list, stop, status
	Principal  Principal     // Cedar principal: budget ceiling, allowed tiers, toggles
	Techniques []string      // worker-supported techniques, constrains planning
	BudgetUSD  float64       // per-question envelope (0 ⇒ defaultQuestionBudgetUSD)
	Region     string        // optional spawn/pricing region scope
	Spot       bool          // Spot launch for the cheap path

	WorkerImage  string  // ECR image holding the nnsight worker (make worker-push)
	DataBucket   string  // in-region bucket: graph in, result + saves out
	Device       string  // accelerator target for the worker ("cuda")
	CostLimitUSD float64 // per-task ceiling enforced inside spawn; 0 = unset
	OnComplete   string  // instance fate when the trace ends; empty ⇒ terminate
}

// NewReal wires the real brain: Bedrock planner + Cedar policy + spawn executor,
// behind the same Planner/Policy/Executor seams the fake uses. It errors only on
// a build-time fault (the embedded policy failing to parse).
func NewReal(cfg Config) (*Brain, error) {
	pol, err := NewCedarPolicy(cfg.Principal)
	if err != nil {
		return nil, err
	}
	var regions []string
	if cfg.Region != "" {
		regions = []string{cfg.Region}
	}
	planner := &AgentCorePlanner{
		Invoker:    cfg.Invoker,
		Pricer:     NewTrufflePricer(cfg.Truffle, regions...),
		Techniques: cfg.Techniques,
		BudgetUSD:  cfg.BudgetUSD,
	}
	exec := SpawnExecutor{
		Task:         cfg.Task,
		Spawn:        cfg.Spawn,
		WorkerImage:  cfg.WorkerImage,
		DataBucket:   cfg.DataBucket,
		Device:       cfg.Device,
		Region:       cfg.Region,
		Spot:         cfg.Spot,
		OnComplete:   cfg.OnComplete,
		CostLimitUSD: cfg.CostLimitUSD,
	}
	interp := &AgentCoreInterpreter{Invoker: cfg.Invoker}
	return &Brain{Plan: planner, Policy: pol, Exec: exec, Interp: interp}, nil
}

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
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake-path sentinels. The real adapters surface the tool's own stderr; the
// fakes have no tool, so they return these for the few invariants worth
// preserving offline (a launch needs a name + type; a lookup needs a known id).
var (
	errFakeLaunch  = errors.New("spawn launch (fake): Name and InstanceType are required")
	errFakeUnknown = errors.New("spawn (fake): unknown instance id")
	errFakeWatch   = errors.New("lagotto watch (fake): InstanceType is required")
	errFakeServe   = errors.New("spawn service (fake): InstanceID and Command are required")
)

// Fakes for FORAY_FAKE=1: deterministic pricing/launch/watch data so the whole
// control loop runs with no AWS. Same pattern as internal/export and
// internal/brain (NewFake). Used by the CLI's offline path and the CI gate
// (make demo-fake).

// Fake bundles a fake of each tool. FromEnv hands this back when FORAY_FAKE=1 so
// callers can wire all three from one place.
type Fake struct {
	Truffle Truffle
	Spawn   Spawn
	Lagotto Lagotto
	Server  Server
	Task    Task
}

// Enabled reports whether the fake path is active (FORAY_FAKE=1).
func Enabled() bool { return os.Getenv("FORAY_FAKE") == "1" }

// FromEnv returns fakes when FORAY_FAKE=1, or real exec-backed adapters
// otherwise. ok is false when running against real AWS so the caller can decide
// how to surface "not wired yet" (the CLI does this today).
func FromEnv() (f Fake, fake bool) {
	if Enabled() {
		return NewFake(), true
	}
	r := NewExecRunner()
	return Fake{
		Truffle: NewTruffle(r),
		Spawn:   NewSpawn(r),
		Lagotto: NewLagotto(r),
		Server:  NewServer(NewExecStarter()),
		Task:    NewTask(r),
	}, false
}

// NewFake builds the offline set. The task fake shares the spawn fake's instance table,
// as the real pair do: `spawn task run` launches an instance tagged with the task id, so a
// task that ran is a session `foray sessions`, `foray stop` and the end-of-rung reap can
// all see. A fake that skipped that would have the rehearsal print a termination failure
// on every rung.
func NewFake() Fake {
	sp := newFakeSpawn()
	return Fake{
		Truffle: fakeTruffle{},
		Spawn:   sp,
		Lagotto: fakeLagotto{},
		Server:  &fakeServer{},
		Task:    newFakeTask(sp),
	}
}

// --- truffle fake -----------------------------------------------------------

type fakeTruffle struct{}

// fakeSpotUSDHr is a plausible Spot $/hour by instance type so cost numbers are
// stable and recognizable in the rehearsal. Unknown types fall back to a
// mid-range GPU rate.
var fakeSpotUSDHr = map[string]float64{
	"g7e.xlarge":   0.45,  // slice
	"g7.xlarge":    0.55,  // small
	"g7e.2xlarge":  1.20,  // mid
	"p5e.48xlarge": 22.00, // large (H200 ×8)
}

func (fakeTruffle) Price(_ context.Context, instanceType string, regions ...string) ([]SpotQuote, error) {
	hr, ok := fakeSpotUSDHr[instanceType]
	if !ok {
		hr = 1.00
	}
	region := "us-east-1"
	if len(regions) > 0 {
		region = regions[0]
	}
	return []SpotQuote{{
		InstanceType: instanceType,
		Region:       region,
		AZ:           region + "a",
		PriceUSDHr:   hr,
		OnDemandHr:   hr * 3, // Spot is ~⅓ on-demand, the headline truffle savings
	}}, nil
}

func (fakeTruffle) Quota(_ context.Context, family, region string) (Quota, error) {
	// Plenty of headroom in the fake so the brain never trips a quota gate offline.
	return Quota{Family: family, Region: region, Limit: 256, InUse: 0, Available: 256, Status: "ok"}, nil
}

func (fakeTruffle) Discover(_ context.Context, _ string) ([]string, error) {
	// The enabled NVIDIA menu (mirrors internal/device tiers), tightest-first.
	return []string{"g7e.xlarge", "g7.xlarge", "g7e.2xlarge", "p5e.48xlarge"}, nil
}

// --- spawn fake -------------------------------------------------------------

// fakeSpawn keeps a tiny in-memory instance table so Launch → Status →
// KeepWarm → Terminate behave like a coherent lifecycle in the rehearsal.
type fakeSpawn struct {
	mu   sync.Mutex
	seq  int
	inst map[string]Instance
	// idle models the *in-instance* idle deadline — the thing spawn's agent
	// maintains on the box, not a field spawn reports over the CLI. Launch seeds
	// it at now+grace and KeepWarm rolls it forward, which is faithful to how a
	// trace actually stays alive: LaunchSpec.ActivePorts puts the worker's port
	// under the idle daemon, so an in-flight trace resets that timer itself. The
	// fake stands in for the daemon so tests can assert the effect the real
	// system produces without a GPU. Read it with IdleDeadline.
	idle map[string]time.Time
	// now is fixed so the fake is deterministic (Date.now is non-deterministic
	// and the demo must reproduce); tests can read deadlines relative to it.
	now time.Time
}

func newFakeSpawn() *fakeSpawn {
	return &fakeSpawn{inst: map[string]Instance{}, idle: map[string]time.Time{}, now: fakeEpoch}
}

// IdleDeadline reports the modeled in-instance idle deadline for an instance.
// Test-only observability: the real spawn CLI exposes no such field, which is
// exactly why it lives on the fake rather than on Instance.
func (s *fakeSpawn) IdleDeadline(instanceID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.idle[instanceID]
	return d, ok
}

// fakeEpoch is a fixed reference time so launch/idle/TTL deadlines are stable
// across runs of make demo-fake.
var fakeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (s *fakeSpawn) Launch(_ context.Context, spec LaunchSpec) (Instance, error) {
	if spec.Name == "" || spec.InstanceType == "" {
		return Instance{}, errFakeLaunch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ttl := spec.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	grace := spec.IdleGrace
	if grace == 0 {
		grace = defaultKeepWarmGrace
	}
	inst := Instance{
		ID:           fakeInstanceID(s.seq),
		Name:         spec.Name,
		InstanceType: spec.InstanceType,
		Region:       orDefault(spec.Region, "us-east-1"),
		State:        "running",
		// A documentation-range address (RFC 5737 TEST-NET-3), not a hostname:
		// spawn reports public_ip and has no public_dns field at all. The fake
		// deliberately hands back the same *shape* the real CLI does — supplying a
		// plausible-looking value of the wrong shape is what let the empty-worker-
		// host bug sit undetected behind a green test suite.
		PublicIP:    fmt.Sprintf("203.0.113.%d", s.seq%254+1),
		LaunchedAt:  s.now,
		TTL:         ttl,
		IdleTimeout: grace,
	}
	s.inst[inst.ID] = inst
	s.idle[inst.ID] = s.now.Add(grace)
	return inst, nil
}

// resolve finds an instance by id or by Name, mirroring real spawn: `resolveInstance`
// treats an identifier starting with "i-" as an exact instance id and anything else as a
// case-insensitive Name match (spawn cmd/utils.go). foray depends on the name half — a
// session id is the task id, which becomes the instance's Name tag, and that is the handle
// `foray stop`, the end-of-rung reap and export ownership all pass. A fake that only knew
// ids would fail every one of those while staying green.
//
// Caller holds the lock.
func (s *fakeSpawn) resolve(identifier string) (Instance, bool) {
	if strings.HasPrefix(identifier, "i-") {
		inst, ok := s.inst[identifier]
		return inst, ok
	}
	for _, inst := range s.inst {
		if strings.EqualFold(inst.Name, identifier) {
			return inst, true
		}
	}
	return Instance{}, false
}

func (s *fakeSpawn) Status(_ context.Context, instanceID string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.resolve(instanceID)
	if !ok {
		return Instance{}, errFakeUnknown
	}
	return inst, nil
}

func (s *fakeSpawn) List(_ context.Context) ([]Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.inst))
	for _, inst := range s.inst {
		out = append(out, inst)
	}
	// Deterministic order for the rehearsal (the map iteration order is not).
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *fakeSpawn) Terminate(_ context.Context, instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.resolve(instanceID)
	if !ok {
		return errFakeUnknown
	}
	inst.State = "terminated"
	s.inst[inst.ID] = inst
	return nil
}

func (s *fakeSpawn) KeepWarm(_ context.Context, instanceID string, lastRequest time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.resolve(instanceID)
	if !ok {
		return errFakeUnknown
	}
	// Model what a trace does to the instance's own idle timer: the connection on
	// an --active-ports port counts as activity, so the deadline moves to
	// lastRequest + the session's idle window. The real adapter makes no CLI call
	// here precisely because the instance does this itself.
	grace := inst.IdleTimeout
	if grace == 0 {
		grace = defaultKeepWarmGrace
	}
	// Keyed by the instance's own id, since IdleDeadline is an instance-level probe.
	s.idle[inst.ID] = lastRequest.Add(grace)
	return nil
}

// --- spawn service fake -----------------------------------------------------

// fakeServer stands in for `spawn service`: it hands back a loopback URL with a
// token, the same shape the real tunnel reports, so the offline loop exercises
// the token-splitting path rather than a bare host:port that would hide it.
type fakeServer struct {
	mu   sync.Mutex
	seq  int
	open int // live tunnels, so a test can assert Stop() actually ran
}

func (s *fakeServer) Serve(_ context.Context, spec ServeSpec) (*Service, error) {
	if spec.InstanceID == "" || len(spec.Command) == 0 {
		return nil, errFakeServe
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	port := 54320 + s.seq
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	s.open++
	return &Service{
		URL:        fmt.Sprintf("http://%s/?token=fake-token-%06d", addr, s.seq),
		Addr:       addr,
		InstanceID: spec.InstanceID,
		stop: func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.open > 0 {
				s.open--
			}
			return nil
		},
	}, nil
}

// OpenTunnels reports how many fake tunnels are currently open. Test-only
// observability for "the session closed its tunnel".
func (s *fakeServer) OpenTunnels() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// --- lagotto fake -----------------------------------------------------------

type fakeLagotto struct{}

func (fakeLagotto) Watch(_ context.Context, spec WatchSpec) (CapacityWatch, error) {
	if spec.InstanceType == "" {
		return CapacityWatch{}, errFakeWatch
	}
	region := "us-east-1"
	if len(spec.Regions) > 0 {
		region = spec.Regions[0]
	}
	return CapacityWatch{
		ID:           "fake-watch-" + spec.InstanceType,
		InstanceType: spec.InstanceType,
		State:        "active",
		Region:       region,
	}, nil
}

func (fakeLagotto) List(_ context.Context) ([]CapacityWatch, error) {
	return []CapacityWatch{}, nil
}

func (fakeLagotto) Status(_ context.Context, watchID string) (CapacityWatch, error) {
	return CapacityWatch{ID: watchID, State: "active", Region: "us-east-1"}, nil
}

// --- shared fake helpers ----------------------------------------------------

func fakeInstanceID(seq int) string { return "i-fake" + pad6(seq) }

// pad6 zero-pads a small sequence into a 6-char suffix, mimicking an EC2 id tail
// without importing fmt just for one call site.
func pad6(n int) string {
	s := []byte("000000")
	for i := len(s) - 1; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// --- spawn task run fake ----------------------------------------------------

// fakeTask stands in for `spawn task run`. It records launched specs and reports a
// task as pending on its first status check and complete on the next — deliberately,
// so the offline rehearsal exercises the caller's poll loop instead of letting it rot
// (same reasoning as gateway's fake handoff).
type fakeTask struct {
	mu       sync.Mutex
	launched map[string]TaskSpec
	polls    map[string]int
	// spawn is the shared instance table, so a launched task is a visible session.
	spawn Spawn
	// exitCode is what a completed task reports; non-zero drives the failure path.
	exitCode int
	// runErr fails the launch itself.
	runErr error
}

func newFakeTask(sp Spawn) *fakeTask {
	return &fakeTask{launched: map[string]TaskSpec{}, polls: map[string]int{}, spawn: sp}
}

func (f *fakeTask) Run(ctx context.Context, spec TaskSpec) (TaskRun, error) {
	if err := spec.validate(); err != nil {
		return TaskRun{}, err
	}
	if f.runErr != nil {
		return TaskRun{}, f.runErr
	}
	f.mu.Lock()
	f.launched[spec.TaskID] = spec
	f.mu.Unlock()

	// An instance appears, named for the task — what real spawn does, and what makes the
	// session resolvable by every lifecycle verb. The fake's instance ids ARE the name,
	// matching foray's session-id-is-instance-id model.
	if f.spawn != nil {
		if _, err := f.spawn.Launch(ctx, LaunchSpec{
			Name:         spec.TaskID,
			InstanceType: spec.Resources.InstanceType,
			TTL:          parseTaskTTL(spec.Lifecycle.TTL),
		}); err != nil {
			return TaskRun{}, fmt.Errorf("spawn task run %s (fake): %w", spec.TaskID, err)
		}
	}
	return TaskRun{TaskID: spec.TaskID}, nil
}

// parseTaskTTL reads the lifecycle TTL back into a duration for the fake instance table.
// An unparseable value cannot happen (validate rejects an empty one and the executor
// renders a time.Duration), so it degrades to an hour rather than failing a rehearsal.
func parseTaskTTL(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

func (f *fakeTask) Status(_ context.Context, taskID string) (TaskCompletion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.launched[taskID]; !ok {
		// spawn answers "no completion record yet" for an unknown task rather than
		// erroring, so a never-launched task reads as pending here too.
		return TaskCompletion{TaskID: taskID}, nil
	}
	f.polls[taskID]++
	if f.polls[taskID] < 2 {
		return TaskCompletion{TaskID: taskID}, nil
	}
	state := "completed"
	if f.exitCode != 0 {
		state = "failed"
	}
	return TaskCompletion{TaskID: taskID, State: state, ExitCode: f.exitCode, Complete: true}, nil
}

// Spec returns a launched task's spec, for tests asserting what foray asked spawn for.
func (f *fakeTask) Spec(taskID string) (TaskSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.launched[taskID]
	return s, ok
}

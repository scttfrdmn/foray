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

package gateway

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/scttfrdmn/foray/internal/spore"
)

// NewFake builds a gateway with no AWS: an in-memory session store, a worker
// that returns a canned result reference, and the spore fake spawn driving the
// idle bridge. Same pattern as internal/spore, internal/export, internal/brain
// (FORAY_FAKE=1) — the dev/rehearse path and the CI gate, zero AWS calls.
//
// The fake seeds one session — launched through the spore fake spawn so the
// instance exists for KeepWarm — and the rehearsal can route a trace
// immediately. The caller can Put more via the returned store.
func NewFake() (*Gateway, *MemStore) {
	sp := spore.NewFake().Spawn
	// Launch through the spore fake so the instance is in its table and the
	// idle bridge's KeepWarm finds it (rather than guessing an instance id).
	inst, _ := sp.Launch(context.Background(), spore.LaunchSpec{
		Name:         FakeSessionID,
		InstanceType: "g7e.2xlarge",
	})
	store := NewMemStore()
	_ = store.Put(context.Background(), Session{
		ID:         FakeSessionID,
		InstanceID: inst.ID,
		WorkerURL:  "http://worker.fake.spore.host:8000",
	})
	g := &Gateway{
		Store:  store,
		Worker: fakeWorker{},
		Spawn:  sp,
		Now:    func() time.Time { return fakeNow },
	}
	return g, store
}

// FakeSessionID is the seeded session in the fake gateway.
const FakeSessionID = "sess-fake000001"

// fakeNow is a fixed clock so routed traces stamp a deterministic
// last_request_time across runs of make demo-fake.
var fakeNow = time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

// fakeWorker returns a stable result reference — pixels and an s3:// save ref,
// never tensors, honoring the no-automatic-egress invariant even offline.
type fakeWorker struct{}

func (fakeWorker) Run(_ context.Context, _ string, _ Graph) (TraceResult, error) {
	return TraceResult{
		SaveRef: "s3://your-bucket-us-east-1/sessions/" + FakeSessionID + "/activations/",
		VizRef:  "viz://" + FakeSessionID + "/logit-lens.png",
		NNSight: "with model.trace(prompt) as t:\n    logits = model.lm_head.output.save()",
	}, nil
}

// MemStore is the in-memory session<->instance map for the fake and for local
// runs. It implements the optional enumerator capability so /healthz can report
// the freshest last_request_time. Safe for concurrent use.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]Session
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{sessions: map[string]Session{}} }

func (m *MemStore) Get(_ context.Context, sessionID string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return Session{}, ErrUnknownSession
	}
	return s, nil
}

func (m *MemStore) Put(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *MemStore) Touch(_ context.Context, sessionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return ErrUnknownSession
	}
	s.LastRequest = at
	m.sessions[sessionID] = s
	return nil
}

// List enumerates sessions (the enumerator capability /healthz uses), ordered by
// id for a deterministic health payload.
func (m *MemStore) List(_ context.Context) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

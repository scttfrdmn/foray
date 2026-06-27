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

package webapi

import (
	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/export"
	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/spore"
)

// NewFakeDeps wires the offline collaborators for FORAY_FAKE=1 — the dev
// server's rehearsal mode and the package's test gate. It is the cmd/foray
// buildFakeDeps shape: one shared fake spawn so the brain's executor and the
// gateway's idle bridge see the same instance table, the brain's canned
// GPT-2→8B ladder, the gateway's canned worker, and the fake exporter. Zero AWS.
func NewFakeDeps() Deps {
	f := spore.NewFake()
	gw := &gateway.Gateway{
		Store:  gateway.NewMemStore(),
		Worker: gateway.NewFakeWorker(),
		Spawn:  f.Spawn,
	}
	return Deps{
		Brain:    brain.NewFakeWith(f.Spawn),
		Gateway:  gw,
		Spawn:    f.Spawn,
		Exporter: export.NewFake(),
	}
}

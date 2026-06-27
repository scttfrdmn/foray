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
	"fmt"
)

// Lagotto wraps the lagotto binary: a capacity watcher (Lambda) for the scarce
// multi-GPU case. It is NDIF's former "warm" tier reframed as watch-and-grab —
// you don't keep a large instance hot, you watch for capacity to appear and grab
// it (ARCHITECTURE.md §6.6). Only the large tier ever needs this; the cheap path
// launches directly via spawn. Do not reimplement it.
type Lagotto interface {
	// Watch registers a capacity watch for an instance type and returns its
	// handle. lagotto notifies (and can launch) when capacity appears.
	Watch(ctx context.Context, spec WatchSpec) (CapacityWatch, error)
	// List returns the account's active watches.
	List(ctx context.Context) ([]CapacityWatch, error)
	// Status reports a single watch's current state.
	Status(ctx context.Context, watchID string) (CapacityWatch, error)
}

// WatchSpec describes a capacity watch to register.
type WatchSpec struct {
	InstanceType string   // the scarce type to watch for, e.g. "p5e.48xlarge"
	Regions      []string // empty → lagotto's default region set
	Spot         bool     // watch Spot capacity rather than On-Demand
}

// CapacityWatch is a lagotto watch handle.
type CapacityWatch struct {
	ID           string `json:"watch_id"`
	InstanceType string `json:"instance_type"`
	State        string `json:"state"` // active | matched | canceled | expired
	Region       string `json:"region"`
}

// lagotto is the real adapter: it execs the CLI with `-o json` and parses the
// result.
type lagotto struct{ run Runner }

// NewLagotto returns a Lagotto backed by the real lagotto binary.
func NewLagotto(r Runner) Lagotto { return lagotto{run: r} }

func (l lagotto) Watch(ctx context.Context, spec WatchSpec) (CapacityWatch, error) {
	if spec.InstanceType == "" {
		return CapacityWatch{}, fmt.Errorf("lagotto watch: InstanceType is required")
	}
	args := []string{"watch", spec.InstanceType, "-o", "json"}
	if len(spec.Regions) > 0 {
		args = append(args, "--regions", joinCSV(spec.Regions))
	}
	if spec.Spot {
		args = append(args, "--spot")
	}
	out, err := l.run.Run(ctx, "lagotto", args...)
	if err != nil {
		return CapacityWatch{}, fmt.Errorf("lagotto watch %s: %w", spec.InstanceType, err)
	}
	var w CapacityWatch
	if err := json.Unmarshal(out, &w); err != nil {
		return CapacityWatch{}, fmt.Errorf("lagotto watch %s: parse json: %w", spec.InstanceType, err)
	}
	return w, nil
}

func (l lagotto) List(ctx context.Context) ([]CapacityWatch, error) {
	out, err := l.run.Run(ctx, "lagotto", "list", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("lagotto list: %w", err)
	}
	var watches []CapacityWatch
	if err := json.Unmarshal(out, &watches); err != nil {
		return nil, fmt.Errorf("lagotto list: parse json: %w", err)
	}
	return watches, nil
}

func (l lagotto) Status(ctx context.Context, watchID string) (CapacityWatch, error) {
	out, err := l.run.Run(ctx, "lagotto", "status", watchID, "-o", "json")
	if err != nil {
		return CapacityWatch{}, fmt.Errorf("lagotto status %s: %w", watchID, err)
	}
	var w CapacityWatch
	if err := json.Unmarshal(out, &w); err != nil {
		return CapacityWatch{}, fmt.Errorf("lagotto status %s: parse json: %w", watchID, err)
	}
	return w, nil
}

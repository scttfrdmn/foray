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
	"time"
)

// errNoExecInLambda is returned by DynamoIdleBridge for the launch/lifecycle
// methods, which the gateway never calls — only KeepWarm. It makes a misuse
// (someone wiring this where Launch is needed) fail loudly rather than silently.
var errNoExecInLambda = errors.New("spore: this Spawn is an idle-bridge shim (KeepWarm only); use the exec-backed Spawn for launch/lifecycle")

// DynamoIdleBridge is the Spawn the deployed gateway (forayd) Lambda uses for the
// idle bridge. In a provided.al2023 Lambda the `spawn` binary isn't present, so
// the exec-backed KeepWarm (`spawn extend`) can't run. But it doesn't need to:
// the gateway's Store.Touch already writes per-session last_request_time to
// DynamoDB, and in the deployed control plane THAT timestamp is the durable idle
// signal — a spawn-side consumer reads it instead of the gateway shelling out
// (ARCHITECTURE.md §6.1; the contract is the timestamp, not the mechanism). So
// KeepWarm here is a no-op: Touch has already recorded the activity.
//
// Only KeepWarm is implemented because the gateway only calls KeepWarm
// (gateway.bridgeIdle). The launch/status/list/terminate methods return an error
// so misuse surfaces immediately.
type DynamoIdleBridge struct{}

// NewDynamoIdleBridge returns the KeepWarm-only Spawn for the gateway Lambda.
func NewDynamoIdleBridge() Spawn { return DynamoIdleBridge{} }

// KeepWarm is a no-op: the durable last_request_time is already in DynamoDB via
// the gateway's Store.Touch, which runs before this in bridgeIdle.
func (DynamoIdleBridge) KeepWarm(_ context.Context, _ string, _ time.Time) error { return nil }

func (DynamoIdleBridge) Launch(_ context.Context, _ LaunchSpec) (Instance, error) {
	return Instance{}, errNoExecInLambda
}
func (DynamoIdleBridge) Status(_ context.Context, _ string) (Instance, error) {
	return Instance{}, errNoExecInLambda
}
func (DynamoIdleBridge) List(_ context.Context) ([]Instance, error)  { return nil, errNoExecInLambda }
func (DynamoIdleBridge) Terminate(_ context.Context, _ string) error { return errNoExecInLambda }

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

package deploy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWorkerRepoCreateIsIdempotent(t *testing.T) {
	f := newFakeECR()
	r := &workerRepo{api: f, repo: DefaultWorkerRepo}
	ctx := context.Background()

	first, err := r.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if first.Op != OpCreate {
		t.Errorf("first ensure = %s, want create", first.Op)
	}
	second, err := r.ensure(ctx)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Op != OpExists {
		t.Errorf("second ensure = %s, want exists — there is no state file, so Apply must be re-runnable", second.Op)
	}
}

// Of everything this package creates, the repository is the one resource that grows with
// use: every push adds layers and nothing removes the old ones. So the lifecycle policy is
// not decoration — without it the repository slowly becomes the largest line on the bill
// of a control plane that is supposed to rest at ~$0.
func TestWorkerRepoIsBounded(t *testing.T) {
	f := newFakeECR()
	r := &workerRepo{api: f, repo: DefaultWorkerRepo}
	if _, err := r.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	text, ok := f.lifecycle[DefaultWorkerRepo]
	if !ok {
		t.Fatal("no lifecycle policy — the repository would keep every layer forever")
	}
	var policy struct {
		Rules []struct {
			RulePriority int `json:"rulePriority"`
			Selection    struct {
				TagStatus string `json:"tagStatus"`
				CountType string `json:"countType"`
			} `json:"selection"`
			Action struct{ Type string } `json:"action"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(text), &policy); err != nil {
		t.Fatalf("lifecycle policy is not valid JSON (ECR would reject it): %v", err)
	}
	if len(policy.Rules) != 2 {
		t.Fatalf("got %d rules, want the untagged sweep plus the keep-recent trim", len(policy.Rules))
	}
	if policy.Rules[0].Selection.TagStatus != "untagged" {
		t.Errorf("rule 1 selects %q, want untagged first — ECR applies rules by ascending priority",
			policy.Rules[0].Selection.TagStatus)
	}
	for i, rule := range policy.Rules {
		if rule.Action.Type != "expire" {
			t.Errorf("rule %d action = %q, want expire", i+1, rule.Action.Type)
		}
	}
}

// A converge on a repository that already exists must still install the policy: one made
// by an older deploy, or by hand, would otherwise be unbounded.
func TestWorkerRepoConvergesAnExistingRepository(t *testing.T) {
	f := newFakeECR()
	f.repos[DefaultWorkerRepo] = true // pre-existing, no lifecycle policy
	r := &workerRepo{api: f, repo: DefaultWorkerRepo}

	act, err := r.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpExists {
		t.Errorf("Op = %s, want exists", act.Op)
	}
	if _, ok := f.lifecycle[DefaultWorkerRepo]; !ok {
		t.Error("a pre-existing repository was left unbounded")
	}
}

// Teardown must take the images with it. A repository full of multi-GB layers is a
// standing storage charge, and the image is a build artifact `make worker-push` recreates
// — not the user's data.
func TestWorkerRepoRemoveDeletesImages(t *testing.T) {
	f := newFakeECR()
	r := &workerRepo{api: f, repo: DefaultWorkerRepo}
	ctx := context.Background()
	if _, err := r.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	act, err := r.remove(ctx)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if act.Op != OpDelete {
		t.Errorf("Op = %s, want delete", act.Op)
	}
	if f.repos[DefaultWorkerRepo] {
		t.Error("repository survived teardown")
	}

	// Removing twice is a no-op, not an error: teardown must be safe to re-run.
	again, err := r.remove(ctx)
	if err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if again.Op != OpAbsent {
		t.Errorf("second remove = %s, want absent", again.Op)
	}
}

// The image URI is the one string the control plane and `make worker-push` must agree on.
// A mismatch is an instance that pulls nothing, discovered after the GPU is already
// billing — so it is built in one place and asserted here.
func TestWorkerImageURI(t *testing.T) {
	cfg := Config{Region: "us-west-2", AccountID: "123456789012", WorkerRepo: DefaultWorkerRepo}
	want := "123456789012.dkr.ecr.us-west-2.amazonaws.com/foray-worker:dev"
	if got := cfg.WorkerImageURI("dev"); got != want {
		t.Errorf("WorkerImageURI = %q, want %q", got, want)
	}
	if got := cfg.WorkerImageURI(""); got != want {
		t.Errorf("WorkerImageURI(\"\") = %q, want the default tag %q", got, DefaultWorkerTag)
	}
	if got := cfg.WorkerImageURI("sha-abc"); !strings.HasSuffix(got, ":sha-abc") {
		t.Errorf("WorkerImageURI = %q, want the given tag", got)
	}
}

// The Makefile pushes to the repository this package creates. Nothing in Go can check a
// make variable, so the drift guard reads it — the same technique the Cedar and handoff-key
// guards use.
func TestMakefileAgreesOnTheRepositoryAndTag(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	mk := string(b)
	for _, want := range []string{
		"WORKER_REPO  ?= " + DefaultWorkerRepo,
		"WORKER_TAG   ?= " + DefaultWorkerTag,
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Makefile does not define %q — `make worker-push` would push somewhere "+
				"the control plane does not launch from", want)
		}
	}
}

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
	"errors"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Region:     "us-west-2",
		WebBucket:  "foray-web-123",
		DataBucket: "foray-data-123",
		AccountID:  "123456789012",
	}
}

// resourceCount is how many resources a full Apply touches: the sessions table, two
// buckets, three IAM roles, the spawn instance profile, three log groups, two Lambda
// functions and the HTTP API.
const resourceCount = 13

// testFakes bundles the stand-ins so tests can reach whichever they assert on.
type testFakes struct {
	ddb   *fakeDynamo
	s3    *fakeS3
	iam   *fakeIAM
	lam   *fakeLambda
	logs  *fakeLogs
	apigw *fakeAPIGW
}

// newTestDeployer builds the real resource list over fakes.
func newTestDeployer(t *testing.T, cfg Config) (*Deployer, *testFakes) {
	t.Helper()
	f := &testFakes{
		ddb:   newFakeDynamo(),
		s3:    newFakeS3(),
		iam:   newFakeIAM(),
		lam:   newFakeLambda(),
		logs:  newFakeLogs(),
		apigw: newFakeAPIGW(),
	}
	cfg = mustValidate(t, cfg)
	if cfg.LWALayerARN == "" {
		cfg.LWALayerARN = lwaLayerBase(cfg.Region) + ":25"
	}
	zip := func(path string) ([]byte, error) { return []byte("zip:" + path), nil }
	d := newWithZips(cfg, f.ddb, f.s3, f.iam, f.lam, f.logs, f.apigw, zip)
	return d, f
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantErr   bool
		wantTable string
	}{
		{
			name:      "complete config defaults the table name",
			cfg:       Config{Region: "us-west-2", WebBucket: "w", DataBucket: "d"},
			wantTable: DefaultSessionsTable,
		},
		{
			name:      "explicit table name is kept",
			cfg:       Config{Region: "us-west-2", WebBucket: "w", DataBucket: "d", SessionsTable: "custom"},
			wantTable: "custom",
		},
		{"no region", Config{WebBucket: "w", DataBucket: "d"}, true, ""},
		{"no web bucket", Config{Region: "us-west-2", DataBucket: "d"}, true, ""},
		{"no data bucket", Config{Region: "us-west-2", WebBucket: "w"}, true, ""},
		{
			// They have different access postures, so one bucket for both would widen
			// access to the user's saved activations.
			name:    "web and data buckets must differ",
			cfg:     Config{Region: "us-west-2", WebBucket: "same", DataBucket: "same"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				if !errors.Is(err, ErrConfig) {
					t.Errorf("error %v does not wrap ErrConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if cfg.SessionsTable != tt.wantTable {
				t.Errorf("SessionsTable = %q, want %q", cfg.SessionsTable, tt.wantTable)
			}
		})
	}
}

// Every resource must carry Project=foray. scripts/teardown-verify.sh asserts on
// exactly that tag, so an untagged resource is invisible to the "nothing is left
// billing" check — the one way this package can fail silently. Terraform got this
// from default_tags; here it is per-call.
func TestTagsAlwaysCarryProject(t *testing.T) {
	tags := Tags("foray-sessions")
	if tags[TagProject] != TagProjectValue {
		t.Errorf("%s = %q, want %q — teardown-verify would not see this resource",
			TagProject, tags[TagProject], TagProjectValue)
	}
	if tags[TagManagedBy] != TagManagedByValue {
		t.Errorf("%s = %q, want %q", TagManagedBy, tags[TagManagedBy], TagManagedByValue)
	}
	if tags["Name"] != "foray-sessions" {
		t.Errorf("Name = %q", tags["Name"])
	}
}

// Apply creates what is missing; a second Apply over the same account creates
// nothing. Without a state file this is the property the whole package rests on —
// a half-finished deploy is fixed by running it again.
func TestApplyIsIdempotent(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	first, err := d.Apply(context.Background())
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if len(first) != resourceCount {
		t.Fatalf("got %d actions, want %d", len(first), resourceCount)
	}
	for _, a := range first {
		if a.Op != OpCreate {
			t.Errorf("first Apply: %s %s → %s, want create", a.Kind, a.Name, a.Op)
		}
	}

	second, err := d.Apply(context.Background())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, a := range second {
		if a.Op != OpExists {
			t.Errorf("second Apply: %s %s → %s, want exists", a.Kind, a.Name, a.Op)
		}
	}
}

// Teardown removes what Apply made, and a second Teardown finds nothing to do.
func TestTeardownRemovesEverythingAndIsIdempotent(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	ddb, s3c := f.ddb, f.s3

	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	first, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, a := range first {
		if a.Op != OpDelete {
			t.Errorf("Teardown: %s %s → %s, want delete", a.Kind, a.Name, a.Op)
		}
	}
	if len(ddb.tables) != 0 {
		t.Errorf("%d table(s) left after teardown", len(ddb.tables))
	}
	if len(s3c.buckets) != 0 {
		t.Errorf("%d bucket(s) left after teardown", len(s3c.buckets))
	}

	second, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	for _, a := range second {
		if a.Op != OpAbsent {
			t.Errorf("second Teardown: %s %s → %s, want absent", a.Kind, a.Name, a.Op)
		}
	}
}

// Teardown runs in reverse dependency order, so later resources come off before
// the ones they were built on.
func TestTeardownReversesApplyOrder(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	applied, err := d.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	removed, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(applied) != len(removed) {
		t.Fatalf("applied %d, removed %d", len(applied), len(removed))
	}
	for i := range applied {
		want := applied[len(applied)-1-i].Name
		if removed[i].Name != want {
			t.Errorf("teardown[%d] = %q, want %q (reverse order)", i, removed[i].Name, want)
		}
	}
}

// Teardown must not stop at the first failure: the requirement is that nothing is
// left billing, so one stubborn resource cannot shield the rest.
func TestTeardownContinuesPastAFailure(t *testing.T) {
	cfg := mustValidate(t, testConfig())
	d, f := newTestDeployer(t, cfg)
	ddb, s3c := f.ddb, f.s3
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The data bucket is removed first (reverse order); make it fail.
	s3c.failDelete[cfg.DataBucket] = errors.New("bucket not empty")

	actions, err := d.Teardown(context.Background())
	if err == nil {
		t.Fatal("want the failure reported")
	}
	if !strings.Contains(err.Error(), cfg.DataBucket) {
		t.Errorf("error %q does not name the failed resource", err)
	}
	// Everything else still went.
	if _, ok := s3c.buckets[cfg.WebBucket]; ok {
		t.Error("web bucket survived — teardown stopped early")
	}
	if len(ddb.tables) != 0 {
		t.Error("table survived — teardown stopped early")
	}
	if len(actions) != resourceCount {
		t.Errorf("got %d actions, want %d (every resource attempted)", len(actions), resourceCount)
	}
}

// Apply stops at the first failure, since later resources generally depend on
// earlier ones — and it returns what it managed to do, because there is no state
// file for the caller to consult.
func TestApplyStopsAtFirstFailureAndReportsProgress(t *testing.T) {
	cfg := mustValidate(t, testConfig())
	d, f := newTestDeployer(t, cfg)
	s3c := f.s3
	s3c.failCreate[cfg.WebBucket] = errors.New("boom")
	actions, err := d.Apply(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	// The table (first in order) was created before the failing bucket.
	if len(actions) != 1 || actions[0].Kind != "dynamodb table" {
		t.Fatalf("actions = %+v, want just the table", actions)
	}
	if !strings.Contains(err.Error(), cfg.WebBucket) {
		t.Errorf("error %q does not name the failed resource", err)
	}
}

// A dry run reports the plan and touches nothing.
func TestDryRunMutatesNothing(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	ddb, s3c := f.ddb, f.s3
	d.DryRun = true

	actions, err := d.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(actions) != resourceCount {
		t.Fatalf("got %d actions, want %d", len(actions), resourceCount)
	}
	for _, a := range actions {
		if a.Op != OpPlan {
			t.Errorf("%s → %s, want plan", a.Name, a.Op)
		}
	}
	if len(ddb.tables) != 0 || len(s3c.buckets) != 0 {
		t.Error("dry run created something")
	}
	if ddb.creates != 0 || s3c.creates != 0 {
		t.Errorf("dry run called mutating APIs (%d table creates, %d bucket creates)", ddb.creates, s3c.creates)
	}
}

func mustValidate(t *testing.T, c Config) Config {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return c
}

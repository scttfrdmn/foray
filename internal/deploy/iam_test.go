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
	"strings"
	"testing"
)

const testAccount = "123456789012"

// --- lifecycle --------------------------------------------------------------

// The three roles and the instance profile are created, and the profile holds the
// spawn role.
func TestIAMRolesCreated(t *testing.T) {
	d, _, _, iamc := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []string{RoleGatewayLambda, RoleWebAPILambda, RoleSpawnInstance} {
		r, ok := iamc.roles[want]
		if !ok {
			t.Fatalf("role %s not created", want)
		}
		if r.tags[TagProject] != TagProjectValue {
			t.Errorf("role %s missing the Project tag — teardown-verify would not see it", want)
		}
	}
	p, ok := iamc.profiles[RoleSpawnInstance]
	if !ok {
		t.Fatal("instance profile not created")
	}
	if _, held := p.roles[RoleSpawnInstance]; !held {
		t.Error("instance profile does not hold the spawn role — spawn could not launch with it")
	}
}

// IAM refuses to delete a role that still carries inline or attached policies, and
// refuses to delete an instance profile that still holds a role. Teardown must strip
// those first. The fakes model both refusals, so this test would fail if the
// ordering were wrong.
func TestIAMTeardownStripsPoliciesAndProfileFirst(t *testing.T) {
	d, _, _, iamc := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(iamc.roles) != 0 {
		left := make([]string, 0, len(iamc.roles))
		for n := range iamc.roles {
			left = append(left, n)
		}
		t.Errorf("roles left after teardown: %v", left)
	}
	if len(iamc.profiles) != 0 {
		t.Errorf("%d instance profile(s) left after teardown", len(iamc.profiles))
	}
}

// The instance profile must be torn down before the role it holds, or the role
// deletion is refused. Asserted on the action order rather than the end state, so a
// regression is caught even if a retry happened to paper over it.
func TestInstanceProfileRemovedBeforeItsRole(t *testing.T) {
	d, _, _, _ := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	actions, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	profileAt, roleAt := -1, -1
	for i, a := range actions {
		switch {
		case a.Kind == "instance profile":
			profileAt = i
		case a.Kind == "iam role" && a.Name == RoleSpawnInstance:
			roleAt = i
		}
	}
	if profileAt < 0 || roleAt < 0 {
		t.Fatalf("missing actions: profile=%d role=%d", profileAt, roleAt)
	}
	if profileAt > roleAt {
		t.Errorf("instance profile removed at %d, after its role at %d — IAM would refuse the role deletion", profileAt, roleAt)
	}
}

// A role that drifted — extra inline policies added out of band — must still come
// off cleanly, because teardown enumerates what is actually attached rather than
// assuming its own list.
func TestIAMTeardownRemovesDriftedPolicies(t *testing.T) {
	d, _, _, iamc := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	iamc.roles[RoleGatewayLambda].inline["added-by-hand"] = `{"Version":"2012-10-17"}`
	iamc.roles[RoleGatewayLambda].attached["arn:aws:iam::aws:policy/ReadOnlyAccess"] = struct{}{}

	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, ok := iamc.roles[RoleGatewayLambda]; ok {
		t.Error("drifted role survived teardown")
	}
}

// Re-applying converges rather than failing: AddRoleToInstanceProfile errors if the
// profile already holds a role, so ensure must check first.
func TestInstanceProfileReapplyDoesNotReAddRole(t *testing.T) {
	d, _, _, iamc := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply should converge, not fail: %v", err)
	}
	if n := len(iamc.profiles[RoleSpawnInstance].roles); n != 1 {
		t.Errorf("profile holds %d roles, want 1", n)
	}
}

// A drifted trust policy is a role nothing can assume, which fails at invoke time
// rather than deploy time — so Apply converges it.
func TestRoleTrustPolicyConverges(t *testing.T) {
	d, _, _, iamc := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	iamc.roles[RoleGatewayLambda].assumeDoc = `{"Version":"2012-10-17","Statement":[]}`

	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	doc, err := decodePolicyDocument(iamc.roles[RoleGatewayLambda].assumeDoc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("trust policy not restored: %+v", doc)
	}
	principal, _ := json.Marshal(doc.Statement[0].Principal)
	if !strings.Contains(string(principal), "lambda.amazonaws.com") {
		t.Errorf("trust principal = %s, want lambda.amazonaws.com", principal)
	}
}

// --- policy scoping ---------------------------------------------------------
//
// These are the tests that matter most in this increment: a too-wide grant is not a
// broken deploy, it is a quiet over-permission that nothing else would catch.

// forayd gets one table and nothing else. Notably no Scan (nothing enumerates) and
// no DeleteItem (rows expire by TTL).
func TestGatewayRoleIsTableOnly(t *testing.T) {
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")
	r := gatewayRole(newFakeIAM(), tableARN)

	if len(r.inline) != 1 {
		t.Fatalf("gateway role has %d inline policies, want 1", len(r.inline))
	}
	doc := mustParse(t, r.inline[0].doc)
	actions := stringsOf(t, doc.Statement[0].Action)
	want := []string{"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:Query"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("actions = %v, want %v", actions, want)
	}
	for _, forbidden := range []string{"dynamodb:Scan", "dynamodb:DeleteItem", "dynamodb:*"} {
		for _, a := range actions {
			if a == forbidden {
				t.Errorf("gateway role grants %s — not needed, so not granted", forbidden)
			}
		}
	}
	if got := doc.Statement[0].Resource; got != tableARN {
		t.Errorf("resource = %v, want the one table ARN %s", got, tableARN)
	}
	// Bedrock, S3 and PassRole belong to the web API, not the gateway.
	all := r.inline[0].doc
	for _, forbidden := range []string{"bedrock:", "s3:", "iam:PassRole", "ec2:"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("gateway role policy mentions %q — it only needs the table", forbidden)
		}
	}
}

// The web API's S3 access is scoped to the session prefix, and the ListBucket
// statement is condition-scoped — without that condition it would grant a listing
// of the whole bucket, because ListBucket's resource is the bucket itself.
func TestWebAPIS3ScopedToSessionsPrefix(t *testing.T) {
	doc := mustParse(t, dataBucketSessionsPolicy("foray-data").doc)
	if len(doc.Statement) != 2 {
		t.Fatalf("want 2 statements (objects + conditional list), got %d", len(doc.Statement))
	}

	objects := doc.Statement[0]
	if got, want := objects.Resource, "arn:aws:s3:::foray-data/sessions/*"; got != want {
		t.Errorf("object resource = %v, want %v", got, want)
	}

	list := doc.Statement[1]
	if got, want := list.Resource, "arn:aws:s3:::foray-data"; got != want {
		t.Errorf("list resource = %v, want the bucket %v", got, want)
	}
	if list.Condition == nil {
		t.Fatal("ListBucket has no condition — that grants listing the entire bucket")
	}
	cond, _ := json.Marshal(list.Condition)
	if !strings.Contains(string(cond), "sessions/*") {
		t.Errorf("list condition = %s, want an s3:prefix of sessions/*", cond)
	}
}

// PassRole is the privilege-escalation-shaped grant here: it is narrowed to exactly
// the spawn role and, by condition, to EC2 — so it cannot hand that role (or any
// other) to anything else.
func TestPassRoleIsDoublyNarrowed(t *testing.T) {
	doc := mustParse(t, passSpawnRolePolicy(testAccount).doc)
	st := doc.Statement[0]

	wantARN := roleARN(testAccount, RoleSpawnInstance)
	if st.Resource != wantARN {
		t.Errorf("resource = %v, want exactly the spawn role %s", st.Resource, wantARN)
	}
	if st.Resource == "*" {
		t.Fatal("PassRole on * would let the web API hand any role to any service")
	}
	if st.Condition == nil {
		t.Fatal("PassRole has no PassedToService condition")
	}
	cond, _ := json.Marshal(st.Condition)
	if !strings.Contains(string(cond), "ec2.amazonaws.com") {
		t.Errorf("condition = %s, want iam:PassedToService ec2.amazonaws.com", cond)
	}
}

// The spawn instance can end its own life — the in-instance idle/TTL daemon needs
// that — but only for foray's own instances. The scope is a TAG condition, because
// the instance does not know its own id when the policy is written.
func TestSpawnSelfTerminateIsTagScoped(t *testing.T) {
	doc := mustParse(t, spawnSelfTerminatePolicy(testAccount).doc)
	st := doc.Statement[0]

	actions := stringsOf(t, st.Action)
	if strings.Join(actions, ",") != "ec2:TerminateInstances,ec2:StopInstances" {
		t.Errorf("actions = %v", actions)
	}
	if st.Condition == nil {
		t.Fatal("no tag condition — this would let the instance terminate ANY instance in the account")
	}
	cond, _ := json.Marshal(st.Condition)
	if !strings.Contains(cond2str(cond), "aws:ResourceTag/Project") || !strings.Contains(cond2str(cond), TagProjectValue) {
		t.Errorf("condition = %s, want aws:ResourceTag/Project = foray", cond)
	}
	// RunInstances would let a compromised worker start new GPUs — the expensive
	// direction. It must not be here.
	for _, a := range actions {
		if a == "ec2:RunInstances" || a == "ec2:*" {
			t.Errorf("spawn role grants %s — it may end its own life, not start new ones", a)
		}
	}
}

// The worker writes keys it already knows, so it never lists. Not granting
// ListBucket means a compromised worker cannot inventory other sessions.
func TestSpawnSavesPolicyGrantsNoListing(t *testing.T) {
	p := spawnSessionSavesPolicy("foray-data")
	if strings.Contains(p.doc, "ListBucket") {
		t.Error("spawn role grants s3:ListBucket — the worker writes known keys and never needs to enumerate")
	}
	doc := mustParse(t, p.doc)
	if got, want := doc.Statement[0].Resource, "arn:aws:s3:::foray-data/sessions/*"; got != want {
		t.Errorf("resource = %v, want %v", got, want)
	}
}

// Invoking through an inference profile needs both the profile ARN and the
// foundation-model ARN: either alone is denied. The region is a wildcard because a
// US profile routes across regions.
func TestBedrockPolicyCoversProfileAndModel(t *testing.T) {
	doc := mustParse(t, bedrockInvokePolicy(testAccount, DefaultPlanModelID).doc)
	resources := stringsOf(t, doc.Statement[0].Resource)
	if len(resources) != 2 {
		t.Fatalf("resources = %v, want the profile and the foundation models", resources)
	}
	if !strings.Contains(resources[0], "inference-profile/"+DefaultPlanModelID) {
		t.Errorf("first resource = %q, want the pinned inference profile", resources[0])
	}
	if !strings.Contains(resources[0], ":bedrock:*:") {
		t.Errorf("first resource = %q, want a wildcard region (a US profile routes across regions)", resources[0])
	}
	if !strings.Contains(resources[1], "foundation-model/*") {
		t.Errorf("second resource = %q, want the foundation models", resources[1])
	}
}

// Pricing is read-only and account-wide because there is no ARN for "the price of
// an instance type". Nothing here can launch or spend.
func TestPricingPolicyIsReadOnly(t *testing.T) {
	doc := mustParse(t, truffleSpotPricingPolicy().doc)
	st := doc.Statement[0]
	if st.Resource != "*" {
		t.Errorf("resource = %v, want * (describes have no ARN to scope to)", st.Resource)
	}
	for _, a := range stringsOf(t, st.Action) {
		if !strings.HasPrefix(a, "ec2:Describe") && a != "pricing:GetProducts" {
			t.Errorf("action %q is not a read-only describe", a)
		}
	}
}

// Both Lambda roles get the AWS-managed basic execution policy (their log group)
// and nothing else managed. The spawn role is not a Lambda and gets none.
func TestManagedPolicyAttachments(t *testing.T) {
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")
	f := newFakeIAM()
	tests := []struct {
		name string
		r    *role
		want []string
	}{
		{"gateway", gatewayRole(f, tableARN), []string{lambdaBasicExecutionPolicyARN}},
		{"webapi", webAPIRole(f, tableARN, "foray-data", testAccount, DefaultPlanModelID), []string{lambdaBasicExecutionPolicyARN}},
		{"spawn", spawnRole(f, "foray-data", testAccount), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.r.managed) != len(tt.want) {
				t.Fatalf("managed = %v, want %v", tt.r.managed, tt.want)
			}
			for i := range tt.want {
				if tt.r.managed[i] != tt.want[i] {
					t.Errorf("managed[%d] = %q, want %q", i, tt.r.managed[i], tt.want[i])
				}
			}
		})
	}
}

// Trust policies: the Lambda roles are assumable by Lambda, the spawn role by EC2.
// Getting these backwards produces a role nothing can assume.
func TestTrustPolicies(t *testing.T) {
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")
	f := newFakeIAM()
	tests := []struct {
		name    string
		doc     string
		service string
	}{
		{"gateway", gatewayRole(f, tableARN).assumeDoc, "lambda.amazonaws.com"},
		{"webapi", webAPIRole(f, tableARN, "d", testAccount, DefaultPlanModelID).assumeDoc, "lambda.amazonaws.com"},
		{"spawn", spawnRole(f, "d", testAccount).assumeDoc, "ec2.amazonaws.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParse(t, tt.doc)
			if doc.Statement[0].Action != "sts:AssumeRole" {
				t.Errorf("action = %v, want sts:AssumeRole", doc.Statement[0].Action)
			}
			p, _ := json.Marshal(doc.Statement[0].Principal)
			if !strings.Contains(string(p), tt.service) {
				t.Errorf("principal = %s, want %s", p, tt.service)
			}
		})
	}
}

// Every statement in every policy is an Allow. A stray Deny here would be a
// surprising, hard-to-debug interaction with an org SCP.
func TestNoPolicyUsesDeny(t *testing.T) {
	f := newFakeIAM()
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")
	roles := []*role{
		gatewayRole(f, tableARN),
		webAPIRole(f, tableARN, "foray-data", testAccount, DefaultPlanModelID),
		spawnRole(f, "foray-data", testAccount),
	}
	for _, r := range roles {
		for _, p := range r.inline {
			doc := mustParse(t, p.doc)
			for i, st := range doc.Statement {
				if st.Effect != "Allow" {
					t.Errorf("%s/%s statement %d has Effect %q, want Allow", r.roleName, p.name, i, st.Effect)
				}
			}
		}
	}
}

// --- helpers ----------------------------------------------------------------

func mustParse(t *testing.T, doc string) policyDoc {
	t.Helper()
	d, err := decodePolicyDocument(doc)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	if d.Version != policyVersion {
		t.Errorf("policy version = %q, want %q", d.Version, policyVersion)
	}
	if len(d.Statement) == 0 {
		t.Fatal("policy has no statements")
	}
	return d
}

// stringsOf normalizes an IAM field that may be a string or a list of strings.
func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				t.Fatalf("non-string element %#v", e)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("unexpected shape %#v", v)
		return nil
	}
}

func cond2str(b []byte) string { return string(b) }

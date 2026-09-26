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
	"fmt"
	"strings"
	"testing"
	"time"

	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// THE regression test for this increment. cmd/forayd binds :8080 and cmd/foray-web
// binds :8090, and LWA proxies each request to AWS_LWA_PORT — so one shared value
// leaves a function's readiness check failing forever, which surfaces as a SILENT
// 503 with nothing in the application log. That cost real time in PR #64.
func TestLWAPortIsPerFunction(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	gw, ok := f.lam.functions[FuncGateway]
	if !ok {
		t.Fatalf("%s not created", FuncGateway)
	}
	web, ok := f.lam.functions[FuncWebAPI]
	if !ok {
		t.Fatalf("%s not created", FuncWebAPI)
	}

	if got := gw.env["AWS_LWA_PORT"]; got != portGateway {
		t.Errorf("%s AWS_LWA_PORT = %q, want %q (cmd/forayd binds that port)", FuncGateway, got, portGateway)
	}
	if got := web.env["AWS_LWA_PORT"]; got != portWebAPI {
		t.Errorf("%s AWS_LWA_PORT = %q, want %q (cmd/foray-web binds that port)", FuncWebAPI, got, portWebAPI)
	}
	if gw.env["AWS_LWA_PORT"] == web.env["AWS_LWA_PORT"] {
		t.Fatal("both functions share one AWS_LWA_PORT — one of them will fail its readiness check and 503 silently")
	}
}

// The adapter only works if its wrapper and readiness path are set; without them the
// binary never gets proxied to and the function times out.
func TestLWAEnvPresentOnBothFunctions(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, name := range []string{FuncGateway, FuncWebAPI} {
		fn := f.lam.functions[name]
		if got := fn.env["AWS_LAMBDA_EXEC_WRAPPER"]; got != lwaExecWrapper {
			t.Errorf("%s AWS_LAMBDA_EXEC_WRAPPER = %q, want %q", name, got, lwaExecWrapper)
		}
		if got := fn.env["AWS_LWA_READINESS_CHECK_PATH"]; got != lwaReadinessURL {
			t.Errorf("%s readiness path = %q, want %q", name, got, lwaReadinessURL)
		}
		if len(fn.layers) != 1 || !strings.Contains(fn.layers[0], lwaLayerNameArm64) {
			t.Errorf("%s layers = %v, want the arm64 LWA layer", name, fn.layers)
		}
	}
}

// arm64 and provided.al2023 must match how the binaries are built (`make lambdas`
// cross-compiles GOARCH=arm64). An x86 layer or runtime mismatch fails at invoke
// time, not deploy time.
func TestFunctionsAreArm64ProvidedRuntime(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, name := range []string{FuncGateway, FuncWebAPI} {
		fn := f.lam.functions[name]
		if fn.runtime != lambdatypes.RuntimeProvidedal2023 {
			t.Errorf("%s runtime = %q, want provided.al2023", name, fn.runtime)
		}
		if len(fn.arch) != 1 || fn.arch[0] != lambdatypes.ArchitectureArm64 {
			t.Errorf("%s architectures = %v, want [arm64]", name, fn.arch)
		}
	}
}

// The web API shells out to the bundled truffle binary, which is not on the stock
// Lambda PATH — so /var/task/bin has to be on it or pricing fails at run time (the
// half of the deploy gap PR #67 fixed).
func TestWebAPIPathIncludesBundledBinDir(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	path := f.lam.functions[FuncWebAPI].env["PATH"]
	if !strings.Contains(path, "/var/task/bin") {
		t.Errorf("PATH = %q, want /var/task/bin so exec.LookPath finds the bundled truffle", path)
	}
	// The gateway never prices, so its zip is truffle-free and it needs no such PATH.
	if _, set := f.lam.functions[FuncGateway].env["PATH"]; set {
		t.Error("the gateway sets PATH but never prices — it carries no bundled binary")
	}
}

// Each function assumes its own role. Swapping them would give the gateway the web
// API's Bedrock and PassRole permissions, which is the whole point of having three.
func TestFunctionsUseTheirOwnRoles(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := f.lam.functions[FuncGateway].roleARN, roleARN(cfg.AccountID, RoleGatewayLambda); got != want {
		t.Errorf("%s role = %q, want %q", FuncGateway, got, want)
	}
	if got, want := f.lam.functions[FuncWebAPI].roleARN, roleARN(cfg.AccountID, RoleWebAPILambda); got != want {
		t.Errorf("%s role = %q, want %q", FuncWebAPI, got, want)
	}
}

// IAM is eventually consistent: a role created moments ago is often not yet
// assumable, and Lambda rejects it. Since this package creates the roles itself, a
// first deploy hits that window almost every time — so it must be retried rather
// than surfaced as a confusing failure.
func TestCreateFunctionRetriesIAMPropagation(t *testing.T) {
	f := newFakeLambda()
	f.roleNotReady = 3 // reject the first three attempts

	var slept int
	fn := &lambdaFunc{
		api:      f,
		funcName: FuncGateway,
		roleARN:  roleARN(testAccount, RoleGatewayLambda),
		zipPath:  "build/forayd.zip",
		layerARN: lwaLayerBase("us-west-2") + ":25",
		port:     portGateway,
		timeout:  600,
		memoryMB: 256,
		readZip:  func(string) ([]byte, error) { return []byte("zip"), nil },
		sleep:    func(time.Duration) { slept++ },
	}
	act, err := fn.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure should retry through IAM propagation: %v", err)
	}
	if act.Op != OpCreate {
		t.Errorf("op = %s, want create", act.Op)
	}
	if slept != 3 {
		t.Errorf("slept %d times, want 3 (one per rejected attempt)", slept)
	}
}

// A genuinely bad role must fail fast, not spend a minute retrying — otherwise a
// typo looks like a hang.
func TestCreateFunctionDoesNotRetryOtherErrors(t *testing.T) {
	f := newFakeLambda()
	f.createErr = &lambdatypes.InvalidParameterValueException{
		Message: stringPtr("Unzipped size must be smaller than 262144000 bytes"),
	}
	var slept int
	fn := &lambdaFunc{
		api:      f,
		funcName: FuncGateway,
		roleARN:  roleARN(testAccount, RoleGatewayLambda),
		readZip:  func(string) ([]byte, error) { return []byte("zip"), nil },
		sleep:    func(time.Duration) { slept++ },
	}
	if _, err := fn.ensure(context.Background()); err == nil {
		t.Fatal("want the error surfaced")
	}
	if slept != 0 {
		t.Errorf("slept %d times for a non-propagation error, want 0 — a typo should fail fast", slept)
	}
}

// Republishing an identical package on every deploy is waste; Lambda reports the
// deployed zip's sha256, so the comparison is exact.
func TestFunctionCodeUpdatedOnlyWhenItChanges(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if got := f.lam.functions[FuncGateway].codeUpdate; got != 0 {
		t.Fatalf("code updates after create = %d, want 0", got)
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := f.lam.functions[FuncGateway].codeUpdate; got != 0 {
		t.Errorf("code updates after an unchanged re-apply = %d, want 0", got)
	}

	// Now change the deployed code out from under us and confirm it converges.
	f.lam.functions[FuncGateway].codeSHA = "stale"
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("third Apply: %v", err)
	}
	if got := f.lam.functions[FuncGateway].codeUpdate; got != 1 {
		t.Errorf("code updates after a changed package = %d, want 1", got)
	}
}

// A missing deployment package must say what to do about it, not report a bare
// file-not-found — `make lambdas` is the answer and most deployers will not guess it.
func TestMissingZipSaysHowToBuildIt(t *testing.T) {
	fn := &lambdaFunc{
		api:      newFakeLambda(),
		funcName: FuncGateway,
		zipPath:  "build/forayd.zip",
		readZip:  func(string) ([]byte, error) { return nil, errors.New("no such file") },
	}
	_, err := fn.ensure(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "make lambdas") {
		t.Errorf("error %q should point at `make lambdas`", err)
	}
}

// --- LWA layer resolution ---------------------------------------------------

// The whole point of resolving: a hand-pinned, region-specific ARN was the friction
// Terraform imposed, and a wrong one is a silent 503.
//
// Versions cannot be enumerated (a public layer grants only GetLayerVersion), so this
// searches — and the search must land on the NEWEST version, not merely an existing
// one.
func TestResolveLWALayerARN(t *testing.T) {
	for _, latest := range []int32{1, 2, 25, 30, 31, 64, 100} {
		f := newFakeLambda()
		f.latestLayerVersion = latest

		arn, err := resolveLWALayerARN(context.Background(), f, "us-west-2")
		if err != nil {
			t.Fatalf("latest=%d: resolve: %v", latest, err)
		}
		want := fmt.Sprintf("arn:aws:lambda:us-west-2:%s:layer:%s:%d",
			lwaPublisherAccount, lwaLayerNameArm64, latest)
		if arn != want {
			t.Errorf("latest=%d: arn = %q, want %q", latest, arn, want)
		}
	}
}

// The search must stay cheap: doubling then binary-searching is ~2·log2(n) calls, so
// a linear scan creeping back in would show up here.
func TestResolveLWALayerSearchIsLogarithmic(t *testing.T) {
	f := newFakeLambda()
	f.latestLayerVersion = 30
	if _, err := resolveLWALayerARN(context.Background(), f, "us-west-2"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// 1 verify + 5 doubling + ~4 bisect. 20 is generous headroom; 30+ would mean a
	// linear scan.
	if f.layerProbes > 20 {
		t.Errorf("used %d probes for version 30, want a logarithmic search (<=20)", f.layerProbes)
	}
}

// An unpublished version answers AccessDenied, NOT NotFound, because the layer's
// resource policy is per-version. Reading that as a hard failure is what broke the
// first implementation against a real account.
func TestResolveLWALayerTreatsAccessDeniedAsAbsent(t *testing.T) {
	f := newFakeLambda()
	f.latestLayerVersion = 30
	arn, err := resolveLWALayerARN(context.Background(), f, "us-west-2")
	if err != nil {
		t.Fatalf("AccessDenied on an unpublished version must mean absent, not fatal: %v", err)
	}
	if !strings.HasSuffix(arn, ":30") {
		t.Errorf("arn = %q, want the newest version", arn)
	}
}

// A throttle or network failure must NOT be read as "absent" — that would silently
// resolve an older version, or none.
func TestResolveLWALayerPropagatesRealErrors(t *testing.T) {
	f := newFakeLambda()
	f.layerErr = errors.New("ThrottlingException: rate exceeded")
	if _, err := resolveLWALayerARN(context.Background(), f, "us-west-2"); err == nil {
		t.Fatal("want a throttle surfaced, not treated as an absent version")
	}
}

// The region has to appear in the ARN: a layer from another region cannot be
// attached, so resolving against the wrong one would fail at deploy time.
func TestResolveLWALayerIsRegionScoped(t *testing.T) {
	f := newFakeLambda()
	for _, region := range []string{"us-east-1", "eu-central-1", "ap-southeast-2"} {
		arn, err := resolveLWALayerARN(context.Background(), f, region)
		if err != nil {
			t.Fatalf("resolve %s: %v", region, err)
		}
		if !strings.Contains(arn, ":"+region+":") {
			t.Errorf("arn %q does not name region %s", arn, region)
		}
	}
}

// Both failure modes must tell the deployer about --lwa-layer-arn, since that is the
// escape hatch for an account where the public layer is unavailable.
func TestResolveLWALayerErrorsMentionTheOverride(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeLambda)
	}{
		{"api error", func(f *fakeLambda) { f.layerErr = errors.New("boom") }},
		{"layer not published in this region", func(f *fakeLambda) { f.latestLayerVersion = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLambda()
			tt.setup(f)
			_, err := resolveLWALayerARN(context.Background(), f, "us-west-2")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), "--lwa-layer-arn") {
				t.Errorf("error %q should point at --lwa-layer-arn", err)
			}
		})
	}
}

// --- log groups -------------------------------------------------------------

// Created explicitly so retention is set from the start: a group Lambda creates
// implicitly on first invocation retains FOREVER, and log storage is one of the few
// things here that bills by the GB-month with no TTL of its own.
func TestLogGroupsCreatedWithRetention(t *testing.T) {
	cfg := testConfig()
	cfg.LogRetentionDays = 7
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, name := range []string{lambdaLogGroup(FuncGateway), lambdaLogGroup(FuncWebAPI)} {
		g, ok := f.logs.groups[name]
		if !ok {
			t.Fatalf("log group %s not created", name)
		}
		if g.retentionDays != 7 {
			t.Errorf("%s retention = %d, want 7 — an unbounded group bills forever", name, g.retentionDays)
		}
	}
}

// Retention is re-applied to a group that already exists, because the group Lambda
// created implicitly is precisely the one retaining forever.
func TestLogGroupRetentionConvergesOnExistingGroup(t *testing.T) {
	cfg := mustValidate(t, testConfig())
	f := newFakeLogs()
	name := lambdaLogGroup(FuncGateway)
	// Pre-existing, unbounded (what Lambda's implicit creation leaves behind).
	f.groups[name] = &fakeLogGroup{tags: map[string]string{}}

	g := &logGroup{api: f, group: name, retentionDays: cfg.LogRetentionDays, region: cfg.Region, accountID: cfg.AccountID}
	act, err := g.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpExists {
		t.Errorf("op = %s, want exists", act.Op)
	}
	if f.groups[name].retentionDays != cfg.LogRetentionDays {
		t.Errorf("retention = %d, want %d applied to the pre-existing group",
			f.groups[name].retentionDays, cfg.LogRetentionDays)
	}
}

// exists must compare names exactly: DescribeLogGroups takes a PREFIX, so a prefix
// match would report "/aws/lambda/foray-gateway" as present when only
// "/aws/lambda/foray-gateway-v2" is.
func TestLogGroupExistsIsExactNotPrefix(t *testing.T) {
	f := newFakeLogs()
	f.groups["/aws/lambda/foray-gateway-v2"] = &fakeLogGroup{tags: map[string]string{}}

	g := &logGroup{api: f, group: "/aws/lambda/foray-gateway", region: "us-west-2", accountID: testAccount}
	exists, err := g.exists(context.Background())
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Error("prefix match reported as existing — the group name must match exactly")
	}
}

// Tagging a log group is best-effort: some accounts restrict it, and nothing that
// bills is left untracked by it. A tag failure must not fail the deploy.
func TestLogGroupTagFailureIsNotFatal(t *testing.T) {
	f := newFakeLogs()
	f.tagErr = errors.New("not authorized to tag")
	g := &logGroup{api: f, group: lambdaLogGroup(FuncGateway), retentionDays: 14, region: "us-west-2", accountID: testAccount}

	act, err := g.ensure(context.Background())
	if err != nil {
		t.Fatalf("a tag failure should not fail the deploy: %v", err)
	}
	if !strings.Contains(act.Detail, "untagged") {
		t.Errorf("detail = %q, want it to note the group went untagged", act.Detail)
	}
}

// Log groups are deleted on teardown, so log storage does not keep billing behind a
// deleted stack.
func TestLogGroupsRemovedOnTeardown(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.logs.groups) != 0 {
		t.Errorf("%d log group(s) left billing after teardown", len(f.logs.groups))
	}
	if len(f.lam.functions) != 0 {
		t.Errorf("%d function(s) left after teardown", len(f.lam.functions))
	}
}

// Functions come off before the roles they assume — a role in use is still
// deletable, but removing the consumer first keeps the account coherent at every
// intermediate step.
func TestFunctionsRemovedBeforeTheirRoles(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	actions, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	lastFunc, firstRole := -1, -1
	for i, a := range actions {
		if a.Kind == "lambda function" {
			lastFunc = i
		}
		if a.Kind == "iam role" && firstRole < 0 {
			firstRole = i
		}
	}
	if lastFunc < 0 || firstRole < 0 {
		t.Fatalf("missing actions: func=%d role=%d", lastFunc, firstRole)
	}
	if lastFunc > firstRole {
		t.Errorf("a function was removed at %d, after a role at %d", lastFunc, firstRole)
	}
}

func stringPtr(s string) *string { return &s }

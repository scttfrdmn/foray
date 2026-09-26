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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apitypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
)

// Each route must reach the function that actually serves it. A route pointing at
// the wrong integration is the worst kind of wrong here: the deploy succeeds, and
// every request to that path 404s from a function that has never heard of it.
func TestRoutesTargetTheRightFunctions(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	api := f.apigw.apiByName(APIName)
	if api == nil {
		t.Fatalf("no API named %q", APIName)
	}
	if len(api.routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(api.routes))
	}

	// integration id → the function ARN it invokes
	uriOf := map[string]string{}
	for id, it := range api.integrations {
		uriOf[id] = it.uri
	}

	tests := []struct {
		routeKey string
		wantFunc string
		why      string
	}{
		{RouteTrace, FuncGateway, "traces go to forayd"},
		{RouteAPI, FuncWebAPI, "the page's brain loop goes to the web API"},
		{RouteHealthz, FuncWebAPI, "the health check goes to the web API"},
	}
	for _, tt := range tests {
		t.Run(tt.routeKey, func(t *testing.T) {
			r, ok := api.routes[tt.routeKey]
			if !ok {
				t.Fatalf("route %q missing", tt.routeKey)
			}
			integrationID := strings.TrimPrefix(r.target, "integrations/")
			gotURI, ok := uriOf[integrationID]
			if !ok {
				t.Fatalf("route %q targets %q, which is not an integration on this API", tt.routeKey, r.target)
			}
			wantURI := functionARN(cfg.Region, cfg.AccountID, tt.wantFunc)
			if gotURI != wantURI {
				t.Errorf("route %q reaches %q, want %q (%s)", tt.routeKey, gotURI, wantURI, tt.why)
			}
		})
	}
}

// One integration per function, not one per route: /api/* and /healthz both go to the
// web API and should share its integration.
func TestOneIntegrationPerFunction(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	api := f.apigw.apiByName(APIName)
	if len(api.integrations) != 2 {
		t.Errorf("got %d integrations, want 2 (one per function, shared across routes)", len(api.integrations))
	}
}

// AWS_PROXY with payload format 2.0 is what the Lambda Web Adapter expects; 1.0
// delivers a different event shape the adapter would not recognize, which fails at
// invoke time rather than deploy time.
func TestIntegrationsArePayloadV2AwsProxy(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, it := range f.apigw.apiByName(APIName).integrations {
		if it.integType != apitypes.IntegrationTypeAwsProxy {
			t.Errorf("integration %s type = %q, want AWS_PROXY", it.id, it.integType)
		}
		if it.payloadFormat != "2.0" {
			t.Errorf("integration %s payload format = %q, want 2.0 (LWA expects it)", it.id, it.payloadFormat)
		}
		// POST is how API Gateway invokes Lambda regardless of the caller's method.
		if it.method != "POST" {
			t.Errorf("integration %s method = %q, want POST", it.id, it.method)
		}
	}
}

// The invoke permission must be scoped to THIS API's execution ARN. Granting
// apigateway.amazonaws.com without a SourceArn would let any API in any account
// invoke the function.
func TestInvokePermissionScopedToThisAPI(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	apiID := f.apigw.apiByName(APIName).id
	want := executionARN(cfg.Region, cfg.AccountID, apiID) + "/*/*"

	for _, name := range []string{FuncGateway, FuncWebAPI} {
		fn := f.lam.functions[name]
		got, ok := fn.permissions[invokePermissionID]
		if !ok {
			t.Fatalf("%s has no %s statement — every request would 500", name, invokePermissionID)
		}
		if got != want {
			t.Errorf("%s SourceArn = %q, want %q", name, got, want)
		}
		if got == "" || got == "*" {
			t.Errorf("%s SourceArn is unscoped (%q) — any API in any account could invoke it", name, got)
		}
		// execute-api, not apigateway: the latter names the control plane and would
		// never match a request.
		if !strings.Contains(got, ":execute-api:") {
			t.Errorf("%s SourceArn = %q, want an execute-api ARN", name, got)
		}
	}
}

// The $default stage is what serves the API at the bare domain with no stage segment
// — which is what lets CloudFront forward /api/* straight through. AutoDeploy is what
// makes a route change take effect; without it a new route 404s and nothing says why.
func TestDefaultStageAutoDeploys(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	api := f.apigw.apiByName(APIName)
	s, ok := api.stages[StageDefault]
	if !ok {
		t.Fatalf("no %s stage — the API would need a stage segment in every path", StageDefault)
	}
	if !s.autoDeploy {
		t.Error("AutoDeploy is off — a newly added route would 404 with no explanation")
	}
}

// Access logging carries integrationErrorMessage, which is what reports "the Lambda
// never became ready" — the failure that is otherwise completely invisible (#64).
func TestStageAccessLoggingCapturesIntegrationErrors(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := f.apigw.apiByName(APIName).stages[StageDefault]
	if s.logARN == "" {
		t.Fatal("stage has no access-log destination")
	}
	if !strings.Contains(s.logARN, apigwLogGroup) {
		t.Errorf("log destination = %q, want the %s group", s.logARN, apigwLogGroup)
	}

	var format map[string]string
	if err := json.Unmarshal([]byte(s.logFormat), &format); err != nil {
		t.Fatalf("access log format is not JSON: %v", err)
	}
	if format["integrationE"] != "$context.integrationErrorMessage" {
		t.Errorf("access log omits integrationErrorMessage — the LWA-readiness failure would be invisible again")
	}
	for _, key := range []string{"requestId", "routeKey", "status"} {
		if format[key] == "" {
			t.Errorf("access log format missing %q", key)
		}
	}
}

// The API's own log group is created with retention, like the Lambdas'.
func TestAPIGatewayLogGroupCreated(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	g, ok := f.logs.groups[apigwLogGroup]
	if !ok {
		t.Fatalf("log group %s not created", apigwLogGroup)
	}
	if g.retentionDays <= 0 {
		t.Error("API access logs have unbounded retention — they would bill forever")
	}
}

// Re-applying converges: no duplicate routes, no duplicate integrations, and the
// fixed statement id means the permission grant is one statement rather than a
// growing list.
func TestAPIApplyIsIdempotent(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	firstID := f.apigw.apiByName(APIName).id

	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(f.apigw.apis) != 1 {
		t.Errorf("%d APIs after re-apply, want 1", len(f.apigw.apis))
	}
	api := f.apigw.apiByName(APIName)
	if api.id != firstID {
		t.Errorf("API id changed on re-apply (%s → %s)", firstID, api.id)
	}
	if len(api.routes) != 3 {
		t.Errorf("%d routes after re-apply, want 3", len(api.routes))
	}
	if len(api.integrations) != 2 {
		t.Errorf("%d integrations after re-apply, want 2", len(api.integrations))
	}
	for _, name := range []string{FuncGateway, FuncWebAPI} {
		if n := len(f.lam.functions[name].permissions); n != 1 {
			t.Errorf("%s has %d policy statements after re-apply, want 1", name, n)
		}
	}
}

// A route left pointing at a stale integration silently serves the wrong function,
// so Apply retargets rather than leaving it.
func TestRouteRetargetedWhenIntegrationChanges(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	api := f.apigw.apiByName(APIName)
	api.routes[RouteAPI].target = "integrations/bogus"

	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if got := api.routes[RouteAPI].target; got == "integrations/bogus" {
		t.Error("route left pointing at a stale integration — /api/* would reach nothing")
	}
}

// Teardown removes the invoke statements (they live on the FUNCTIONS, not the API, so
// deleting the API alone would strand them) and then the API, whose deletion cascades
// to integrations, routes and stages.
func TestAPITeardownRemovesPermissionsAndAPI(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Capture the functions before teardown deletes them, to inspect their policies.
	gw := f.lam.functions[FuncGateway]
	web := f.lam.functions[FuncWebAPI]

	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.apigw.apis) != 0 {
		t.Errorf("%d API(s) left after teardown", len(f.apigw.apis))
	}
	if len(gw.permissions) != 0 {
		t.Errorf("gateway still carries %d policy statement(s)", len(gw.permissions))
	}
	if len(web.permissions) != 0 {
		t.Errorf("web API still carries %d policy statement(s)", len(web.permissions))
	}
}

// Teardown must be re-runnable: the second pass finds nothing and must not error on
// the already-removed permission statements.
func TestAPITeardownIsIdempotent(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("first Teardown: %v", err)
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("second Teardown should be a no-op, got: %v", err)
	}
}

// The API comes off before the functions it invokes, so no intermediate state has a
// live API routing to a deleted function.
func TestAPIRemovedBeforeItsFunctions(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	actions, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	apiAt, firstFuncAt := -1, -1
	for i, a := range actions {
		if a.Kind == "http api" {
			apiAt = i
		}
		if a.Kind == "lambda function" && firstFuncAt < 0 {
			firstFuncAt = i
		}
	}
	if apiAt < 0 || firstFuncAt < 0 {
		t.Fatalf("missing actions: api=%d func=%d", apiAt, firstFuncAt)
	}
	if apiAt > firstFuncAt {
		t.Errorf("API removed at %d, after a function at %d — it would briefly route to nothing", apiAt, firstFuncAt)
	}
}

// API Gateway does not enforce unique names, so two APIs called "foray" must be
// reported rather than guessed between — silently picking one would have successive
// deploys converge different APIs.
func TestDuplicateAPINamesAreReported(t *testing.T) {
	f := newFakeAPIGW()
	for i := 0; i < 2; i++ {
		if _, err := f.CreateApi(context.Background(), &apigatewayv2.CreateApiInput{
			Name:         aws.String(APIName),
			ProtocolType: apitypes.ProtocolTypeHttp,
		}); err != nil {
			t.Fatalf("seed api: %v", err)
		}
	}
	h := forayHTTPAPI(f, newFakeLambda(), mustValidate(t, testConfig()), "")
	_, err := h.ensure(context.Background())
	if err == nil {
		t.Fatal("want an error naming the ambiguity")
	}
	if !strings.Contains(err.Error(), "unique names") {
		t.Errorf("error %q should explain that API Gateway does not enforce unique names", err)
	}
}

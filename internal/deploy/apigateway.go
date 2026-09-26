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
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apitypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// apigwAPI is the slice of API Gateway v2 this package uses.
type apigwAPI interface {
	GetApis(ctx context.Context, in *apigatewayv2.GetApisInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error)
	CreateApi(ctx context.Context, in *apigatewayv2.CreateApiInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateApiOutput, error)
	DeleteApi(ctx context.Context, in *apigatewayv2.DeleteApiInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.DeleteApiOutput, error)

	GetIntegrations(ctx context.Context, in *apigatewayv2.GetIntegrationsInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationsOutput, error)
	CreateIntegration(ctx context.Context, in *apigatewayv2.CreateIntegrationInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateIntegrationOutput, error)

	GetRoutes(ctx context.Context, in *apigatewayv2.GetRoutesInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error)
	CreateRoute(ctx context.Context, in *apigatewayv2.CreateRouteInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateRouteOutput, error)
	UpdateRoute(ctx context.Context, in *apigatewayv2.UpdateRouteInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateRouteOutput, error)

	GetStage(ctx context.Context, in *apigatewayv2.GetStageInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.GetStageOutput, error)
	CreateStage(ctx context.Context, in *apigatewayv2.CreateStageInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateStageOutput, error)
	UpdateStage(ctx context.Context, in *apigatewayv2.UpdateStageInput, opts ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateStageOutput, error)
}

// lambdaPermAPI is the resource-policy slice of Lambda: granting API Gateway the
// right to invoke a function.
type lambdaPermAPI interface {
	AddPermission(ctx context.Context, in *lambda.AddPermissionInput, opts ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error)
	RemovePermission(ctx context.Context, in *lambda.RemovePermissionInput, opts ...func(*lambda.Options)) (*lambda.RemovePermissionOutput, error)
}

// The API's shape. Routes mirror deploy/terraform/api.tf.
const (
	APIName = "foray"
	// StageDefault is the implicit stage: with `$default`, the API is served at the
	// bare domain with no stage segment in the path, which is what lets CloudFront
	// forward /api/* straight through.
	StageDefault = "$default"

	RouteTrace   = "ANY /sessions/{proxy+}" // forayd: POST /sessions/{id}/trace
	RouteAPI     = "ANY /api/{proxy+}"      // the page's brain loop
	RouteHealthz = "GET /healthz"

	// invokePermissionID is the statement id on the function's resource policy.
	// Fixed so a re-apply updates one statement rather than accumulating them.
	invokePermissionID = "AllowAPIGatewayInvoke"

	apigwLogGroup = "/aws/apigateway/foray"
)

// apiRoute binds a route key to the function that serves it.
type apiRoute struct {
	key      string
	function string
}

// httpAPI provisions the HTTP API and everything that only exists inside it:
// integrations, routes, the $default stage, and the Lambda resource-policy grants
// that let the API invoke the functions.
//
// These are one resource rather than several because none of them can be addressed
// without the API's id, and the id is *generated* — unlike every other resource
// here, it cannot be constructed from the config (see arn.go, where that property
// is what lets the IAM policies be written before their targets exist). Rather than
// thread discovered state between resources, the unit that owns the id owns
// everything that needs it.
type httpAPI struct {
	api apigwAPI
	lam lambdaPermAPI

	apiName   string
	region    string
	accountID string
	routes    []apiRoute
	// logGroupARN receives the stage's access logs. Empty disables access logging.
	logGroupARN string
}

func (h *httpAPI) kind() string { return "http api" }
func (h *httpAPI) name() string { return h.apiName }

func (h *httpAPI) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: h.kind(), Name: h.apiName}

	id, err := h.find(ctx)
	if err != nil {
		return act, err
	}
	if id == "" {
		out, err := h.api.CreateApi(ctx, &apigatewayv2.CreateApiInput{
			Name:         aws.String(h.apiName),
			ProtocolType: apitypes.ProtocolTypeHttp,
			Tags:         Tags(h.apiName),
		})
		if err != nil {
			return act, fmt.Errorf("create api: %w", err)
		}
		id = aws.ToString(out.ApiId)
		act.Op = OpCreate
	} else {
		act.Op = OpExists
	}

	// One integration per function, then the routes that point at them. Converged on
	// every run: a route missing its integration is a 500 at the edge, and nothing
	// about that failure names the cause.
	integrations, err := h.ensureIntegrations(ctx, id)
	if err != nil {
		return act, err
	}
	if err := h.ensureRoutes(ctx, id, integrations); err != nil {
		return act, err
	}
	if err := h.ensureStage(ctx, id); err != nil {
		return act, err
	}
	if err := h.ensureInvokePermissions(ctx, id); err != nil {
		return act, err
	}

	act.Detail = fmt.Sprintf("%d routes, %s stage, id %s", len(h.routes), StageDefault, id)
	return act, nil
}

// ensureIntegrations returns integration ids keyed by function name.
//
// Integrations have generated ids too, so they are matched by their target — the
// function ARN in IntegrationUri — which is the only stable identity they have.
func (h *httpAPI) ensureIntegrations(ctx context.Context, apiID string) (map[string]string, error) {
	existing, err := h.api.GetIntegrations(ctx, &apigatewayv2.GetIntegrationsInput{ApiId: aws.String(apiID)})
	if err != nil {
		return nil, fmt.Errorf("get integrations: %w", err)
	}
	byURI := map[string]string{}
	for _, it := range existing.Items {
		byURI[aws.ToString(it.IntegrationUri)] = aws.ToString(it.IntegrationId)
	}

	out := map[string]string{}
	for _, fn := range h.functions() {
		uri := functionARN(h.region, h.accountID, fn)
		if id, ok := byURI[uri]; ok {
			out[fn] = id
			continue
		}
		created, err := h.api.CreateIntegration(ctx, &apigatewayv2.CreateIntegrationInput{
			ApiId:           aws.String(apiID),
			IntegrationType: apitypes.IntegrationTypeAwsProxy,
			IntegrationUri:  aws.String(uri),
			// POST is the method API Gateway uses to *invoke Lambda*, regardless of the
			// caller's method — it is not the route's method.
			IntegrationMethod: aws.String("POST"),
			// 2.0 is what the Lambda Web Adapter expects; 1.0 delivers a different
			// event shape and the adapter would not recognize the request.
			PayloadFormatVersion: aws.String("2.0"),
		})
		if err != nil {
			return nil, fmt.Errorf("create integration for %s: %w", fn, err)
		}
		out[fn] = aws.ToString(created.IntegrationId)
	}
	return out, nil
}

// ensureRoutes creates or retargets each route. Routes are matched by RouteKey,
// which is their stable identity.
func (h *httpAPI) ensureRoutes(ctx context.Context, apiID string, integrations map[string]string) error {
	existing, err := h.api.GetRoutes(ctx, &apigatewayv2.GetRoutesInput{ApiId: aws.String(apiID)})
	if err != nil {
		return fmt.Errorf("get routes: %w", err)
	}
	byKey := map[string]apitypes.Route{}
	for _, r := range existing.Items {
		byKey[aws.ToString(r.RouteKey)] = r
	}

	for _, want := range h.routes {
		integrationID, ok := integrations[want.function]
		if !ok {
			return fmt.Errorf("no integration for %s (route %q)", want.function, want.key)
		}
		target := "integrations/" + integrationID

		cur, exists := byKey[want.key]
		if !exists {
			if _, err := h.api.CreateRoute(ctx, &apigatewayv2.CreateRouteInput{
				ApiId:    aws.String(apiID),
				RouteKey: aws.String(want.key),
				Target:   aws.String(target),
			}); err != nil {
				return fmt.Errorf("create route %q: %w", want.key, err)
			}
			continue
		}
		// A route pointing at a stale integration silently serves the wrong function
		// — /api/* reaching forayd would 404 every call — so retarget rather than
		// leave it.
		if aws.ToString(cur.Target) != target {
			if _, err := h.api.UpdateRoute(ctx, &apigatewayv2.UpdateRouteInput{
				ApiId:    aws.String(apiID),
				RouteId:  cur.RouteId,
				RouteKey: aws.String(want.key),
				Target:   aws.String(target),
			}); err != nil {
				return fmt.Errorf("retarget route %q: %w", want.key, err)
			}
		}
	}
	return nil
}

// accessLogFormat is the JSON access-log line. Chosen for debuggability of exactly
// the failures this control plane produces: integrationErrorMessage is what carries
// "the Lambda never became ready", which is otherwise invisible (see #64).
func accessLogFormat() string {
	b, err := json.Marshal(map[string]string{
		"requestId":    "$context.requestId",
		"ip":           "$context.identity.sourceIp",
		"routeKey":     "$context.routeKey",
		"status":       "$context.status",
		"responseLen":  "$context.responseLength",
		"integrationE": "$context.integrationErrorMessage",
	})
	if err != nil {
		panic(fmt.Sprintf("deploy: marshal access log format: %v", err))
	}
	return string(b)
}

// ensureStage creates or converges the $default stage.
//
// AutoDeploy is what makes a route change take effect without an explicit
// deployment — with it off, a freshly added route returns 404 and nothing says why.
func (h *httpAPI) ensureStage(ctx context.Context, apiID string) error {
	var settings *apitypes.AccessLogSettings
	if h.logGroupARN != "" {
		settings = &apitypes.AccessLogSettings{
			DestinationArn: aws.String(h.logGroupARN),
			Format:         aws.String(accessLogFormat()),
		}
	}

	_, err := h.api.GetStage(ctx, &apigatewayv2.GetStageInput{
		ApiId:     aws.String(apiID),
		StageName: aws.String(StageDefault),
	})
	if err != nil {
		var notFound *apitypes.NotFoundException
		if !errors.As(err, &notFound) {
			return fmt.Errorf("get stage: %w", err)
		}
		if _, err := h.api.CreateStage(ctx, &apigatewayv2.CreateStageInput{
			ApiId:             aws.String(apiID),
			StageName:         aws.String(StageDefault),
			AutoDeploy:        aws.Bool(true),
			AccessLogSettings: settings,
			Tags:              Tags(h.apiName),
		}); err != nil {
			return fmt.Errorf("create %s stage: %w", StageDefault, err)
		}
		return nil
	}

	if _, err := h.api.UpdateStage(ctx, &apigatewayv2.UpdateStageInput{
		ApiId:             aws.String(apiID),
		StageName:         aws.String(StageDefault),
		AutoDeploy:        aws.Bool(true),
		AccessLogSettings: settings,
	}); err != nil {
		return fmt.Errorf("update %s stage: %w", StageDefault, err)
	}
	return nil
}

// ensureInvokePermissions lets this API — and only this API — invoke the functions.
//
// Without the grant every request is a 500 that says nothing useful. The SourceArn
// is the API's execution ARN with `/*/*` (any stage, any route), which scopes the
// permission to this API rather than to the whole service: `apigateway.amazonaws.com`
// alone would let *any* API in *any* account invoke the function.
func (h *httpAPI) ensureInvokePermissions(ctx context.Context, apiID string) error {
	source := executionARN(h.region, h.accountID, apiID) + "/*/*"
	for _, fn := range h.functions() {
		_, err := h.lam.AddPermission(ctx, &lambda.AddPermissionInput{
			FunctionName: aws.String(fn),
			StatementId:  aws.String(invokePermissionID),
			Action:       aws.String("lambda:InvokeFunction"),
			Principal:    aws.String("apigateway.amazonaws.com"),
			SourceArn:    aws.String(source),
		})
		if err == nil {
			continue
		}
		// The statement already exists — the normal case on re-apply, since the
		// statement id is fixed.
		var conflict *lambdatypes.ResourceConflictException
		if errors.As(err, &conflict) {
			continue
		}
		return fmt.Errorf("allow %s to invoke %s: %w", h.apiName, fn, err)
	}
	return nil
}

func (h *httpAPI) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: h.kind(), Name: h.apiName}
	id, err := h.find(ctx)
	if err != nil {
		return act, err
	}
	if id == "" {
		act.Op = OpAbsent
		return act, nil
	}

	// Drop the resource-policy statements first. They live on the *functions*, not
	// the API, so deleting the API would strand them — and a leftover statement
	// naming a deleted API blocks nothing but does make the next deploy's
	// AddPermission a conflict on a stale SourceArn.
	for _, fn := range h.functions() {
		if _, err := h.lam.RemovePermission(ctx, &lambda.RemovePermissionInput{
			FunctionName: aws.String(fn),
			StatementId:  aws.String(invokePermissionID),
		}); err != nil {
			var notFound *lambdatypes.ResourceNotFoundException
			if !errors.As(err, &notFound) {
				return act, fmt.Errorf("remove invoke permission from %s: %w", fn, err)
			}
		}
	}

	// Deleting the API cascades to its integrations, routes and stages.
	if _, err := h.api.DeleteApi(ctx, &apigatewayv2.DeleteApiInput{ApiId: aws.String(id)}); err != nil {
		return act, fmt.Errorf("delete api: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

// find returns the API's generated id, or "" when it does not exist.
//
// Matched by name because that is the only thing the config knows. API Gateway does
// not enforce unique names, so a duplicate is reported rather than guessed at —
// silently picking one would have the deploy converge a different API than the last
// run did.
func (h *httpAPI) find(ctx context.Context) (string, error) {
	var (
		next  *string
		found []string
	)
	for {
		out, err := h.api.GetApis(ctx, &apigatewayv2.GetApisInput{NextToken: next})
		if err != nil {
			return "", fmt.Errorf("get apis: %w", err)
		}
		for _, a := range out.Items {
			if aws.ToString(a.Name) == h.apiName {
				found = append(found, aws.ToString(a.ApiId))
			}
		}
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%d HTTP APIs are named %q (%v) — API Gateway does not enforce unique names; "+
			"delete the extras so a deploy converges a single API", len(found), h.apiName, found)
	}
}

// functions lists the distinct functions the routes target, in a stable order.
func (h *httpAPI) functions() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range h.routes {
		if !seen[r.function] {
			seen[r.function] = true
			out = append(out, r.function)
		}
	}
	return out
}

// forayHTTPAPI is the API in front of the two Lambdas: the gateway serves traces,
// the web API serves the page's loop and the health check.
func forayHTTPAPI(api apigwAPI, lam lambdaPermAPI, cfg Config, logGroupARN string) *httpAPI {
	return &httpAPI{
		api:         api,
		lam:         lam,
		apiName:     APIName,
		region:      cfg.Region,
		accountID:   cfg.AccountID,
		logGroupARN: logGroupARN,
		routes: []apiRoute{
			{key: RouteTrace, function: FuncGateway},
			{key: RouteAPI, function: FuncWebAPI},
			{key: RouteHealthz, function: FuncWebAPI},
		},
	}
}

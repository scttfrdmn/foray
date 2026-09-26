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
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// lambdaAPI is the slice of Lambda this package uses.
type lambdaAPI interface {
	layerLister
	GetFunction(ctx context.Context, in *lambda.GetFunctionInput, opts ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
	CreateFunction(ctx context.Context, in *lambda.CreateFunctionInput, opts ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error)
	DeleteFunction(ctx context.Context, in *lambda.DeleteFunctionInput, opts ...func(*lambda.Options)) (*lambda.DeleteFunctionOutput, error)
	UpdateFunctionCode(ctx context.Context, in *lambda.UpdateFunctionCodeInput, opts ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error)
	UpdateFunctionConfiguration(ctx context.Context, in *lambda.UpdateFunctionConfigurationInput, opts ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error)
	TagResource(ctx context.Context, in *lambda.TagResourceInput, opts ...func(*lambda.Options)) (*lambda.TagResourceOutput, error)
}

// lambdaFullAPI is everything this package asks of Lambda: managing the functions
// (lambdaAPI) and writing the resource-policy grants the HTTP API needs
// (lambdaPermAPI). One client satisfies both; the interfaces stay split so each
// resource declares only what it uses.
type lambdaFullAPI interface {
	lambdaAPI
	lambdaPermAPI
}

// Function names and the ports each binary listens on.
//
// The ports are the load-bearing detail. `cmd/forayd` binds :8080 and
// `cmd/foray-web` binds :8090, and LWA proxies each request to AWS_LWA_PORT — so a
// single shared value leaves one function's readiness check failing forever, which
// surfaces as a **silent 503** with nothing in the application log. That was a real
// bug (PR #64); the constants live next to each other here so the next reader sees
// immediately that they must differ, and a test asserts it.
const (
	FuncGateway     = "foray-gateway"
	FuncWebAPI      = "foray-webapi"
	portGateway     = "8080" // cmd/forayd
	portWebAPI      = "8090" // cmd/foray-web
	lwaExecWrapper  = "/opt/bootstrap"
	lwaReadinessURL = "/healthz"
)

// lambdaFunc provisions one Lambda: an unmodified http.Server binary behind LWA.
type lambdaFunc struct {
	api      lambdaAPI
	funcName string
	roleARN  string
	zipPath  string
	layerARN string
	port     string
	timeout  int32
	memoryMB int32
	env      map[string]string
	// now and sleep are injectable so the role-propagation retry is testable.
	sleep func(time.Duration)
	// readZip loads the deployment package; nil → os.ReadFile. Injected by the
	// FORAY_FAKE rehearsal (which has no built zips) and by tests, so neither needs
	// `make lambdas` to have run.
	readZip func(path string) ([]byte, error)
}

func (l *lambdaFunc) kind() string { return "lambda function" }
func (l *lambdaFunc) name() string { return l.funcName }

// lwaEnv merges the adapter's own settings with the function's. AWS_LWA_PORT is set
// per function — see the constants above.
func (l *lambdaFunc) lwaEnv() map[string]string {
	out := map[string]string{
		"AWS_LAMBDA_EXEC_WRAPPER":      lwaExecWrapper,
		"AWS_LWA_READINESS_CHECK_PATH": lwaReadinessURL,
		"AWS_LWA_PORT":                 l.port,
	}
	for k, v := range l.env {
		out[k] = v
	}
	return out
}

func (l *lambdaFunc) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: l.kind(), Name: l.funcName}

	read := l.readZip
	if read == nil {
		read = os.ReadFile
	}
	zip, err := read(l.zipPath)
	if err != nil {
		return act, fmt.Errorf("read %s (build it first: make lambdas): %w", l.zipPath, err)
	}

	cur, err := l.get(ctx)
	if err != nil {
		return act, err
	}
	if cur == nil {
		if err := l.create(ctx, zip); err != nil {
			return act, err
		}
		act.Op = OpCreate
		act.Detail = fmt.Sprintf("arm64, :%s, %s", l.port, humanBytes(len(zip)))
		return act, nil
	}

	act.Op = OpExists
	// Update the code only when it actually differs. Lambda reports the deployed
	// zip's base64 sha256, so comparing is exact and avoids republishing an
	// identical package on every deploy.
	want := zipSHA256(zip)
	if aws.ToString(cur.CodeSha256) != want {
		if _, err := l.api.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
			FunctionName: aws.String(l.funcName),
			ZipFile:      zip,
		}); err != nil {
			return act, fmt.Errorf("update function code: %w", err)
		}
		act.Detail = "code updated"
	}
	// Configuration converges either way: env, timeout, memory and the layer can
	// all drift, and the LWA env in particular is what makes the function respond
	// at all.
	if _, err := l.api.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String(l.funcName),
		Role:         aws.String(l.roleARN),
		Timeout:      aws.Int32(l.timeout),
		MemorySize:   aws.Int32(l.memoryMB),
		Layers:       []string{l.layerARN},
		Environment:  &lambdatypes.Environment{Variables: l.lwaEnv()},
	}); err != nil {
		return act, fmt.Errorf("update function configuration: %w", err)
	}
	if err := l.tag(ctx, cur.FunctionArn); err != nil {
		return act, err
	}
	return act, nil
}

// create publishes the function, retrying the one failure that is expected rather
// than exceptional.
//
// IAM is eventually consistent: a role created moments ago is often not yet visible
// to Lambda, and CreateFunction rejects it with InvalidParameterValueException
// ("The role defined for the function cannot be assumed"). Since this package
// creates the roles itself, a first deploy hits that window almost every time — so
// it is retried rather than surfaced as a confusing failure.
func (l *lambdaFunc) create(ctx context.Context, zip []byte) error {
	sleep := l.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	const (
		attempts = 6
		backoff  = 3 * time.Second
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		_, err := l.api.CreateFunction(ctx, &lambda.CreateFunctionInput{
			FunctionName:  aws.String(l.funcName),
			Role:          aws.String(l.roleARN),
			Handler:       aws.String("bootstrap"),
			Runtime:       lambdatypes.RuntimeProvidedal2023,
			Architectures: []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
			Timeout:       aws.Int32(l.timeout),
			MemorySize:    aws.Int32(l.memoryMB),
			Layers:        []string{l.layerARN},
			Environment:   &lambdatypes.Environment{Variables: l.lwaEnv()},
			Code:          &lambdatypes.FunctionCode{ZipFile: zip},
			Tags:          Tags(l.funcName),
		})
		if err == nil {
			return nil
		}
		var already *lambdatypes.ResourceConflictException
		if errors.As(err, &already) {
			// Lost a race with a concurrent apply; that is success here.
			return nil
		}
		if !isRoleNotYetAssumable(err) {
			return fmt.Errorf("create function: %w", err)
		}
		lastErr = err
		if err := ctx.Err(); err != nil {
			return err
		}
		sleep(backoff)
	}
	return fmt.Errorf("create function %s: role %s still not assumable after %d attempts (IAM propagation): %w",
		l.funcName, l.roleARN, attempts, lastErr)
}

func (l *lambdaFunc) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: l.kind(), Name: l.funcName}
	cur, err := l.get(ctx)
	if err != nil {
		return act, err
	}
	if cur == nil {
		act.Op = OpAbsent
		return act, nil
	}
	if _, err := l.api.DeleteFunction(ctx, &lambda.DeleteFunctionInput{
		FunctionName: aws.String(l.funcName),
	}); err != nil {
		return act, fmt.Errorf("delete function: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

func (l *lambdaFunc) get(ctx context.Context) (*lambdatypes.FunctionConfiguration, error) {
	out, err := l.api.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String(l.funcName)})
	if err != nil {
		var notFound *lambdatypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get function: %w", err)
	}
	return out.Configuration, nil
}

func (l *lambdaFunc) tag(ctx context.Context, arn *string) error {
	if aws.ToString(arn) == "" {
		return nil
	}
	if _, err := l.api.TagResource(ctx, &lambda.TagResourceInput{
		Resource: arn,
		Tags:     Tags(l.funcName),
	}); err != nil {
		return fmt.Errorf("tag function: %w", err)
	}
	return nil
}

// isRoleNotYetAssumable reports whether an error is the IAM-propagation case rather
// than a genuinely bad role. Matching on the message is unpleasant but is what AWS
// offers: the error code is the generic InvalidParameterValueException, which also
// covers real misconfiguration, and retrying that indiscriminately would turn a
// typo into a minute of silence.
func isRoleNotYetAssumable(err error) bool {
	var invalid *lambdatypes.InvalidParameterValueException
	if !errors.As(err, &invalid) {
		return false
	}
	msg := aws.ToString(invalid.Message)
	return strings.Contains(msg, "cannot be assumed") ||
		strings.Contains(msg, "Function's execution role")
}

// zipSHA256 is the base64 sha256 Lambda reports as CodeSha256.
func zipSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func humanBytes(n int) string {
	const mb = 1 << 20
	if n >= mb {
		return fmt.Sprintf("%.1f MiB", float64(n)/mb)
	}
	return fmt.Sprintf("%d B", n)
}

// --- function definitions ---------------------------------------------------

// gatewayFunction is forayd: it routes intervention graphs and stamps
// last_request_time. The 10-minute timeout matches the worker HTTP client's — a
// trace can be slow, and a gateway that times out before the worker replies would
// lose a result the user already paid for.
func gatewayFunction(api lambdaAPI, cfg Config, layerARN string) *lambdaFunc {
	return &lambdaFunc{
		api:      api,
		funcName: FuncGateway,
		roleARN:  roleARN(cfg.AccountID, RoleGatewayLambda),
		zipPath:  cfg.GatewayZip,
		layerARN: layerARN,
		port:     portGateway,
		timeout:  600,
		memoryMB: 256,
		env: map[string]string{
			"FORAY_SESSIONS_TABLE": cfg.SessionsTable,
		},
	}
}

// webAPIFunction is the page's API: it plans (Bedrock), prices (truffle) and
// presigns. More memory than the gateway because it does more, and the same long
// timeout because planning plus a trace round-trip happen in one request.
//
// PATH carries /var/task/bin so the bundled truffle binary is found by
// exec.LookPath — the spore "call the tool, don't reimplement" rule reaching all the
// way into the Lambda environment.
func webAPIFunction(api lambdaAPI, cfg Config, layerARN string) *lambdaFunc {
	return &lambdaFunc{
		api:      api,
		funcName: FuncWebAPI,
		roleARN:  roleARN(cfg.AccountID, RoleWebAPILambda),
		zipPath:  cfg.WebAPIZip,
		layerARN: layerARN,
		port:     portWebAPI,
		timeout:  600,
		memoryMB: 512,
		env: map[string]string{
			"FORAY_SESSIONS_TABLE": cfg.SessionsTable,
			"FORAY_DATA_BUCKET":    cfg.DataBucket,
			"FORAY_PLAN_MODEL":     cfg.PlanModelID,
			"FORAY_BUDGET_CEILING": fmt.Sprintf("%.2f", cfg.BudgetCeilingUSD),
			"PATH":                 "/var/task/bin:/usr/local/bin:/usr/bin:/bin",
		},
	}
}

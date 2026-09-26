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

	"github.com/aws/aws-sdk-go-v2/aws"
	cwl "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// logsAPI is the slice of CloudWatch Logs this package uses.
type logsAPI interface {
	CreateLogGroup(ctx context.Context, in *cwl.CreateLogGroupInput, opts ...func(*cwl.Options)) (*cwl.CreateLogGroupOutput, error)
	DeleteLogGroup(ctx context.Context, in *cwl.DeleteLogGroupInput, opts ...func(*cwl.Options)) (*cwl.DeleteLogGroupOutput, error)
	PutRetentionPolicy(ctx context.Context, in *cwl.PutRetentionPolicyInput, opts ...func(*cwl.Options)) (*cwl.PutRetentionPolicyOutput, error)
	DescribeLogGroups(ctx context.Context, in *cwl.DescribeLogGroupsInput, opts ...func(*cwl.Options)) (*cwl.DescribeLogGroupsOutput, error)
	TagResource(ctx context.Context, in *cwl.TagResourceInput, opts ...func(*cwl.Options)) (*cwl.TagResourceOutput, error)
}

// DefaultLogRetentionDays keeps logs short. The control plane is cheap to observe
// and log storage is one of the few things here that bills by the GB-month with no
// TTL of its own — so an unbounded retention would quietly become the resting cost
// this architecture exists to avoid.
const DefaultLogRetentionDays = 14

// logGroup provisions a Lambda's (or the API's) log group explicitly, rather than
// letting Lambda create it implicitly on first invocation.
//
// Creating it here buys two things: the retention policy is set from the start —
// an implicitly-created group retains **forever** — and teardown has something to
// delete, so `foray teardown` does not leave log storage billing behind a deleted
// stack.
type logGroup struct {
	api           logsAPI
	group         string
	retentionDays int32
	// region and accountID build the ARN that tagging needs.
	region    string
	accountID string
}

func (g *logGroup) kind() string { return "log group" }
func (g *logGroup) name() string { return g.group }

func (g *logGroup) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: g.kind(), Name: g.group}

	exists, err := g.exists(ctx)
	if err != nil {
		return act, err
	}
	if exists {
		act.Op = OpExists
	} else {
		if _, err := g.api.CreateLogGroup(ctx, &cwl.CreateLogGroupInput{
			LogGroupName: aws.String(g.group),
			Tags:         Tags(g.group),
		}); err != nil {
			var already *cwltypes.ResourceAlreadyExistsException
			if !errors.As(err, &already) {
				return act, fmt.Errorf("create log group: %w", err)
			}
			// Raced with Lambda's implicit creation, or with a concurrent apply.
			act.Op = OpExists
		}
		if act.Op == "" {
			act.Op = OpCreate
		}
	}

	// Set retention every run. A group Lambda created implicitly — because the
	// function was invoked before this ever ran — retains forever, and that is
	// exactly the case worth correcting rather than leaving alone.
	retention := g.retentionDays
	if retention <= 0 {
		retention = DefaultLogRetentionDays
	}
	if _, err := g.api.PutRetentionPolicy(ctx, &cwl.PutRetentionPolicyInput{
		LogGroupName:    aws.String(g.group),
		RetentionInDays: aws.Int32(retention),
	}); err != nil {
		return act, fmt.Errorf("set log retention: %w", err)
	}
	if _, err := g.api.TagResource(ctx, &cwl.TagResourceInput{
		ResourceArn: aws.String(logGroupARN(g)),
		Tags:        Tags(g.group),
	}); err != nil {
		// Tagging a log group is best-effort: some accounts restrict it, and a
		// missing tag on a log group does not leave anything billing that teardown
		// cannot find by name. Everything that *does* bill is tagged.
		act.Detail = fmt.Sprintf("retention %dd (untagged: %v)", retention, err)
		return act, nil
	}
	act.Detail = fmt.Sprintf("retention %dd", retention)
	return act, nil
}

func (g *logGroup) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: g.kind(), Name: g.group}
	exists, err := g.exists(ctx)
	if err != nil {
		return act, err
	}
	if !exists {
		act.Op = OpAbsent
		return act, nil
	}
	if _, err := g.api.DeleteLogGroup(ctx, &cwl.DeleteLogGroupInput{
		LogGroupName: aws.String(g.group),
	}); err != nil {
		return act, fmt.Errorf("delete log group: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

// exists checks by exact name. DescribeLogGroups takes a *prefix*, so the result is
// filtered — a prefix match would report "/aws/lambda/foray-gateway" as existing
// when only "/aws/lambda/foray-gateway-v2" does.
func (g *logGroup) exists(ctx context.Context) (bool, error) {
	out, err := g.api.DescribeLogGroups(ctx, &cwl.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(g.group),
	})
	if err != nil {
		return false, fmt.Errorf("describe log groups: %w", err)
	}
	for _, lg := range out.LogGroups {
		if aws.ToString(lg.LogGroupName) == g.group {
			return true, nil
		}
	}
	return false, nil
}

// logGroupARN is only needed for tagging; the trailing :* is the form the Logs API
// expects for a log-group resource ARN.
func logGroupARN(g *logGroup) string {
	return fmt.Sprintf("arn:%s:logs:%s:%s:log-group:%s:*", partition, g.region, g.accountID, g.group)
}

// lambdaLogGroup is the conventional name Lambda itself would use, so creating it
// ahead of time actually claims the group the function will write to.
func lambdaLogGroup(funcName string) string { return "/aws/lambda/" + funcName }

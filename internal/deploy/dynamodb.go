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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// dynamoAPI is the slice of DynamoDB this package uses. Narrow by habit (the
// house pattern in internal/gateway and internal/export) so tests need no SDK.
type dynamoAPI interface {
	DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	CreateTable(ctx context.Context, in *dynamodb.CreateTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DeleteTable(ctx context.Context, in *dynamodb.DeleteTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error)
	UpdateTimeToLive(ctx context.Context, in *dynamodb.UpdateTimeToLiveInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
	DescribeTimeToLive(ctx context.Context, in *dynamodb.DescribeTimeToLiveInput, opts ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
}

// sessionsTable is the session<->instance map and the per-question cost receipts,
// under one composite key (see internal/gateway/dynamo.go for the layout:
// pk=SESSION#<id>/sk=META and pk=QUESTION#<id>/sk=RECEIPT#<n>).
//
// Mirrors deploy/terraform/dynamodb.tf: on-demand billing, TTL on `expires`, no
// point-in-time recovery. Each of those is a resting-cost decision, not a
// preference — provisioned capacity bills around the clock, and PITR adds a
// standing charge to a table whose whole contents expire.
type sessionsTable struct {
	api      dynamoAPI
	table    string
	waitFor  time.Duration // how long to wait for ACTIVE; 0 → defaultTableWait
	pollFunc func(time.Duration)
}

// TTLAttribute is the item attribute DynamoDB expires on. Must match
// internal/gateway's `expires` — a mismatch means rows accumulate forever, which
// turns a $0-at-rest table into a growing bill.
const TTLAttribute = "expires"

// defaultTableWait bounds the wait for a new table to become ACTIVE. TTL cannot be
// configured until it is, so this is on the critical path of a first deploy.
const defaultTableWait = 2 * time.Minute

func (t *sessionsTable) kind() string { return "dynamodb table" }
func (t *sessionsTable) name() string { return t.table }

func (t *sessionsTable) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: t.kind(), Name: t.table}

	existing, err := t.describe(ctx)
	if err != nil {
		return act, err
	}
	if existing != nil {
		act.Op = OpExists
		// Report — rather than silently correct — a table that exists with the
		// wrong billing mode. Switching it is a cost decision the deployer should
		// make knowingly, and an on-demand table is an invariant (#31), so saying
		// nothing would be the worst option.
		if mode := billingModeOf(existing); mode != string(ddbtypes.BillingModePayPerRequest) {
			act.Detail = fmt.Sprintf("WARNING: billing mode is %s, not PAY_PER_REQUEST — provisioned capacity bills at rest", mode)
			return act, nil
		}
		// A pre-existing table may predate the TTL setting; converge it.
		if err := t.ensureTTL(ctx); err != nil {
			return act, err
		}
		return act, nil
	}

	_, err = t.api.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(t.table),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		Tags: ddbTags(Tags(t.table)),
	})
	if err != nil {
		return act, fmt.Errorf("create table: %w", err)
	}
	if err := t.waitActive(ctx); err != nil {
		return act, err
	}
	if err := t.ensureTTL(ctx); err != nil {
		return act, err
	}
	act.Op = OpCreate
	act.Detail = "on-demand, TTL on " + TTLAttribute
	return act, nil
}

func (t *sessionsTable) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: t.kind(), Name: t.table}
	existing, err := t.describe(ctx)
	if err != nil {
		return act, err
	}
	if existing == nil {
		act.Op = OpAbsent
		return act, nil
	}
	if _, err := t.api.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(t.table)}); err != nil {
		return act, fmt.Errorf("delete table: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

// describe returns the table's description, or nil when it does not exist.
// ResourceNotFoundException is the expected "absent" answer, not a failure — this
// is how the package substitutes discovery for a state file.
func (t *sessionsTable) describe(ctx context.Context) (*ddbtypes.TableDescription, error) {
	out, err := t.api.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(t.table)})
	if err != nil {
		var notFound *ddbtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("describe table: %w", err)
	}
	return out.Table, nil
}

// waitActive polls until the table leaves CREATING. TTL cannot be set on a table
// that is not ACTIVE, so this is not optional politeness.
func (t *sessionsTable) waitActive(ctx context.Context) error {
	limit := t.waitFor
	if limit <= 0 {
		limit = defaultTableWait
	}
	const interval = 2 * time.Second
	sleep := t.pollFunc
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(limit)
	for {
		desc, err := t.describe(ctx)
		if err != nil {
			return err
		}
		if desc != nil && desc.TableStatus == ddbtypes.TableStatusActive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("table %s did not become ACTIVE within %s", t.table, limit)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sleep(interval)
	}
}

// ensureTTL turns on expiry for the TTL attribute, and is a no-op when it is
// already enabled — UpdateTimeToLive errors if asked to enable what is already on.
func (t *sessionsTable) ensureTTL(ctx context.Context) error {
	cur, err := t.api.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(t.table)})
	if err != nil {
		return fmt.Errorf("describe ttl: %w", err)
	}
	if d := cur.TimeToLiveDescription; d != nil {
		switch d.TimeToLiveStatus {
		case ddbtypes.TimeToLiveStatusEnabled, ddbtypes.TimeToLiveStatusEnabling:
			return nil
		}
	}
	_, err = t.api.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(t.table),
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			AttributeName: aws.String(TTLAttribute),
			Enabled:       aws.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("enable ttl on %s: %w", TTLAttribute, err)
	}
	return nil
}

// billingModeOf reads the effective billing mode, defaulting to PROVISIONED the
// way DynamoDB does: the summary is absent on an older provisioned table, and
// reading that absence as on-demand would hide exactly the case worth warning
// about.
func billingModeOf(d *ddbtypes.TableDescription) string {
	if d == nil || d.BillingModeSummary == nil || d.BillingModeSummary.BillingMode == "" {
		return string(ddbtypes.BillingModeProvisioned)
	}
	return string(d.BillingModeSummary.BillingMode)
}

// ddbTags converts the common tag map to DynamoDB's tag shape.
func ddbTags(m map[string]string) []ddbtypes.Tag {
	out := make([]ddbtypes.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, ddbtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

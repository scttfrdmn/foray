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

package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// dynamoAPI is the minimal slice of the DynamoDB client the store uses — the
// seam a fake replaces in tests (no AWS). Touch is one UpdateItem so the hot
// path on every trace is a single write; Get/Put are GetItem/PutItem.
type dynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// DynamoStore is the prod session<->instance map (ARCHITECTURE.md §2/§6.1). It
// is per-invocation friendly — no caches, no background loops — so it drops onto
// a cold Lambda and the control plane rests at ~$0. It deliberately does NOT
// implement the enumerator capability: a Scan per /healthz would be wasteful, so
// /healthz degrades to plain liveness (see http.go). A compile-time assertion in
// the test file pins that decision.
type DynamoStore struct {
	api   dynamoAPI
	table string
	// ttl is how far past last_request a session row should live before DynamoDB
	// TTL reaps it, so the table self-cleans and resting cost stays at $0. The
	// reap is a backstop only — spawn terminates the instance long before.
	ttl time.Duration
}

// DefaultSessionTTL is the row lifetime backstop. A session's instance is reaped
// by spawn's TTL + idle in minutes; this just keeps the mapping row from lingering
// in the table indefinitely.
const DefaultSessionTTL = 24 * time.Hour

// NewDynamoStore wraps a DynamoDB client for the given table. Build the client
// once per cold start (dynamodb.NewFromConfig) and hand it in — this package
// stays free of SDK construction and credential handling.
func NewDynamoStore(c *dynamodb.Client, table string) *DynamoStore {
	return &DynamoStore{api: c, table: table, ttl: DefaultSessionTTL}
}

// sessionItem is the persisted shape of a Session row. The composite key
// (pk=SESSION#<id>, sk=META) leaves room in the same table for per-question cost
// receipts (sk=RECEIPT#<rung>) without a second table — both billed per-write on
// the on-demand table. `expires` is the DynamoDB TTL attribute (epoch seconds).
type sessionItem struct {
	PK          string    `dynamodbav:"pk"`
	SK          string    `dynamodbav:"sk"`
	ID          string    `dynamodbav:"session_id"`
	InstanceID  string    `dynamodbav:"instance_id"`
	WorkerURL   string    `dynamodbav:"worker_url"`
	LastRequest time.Time `dynamodbav:"last_request"`
	Expires     int64     `dynamodbav:"expires"`
}

// sessionPK builds the partition key for a session's rows.
func sessionPK(sessionID string) string { return "SESSION#" + sessionID }

// metaSK names the session-metadata row within a session's partition.
const metaSK = "META"

// key returns the primary key for a session's META row.
func (s *DynamoStore) key(sessionID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: sessionPK(sessionID)},
		"sk": &types.AttributeValueMemberS{Value: metaSK},
	}
}

// Get resolves a session's instance/worker mapping. A missing item maps to
// ErrUnknownSession so the HTTP layer renders 404 rather than 500.
func (s *DynamoStore) Get(ctx context.Context, sessionID string) (Session, error) {
	out, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       s.key(sessionID),
	})
	if err != nil {
		return Session{}, fmt.Errorf("dynamo get %s: %w", sessionID, err)
	}
	if len(out.Item) == 0 {
		return Session{}, ErrUnknownSession
	}
	var it sessionItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return Session{}, fmt.Errorf("dynamo get %s: unmarshal: %w", sessionID, err)
	}
	return Session{
		ID:          it.ID,
		InstanceID:  it.InstanceID,
		WorkerURL:   it.WorkerURL,
		LastRequest: it.LastRequest,
	}, nil
}

// Put writes the full session row on registration (after spawn launches the
// instance). LastRequest starts at the row's creation; Touch advances it.
func (s *DynamoStore) Put(ctx context.Context, sess Session) error {
	last := sess.LastRequest
	if last.IsZero() {
		last = time.Now()
	}
	item, err := attributevalue.MarshalMap(sessionItem{
		PK:          sessionPK(sess.ID),
		SK:          metaSK,
		ID:          sess.ID,
		InstanceID:  sess.InstanceID,
		WorkerURL:   sess.WorkerURL,
		LastRequest: last,
		Expires:     last.Add(s.ttl).Unix(),
	})
	if err != nil {
		return fmt.Errorf("dynamo put %s: marshal: %w", sess.ID, err)
	}
	if _, err := s.api.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("dynamo put %s: %w", sess.ID, err)
	}
	return nil
}

// Touch is the load-bearing idle-bridge write: it stamps last_request_time on
// every trace so a model-holding worker isn't read as idle between two traces
// (ARCHITECTURE.md §6.1). In the deployed control plane this DynamoDB timestamp
// IS the durable idle signal — a spawn-side consumer reads it, rather than the
// gateway shelling out to `spawn extend` from a Lambda. Implemented as a single
// UpdateItem (one write unit), and a ConditionExpression so touching an unknown
// session surfaces ErrUnknownSession rather than silently creating a stub row.
func (s *DynamoStore) Touch(ctx context.Context, sessionID string, at time.Time) error {
	_, err := s.api.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 s.key(sessionID),
		UpdateExpression:    aws.String("SET last_request = :t, expires = :e"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberS{Value: at.Format(time.RFC3339Nano)},
			":e": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", at.Add(s.ttl).Unix())},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrUnknownSession
		}
		return fmt.Errorf("dynamo touch %s: %w", sessionID, err)
	}
	return nil
}

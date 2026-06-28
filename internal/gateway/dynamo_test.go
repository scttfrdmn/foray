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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDynamo records inputs and returns canned outputs/errors, so DynamoStore's
// key/expression construction and result mapping run with no AWS (mirrors
// internal/spore's stubRunner). It also serves as a tiny in-memory table so a
// Put→Get round-trip is exercisable.
type fakeDynamo struct {
	items map[string]map[string]types.AttributeValue // pk|sk -> item

	getCalls, putCalls, updateCalls, queryCalls int
	lastUpdate                                  *dynamodb.UpdateItemInput
	lastPut                                     *dynamodb.PutItemInput

	getErr, putErr, updateErr, queryErr error
}

func newFakeDynamo() *fakeDynamo {
	return &fakeDynamo{items: map[string]map[string]types.AttributeValue{}}
}

func itemKey(item map[string]types.AttributeValue) string {
	pk, _ := item["pk"].(*types.AttributeValueMemberS)
	sk, _ := item["sk"].(*types.AttributeValueMemberS)
	var pkv, skv string
	if pk != nil {
		pkv = pk.Value
	}
	if sk != nil {
		skv = sk.Value
	}
	return pkv + "|" + skv
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &dynamodb.GetItemOutput{Item: f.items[itemKey(in.Key)]}, nil
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putCalls++
	f.lastPut = in
	if f.putErr != nil {
		return nil, f.putErr
	}
	f.items[itemKey(in.Item)] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updateCalls++
	f.lastUpdate = in
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	// Apply the SET so a Touch-then-Get observes the new timestamp.
	if item, ok := f.items[itemKey(in.Key)]; ok {
		if t, ok := in.ExpressionAttributeValues[":t"]; ok {
			item["last_request"] = t
		}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

// Query returns every stored item whose pk matches :pk and whose sk begins with
// :sk — enough of the contract to exercise the receipt partition scan without a
// real DynamoDB.
func (f *fakeDynamo) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queryCalls++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	pk, _ := in.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS)
	skPrefix, _ := in.ExpressionAttributeValues[":sk"].(*types.AttributeValueMemberS)
	var out []map[string]types.AttributeValue
	for _, item := range f.items {
		ipk, _ := item["pk"].(*types.AttributeValueMemberS)
		isk, _ := item["sk"].(*types.AttributeValueMemberS)
		if pk == nil || ipk == nil || ipk.Value != pk.Value {
			continue
		}
		if skPrefix != nil && isk != nil && !strings.HasPrefix(isk.Value, skPrefix.Value) {
			continue
		}
		out = append(out, item)
	}
	return &dynamodb.QueryOutput{Items: out}, nil
}

func newTestStore() (*DynamoStore, *fakeDynamo) {
	f := newFakeDynamo()
	return &DynamoStore{api: f, table: "foray-sessions", ttl: DefaultSessionTTL}, f
}

func TestDynamoStorePutGetRoundTrip(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()
	want := Session{
		ID:          "sess-abc",
		InstanceID:  "i-0123",
		WorkerURL:   "http://worker.internal:8000",
		LastRequest: time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC),
	}
	if err := s.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "sess-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.InstanceID != want.InstanceID || got.WorkerURL != want.WorkerURL {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if !got.LastRequest.Equal(want.LastRequest) {
		t.Fatalf("LastRequest: got %v want %v", got.LastRequest, want.LastRequest)
	}
}

func TestDynamoStorePutSetsExpiresTTL(t *testing.T) {
	s, f := newTestStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Put(context.Background(), Session{ID: "s1", LastRequest: at}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	exp, ok := f.lastPut.Item["expires"].(*types.AttributeValueMemberN)
	if !ok {
		t.Fatal("expires attribute missing or not numeric")
	}
	if want := at.Add(DefaultSessionTTL).Unix(); exp.Value != strconv.FormatInt(want, 10) {
		t.Fatalf("expires = %s, want %d", exp.Value, want)
	}
}

func TestDynamoStoreGetMissingIsUnknownSession(t *testing.T) {
	s, _ := newTestStore()
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("want ErrUnknownSession, got %v", err)
	}
}

func TestDynamoStoreTouchIsSingleUpdate(t *testing.T) {
	s, f := newTestStore()
	ctx := context.Background()
	if err := s.Put(ctx, Session{ID: "s1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	at := time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Touch(ctx, "s1", at); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if f.updateCalls != 1 {
		t.Fatalf("Touch issued %d UpdateItem calls, want 1", f.updateCalls)
	}
	// The hot path must touch last_request (and roll the TTL); it must not be a
	// read-modify-write.
	if got := *f.lastUpdate.UpdateExpression; !strings.Contains(got, "last_request") {
		t.Fatalf("UpdateExpression %q does not set last_request", got)
	}
	if f.lastUpdate.ConditionExpression == nil {
		t.Fatal("Touch must condition on session existence")
	}
	// The new timestamp is observable on a subsequent Get.
	got, err := s.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get after Touch: %v", err)
	}
	if !got.LastRequest.Equal(at) {
		t.Fatalf("LastRequest after Touch: got %v want %v", got.LastRequest, at)
	}
}

func TestDynamoStoreTouchUnknownSession(t *testing.T) {
	s, f := newTestStore()
	f.updateErr = &types.ConditionalCheckFailedException{}
	err := s.Touch(context.Background(), "ghost", time.Now())
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("want ErrUnknownSession from a conditional check failure, got %v", err)
	}
}

// TestDynamoStoreNotEnumerator pins the design decision: the prod store must NOT
// satisfy the enumerator capability (a Scan per /healthz would be wasteful), so
// /healthz degrades to plain liveness. The in-memory MemStore, by contrast, does
// enumerate for the richer fake/local health payload.
func TestDynamoStoreNotEnumerator(t *testing.T) {
	var store Store = &DynamoStore{}
	if _, ok := store.(enumerator); ok {
		t.Fatal("DynamoStore must not implement enumerator (no Scan per /healthz)")
	}
	var mem Store = NewMemStore()
	if _, ok := mem.(enumerator); !ok {
		t.Fatal("MemStore should implement enumerator for the fake /healthz payload")
	}
}

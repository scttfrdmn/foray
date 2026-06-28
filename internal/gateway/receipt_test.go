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
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// QuestionID is stable and whitespace/case-insensitive so the client (which
// carries the question text, not an id) and the server land on the same
// partition, and trivially-different spellings of the same question collide.
func TestQuestionIDStableAndNormalized(t *testing.T) {
	a := QuestionID("Why does it store France as Paris?")
	b := QuestionID("  why   does it store france as paris? ")
	if a != b {
		t.Fatalf("QuestionID not normalized: %q vs %q", a, b)
	}
	if a == QuestionID("a different question") {
		t.Fatal("QuestionID collided across distinct questions")
	}
	if len(a) != 16 {
		t.Fatalf("QuestionID width = %d, want 16", len(a))
	}
}

// SummarizeReceipts folds per-rung rows into one view: rungs sorted by index,
// SpentUSD is the running total (carried on the top rung), BudgetUSD survives.
func TestSummarizeReceipts(t *testing.T) {
	sum := SummarizeReceipts([]Receipt{
		{QuestionID: "q1", QuestionText: "why?", Rung: 1, SpentUSD: 0.42, BudgetUSD: 5},
		{QuestionID: "q1", QuestionText: "why?", Rung: 0, SpentUSD: 0.08, BudgetUSD: 5},
	})
	if sum.RungsRun != 2 {
		t.Fatalf("RungsRun = %d, want 2", sum.RungsRun)
	}
	if sum.Rungs[0].Rung != 0 || sum.Rungs[1].Rung != 1 {
		t.Fatalf("rungs not sorted by index: %+v", sum.Rungs)
	}
	if sum.SpentUSD != 0.42 {
		t.Fatalf("SpentUSD = %v, want the running total 0.42", sum.SpentUSD)
	}
	if sum.BudgetUSD != 5 {
		t.Fatalf("BudgetUSD = %v, want 5", sum.BudgetUSD)
	}
	if sum.QuestionText != "why?" {
		t.Fatalf("QuestionText = %q, want it carried", sum.QuestionText)
	}
}

// RecordReceipt/LoadReceipts ride the optional ReceiptStore capability: a store
// that implements it persists and reads back; the seam folds on read.
func TestRecordAndLoadReceiptsMemStore(t *testing.T) {
	ctx := context.Background()
	var store Store = NewMemStore()

	qid := QuestionID("why does it store France as Paris?")
	for _, r := range []Receipt{
		{QuestionID: qid, Rung: 0, SpentUSD: 0.08, BudgetUSD: 5, Model: "GPT-2"},
		{QuestionID: qid, Rung: 1, SpentUSD: 0.50, BudgetUSD: 5, Model: "Llama-3-8B"},
	} {
		if err := RecordReceipt(ctx, store, r); err != nil {
			t.Fatalf("RecordReceipt: %v", err)
		}
	}

	sum, ok, err := LoadReceipts(ctx, store, qid)
	if err != nil {
		t.Fatalf("LoadReceipts: %v", err)
	}
	if !ok {
		t.Fatal("MemStore should report the ReceiptStore capability")
	}
	if sum.RungsRun != 2 || sum.SpentUSD != 0.50 {
		t.Fatalf("summary = %+v, want 2 rungs / $0.50 spent", sum)
	}
}

// Re-recording the same rung overwrites rather than duplicates, so a retried
// approve does not double-count the question's spend.
func TestRecordReceiptIdempotentPerRung(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	qid := QuestionID("q")
	_ = RecordReceipt(ctx, store, Receipt{QuestionID: qid, Rung: 0, SpentUSD: 0.08, BudgetUSD: 5})
	_ = RecordReceipt(ctx, store, Receipt{QuestionID: qid, Rung: 0, SpentUSD: 0.08, BudgetUSD: 5})
	sum, _, _ := LoadReceipts(ctx, store, qid)
	if sum.RungsRun != 1 {
		t.Fatalf("RungsRun = %d after re-recording rung 0, want 1 (overwrite, not append)", sum.RungsRun)
	}
}

// A store with no ReceiptStore capability is a silent no-op for both seams, so
// callers never need to type-assert.
func TestReceiptSeamNoOpWithoutCapability(t *testing.T) {
	ctx := context.Background()
	var store Store = noReceiptStore{}
	if err := RecordReceipt(ctx, store, Receipt{QuestionID: "q", Rung: 0}); err != nil {
		t.Fatalf("RecordReceipt on a plain store should no-op, got %v", err)
	}
	sum, ok, err := LoadReceipts(ctx, store, "q")
	if err != nil || ok || sum.RungsRun != 0 {
		t.Fatalf("LoadReceipts on a plain store: sum=%+v ok=%v err=%v", sum, ok, err)
	}
}

// noReceiptStore is a Store that does NOT keep receipts — it exercises the
// capability-absent path of the seam.
type noReceiptStore struct{}

func (noReceiptStore) Get(context.Context, string) (Session, error)   { return Session{}, nil }
func (noReceiptStore) Put(context.Context, Session) error             { return nil }
func (noReceiptStore) Touch(context.Context, string, time.Time) error { return nil }

// DynamoStore persists receipts under the question partition and reads them
// back via Query — the schema the table reserved (pk=QUESTION#, sk=RECEIPT#).
func TestDynamoStoreReceiptRoundTrip(t *testing.T) {
	s, f := newTestStore()
	ctx := context.Background()
	qid := QuestionID("why does it store France as Paris?")
	at := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	for _, r := range []Receipt{
		{QuestionID: qid, QuestionText: "why?", Rung: 0, SessionID: "s0", Model: "GPT-2", EstCostUSD: 0.08, SpentUSD: 0.08, BudgetUSD: 5, At: at},
		{QuestionID: qid, QuestionText: "why?", Rung: 1, SessionID: "s1", Model: "Llama-3-8B", EstCostUSD: 0.42, SpentUSD: 0.50, BudgetUSD: 5, At: at},
	} {
		if err := s.PutReceipt(ctx, r); err != nil {
			t.Fatalf("PutReceipt: %v", err)
		}
	}

	got, err := s.Receipts(ctx, qid)
	if err != nil {
		t.Fatalf("Receipts: %v", err)
	}
	sum := SummarizeReceipts(got)
	if sum.RungsRun != 2 || sum.SpentUSD != 0.50 || sum.BudgetUSD != 5 {
		t.Fatalf("summary = %+v, want 2 rungs / $0.50 / $5 budget", sum)
	}

	// The receipt row carries the reserved TTL so the table self-cleans → $0.
	exp, ok := f.lastPut.Item["expires"].(*types.AttributeValueMemberN)
	if !ok {
		t.Fatal("receipt row missing the expires TTL attribute")
	}
	if want := at.Add(DefaultSessionTTL).Unix(); exp.Value != strconv.FormatInt(want, 10) {
		t.Fatalf("receipt expires = %s, want %d", exp.Value, want)
	}
}

// An unrecorded question yields an empty slice, not an error.
func TestDynamoStoreReceiptsEmpty(t *testing.T) {
	s, _ := newTestStore()
	got, err := s.Receipts(context.Background(), QuestionID("never asked"))
	if err != nil {
		t.Fatalf("Receipts on empty question: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no receipts, got %d", len(got))
	}
}

// DynamoStore must satisfy ReceiptStore — the durable page path depends on it.
func TestDynamoStoreIsReceiptStore(t *testing.T) {
	var store Store = &DynamoStore{}
	if _, ok := store.(ReceiptStore); !ok {
		t.Fatal("DynamoStore must implement ReceiptStore for durable per-question receipts")
	}
}

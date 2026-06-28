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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Receipt is a durable per-rung cost record for one question's ladder. It is
// what makes "$-so-far on this question" survive a page reload or a fresh CLI
// invocation: the in-memory Ladder is ephemeral (the stateless web contract
// throws it away between calls), but the spend the human approved is a fact
// worth keeping. One row per approved rung; the question's total is aggregated
// on read (SummarizeReceipts). Carries cost references only — never tensors,
// honoring the no-automatic-egress invariant like every other boundary here.
type Receipt struct {
	QuestionID   string    // stable hash of the question text (QuestionID)
	QuestionText string    // the load-bearing invariant, kept for display
	Rung         int       // ladder index this spend bought
	SessionID    string    // the session the rung launched
	Technique    string    // e.g. "logit lens"
	Model        string    // e.g. "GPT-2"
	EstCostUSD   float64   // this rung's estimate (what was booked)
	SpentUSD     float64   // cumulative spend on the question through this rung
	BudgetUSD    float64   // the per-question envelope (same across a ladder)
	At           time.Time // when the rung was approved
}

// ReceiptStore is the optional capability a Store may implement to persist and
// retrieve per-question cost receipts. It is deliberately separate from Store
// (the session<->instance mapping) and from enumerator (the /healthz Scan): a
// store can map sessions without keeping receipts. The loop writes through it
// best-effort — a failed receipt write never fails a trace — and the page reads
// through it for an authoritative $-so-far. DynamoStore and MemStore implement
// it; callers type-assert exactly as the HTTP layer does for enumerator.
type ReceiptStore interface {
	PutReceipt(ctx context.Context, r Receipt) error
	Receipts(ctx context.Context, questionID string) ([]Receipt, error)
}

// QuestionID derives a stable identifier from the question text so the same
// question maps to the same receipt partition on every call — the web client
// (which carries the question text, not an id) and the server compute it
// identically. Whitespace-normalized and case-folded so trivially-different
// spellings of the same question still collide on purpose; truncated because a
// partition-key prefix needs only to be collision-resistant, not full-width.
func QuestionID(text string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:16]
}

// ReceiptSummary is a question's receipts aggregated for display: rungs run and
// dollars spent against the envelope. It is what `foray sessions` and the page's
// cost meter render when the live Ladder is gone.
type ReceiptSummary struct {
	QuestionID   string
	QuestionText string
	BudgetUSD    float64
	SpentUSD     float64
	RungsRun     int
	Rungs        []Receipt
}

// SummarizeReceipts folds a question's per-rung rows into a single view. SpentUSD
// is the largest cumulative figure seen (rows may arrive out of order, and the
// top rung carries the running total); BudgetUSD is taken from any row that
// recorded one. Rungs are returned sorted by index for a stable display.
func SummarizeReceipts(rs []Receipt) ReceiptSummary {
	out := ReceiptSummary{Rungs: append([]Receipt(nil), rs...)}
	sort.Slice(out.Rungs, func(i, j int) bool { return out.Rungs[i].Rung < out.Rungs[j].Rung })
	out.RungsRun = len(out.Rungs)
	for _, r := range out.Rungs {
		if r.SpentUSD > out.SpentUSD {
			out.SpentUSD = r.SpentUSD
		}
		if r.BudgetUSD > 0 {
			out.BudgetUSD = r.BudgetUSD
		}
		if out.QuestionID == "" {
			out.QuestionID = r.QuestionID
			out.QuestionText = r.QuestionText
		}
	}
	return out
}

// RecordReceipt persists a receipt through a store if it implements ReceiptStore,
// and is a no-op (nil) otherwise. It is the seam the loop calls best-effort after
// an approved rung: a store that can't keep receipts (or a future one) silently
// skips, so the caller need not type-assert. The caller decides what to do with
// a real write error (the loop logs and continues — a lost receipt must never
// fail a trace).
func RecordReceipt(ctx context.Context, store Store, r Receipt) error {
	rs, ok := store.(ReceiptStore)
	if !ok {
		return nil
	}
	return rs.PutReceipt(ctx, r)
}

// LoadReceipts reads a question's receipts through a store if it implements
// ReceiptStore, returning an empty summary otherwise. The bool reports whether
// the store could enumerate receipts at all (vs. a genuinely empty question).
func LoadReceipts(ctx context.Context, store Store, questionID string) (ReceiptSummary, bool, error) {
	rs, ok := store.(ReceiptStore)
	if !ok {
		return ReceiptSummary{}, false, nil
	}
	receipts, err := rs.Receipts(ctx, questionID)
	if err != nil {
		return ReceiptSummary{}, true, err
	}
	return SummarizeReceipts(receipts), true, nil
}

// questionPK builds the partition key for a question's receipt rows. It is a
// distinct partition from a session's (SESSION#<id>) in the same table, exactly
// as the schema reserved (dynamo.go).
func questionPK(questionID string) string { return "QUESTION#" + questionID }

// receiptSK names a rung's receipt row within a question's partition, zero-padded
// so the rows sort by rung index lexicographically under a Query.
func receiptSK(rung int) string { return fmt.Sprintf("RECEIPT#%04d", rung) }

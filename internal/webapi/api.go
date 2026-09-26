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

// Package webapi is the brain-over-HTTP surface for the page (ARCHITECTURE.md
// §6.8). It exposes the same result-gated loop the CLI walks in-process
// (cmd/foray runLoop) as a small JSON API so the static SPA becomes a thin
// client: propose → (human Go) → approve → trace → interpret → assess → climb,
// rung by rung.
//
// Like internal/gateway's Handler, this is a plain stdlib ServeMux built per
// invocation with no daemon state, so it drops onto a cold Lambda and the
// control plane rests at ~$0 (CLAUDE.md invariants). cmd/foray-web wraps
// Handler() in an http.Server for local/dev and rehearsal; the deploy step's
// Lambda adapter wraps the same Handler unchanged.
//
// The loop is stateful — the Ladder mutates across Approve/Assess — but this
// surface holds none of it: the client carries the Ladder JSON on each call and
// the handler returns the updated copy. Cedar still gates server-side at Approve
// regardless of what the client sends, and the trust model is single-tenant
// self-install (the user editing their own carried budget only spends their own
// money), so client-carried state is sound here. The handler launches a GPU only
// in /api/approve — the human's Go is the sole acceptance node; climbing is a
// fresh Go on the next rung, never automatic (CLAUDE.md invariants).
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/export"
	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/spore"
)

// Deps are the collaborators the loop needs, wired once per mode (fake/real),
// mirroring cmd/foray's *deps. The Brain proposes/interprets; the gateway routes
// the trace and bridges the idle signal; spawn resolves the launched session to
// its worker. Exporter mints the opt-in download.
type Deps struct {
	Brain    *brain.Brain
	Gateway  *gateway.Gateway
	Spawn    spore.Spawn
	Exporter *export.Exporter

	// Now lets tests pin the clock for deterministic receipt timestamps; nil →
	// time.Now. The handlers never sleep or schedule on it.
	Now func() time.Time
}

// now returns the current time, honoring an injected clock for tests.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Handler returns the JSON API surface. It is a stdlib ServeMux (Go 1.22
// method+path patterns) so the same handler serves cmd/foray-web locally and a
// Lambda adapter later — no framework, no router dependency (CLAUDE.md
// §"stdlib-first"). log may be nil; a discarding logger is substituted.
//
//	POST /api/propose   question → clarify | (ladder + first rung)
//	POST /api/approve   ladder + rungIndex → launch, trace, interpret, assess, next
//	GET  /api/receipt   question|id → persisted $-so-far for the question (#47)
//	POST /api/export    sessionId + kind → presigned download (opt-in egress)
//	GET  /healthz       liveness
func Handler(d Deps, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/propose", d.handlePropose(log))
	mux.HandleFunc("POST /api/approve", d.handleApprove(log))
	mux.HandleFunc("POST /api/result", d.handleResult(log))
	mux.HandleFunc("GET /api/receipt", d.handleReceipt(log))
	mux.HandleFunc("POST /api/export", d.handleExport(log))
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

// --- /api/propose ------------------------------------------------------------

type proposeReq struct {
	Question string `json:"question"`
}

// proposeResp is the brain's first move: either a clarifying question back (the
// ask underdetermines the experiment) or the planned ladder plus its first rung
// awaiting Go. Nothing has launched.
type proposeResp struct {
	Clarify  string        `json:"clarify,omitempty"`
	Ladder   *brain.Ladder `json:"ladder,omitempty"`
	Proposal *rungView     `json:"proposal,omitempty"`
}

func (d Deps) handlePropose(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req proposeReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Warn("propose: decode", "err", err)
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Question) == "" {
			writeErr(w, http.StatusBadRequest, "a question is required (naming a model is the wrong first move)")
			return
		}

		ladder, prop, err := d.Brain.Propose(r.Context(), req.Question)
		if err != nil {
			log.Warn("propose", "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		// A clarifying proposal short-circuits: there is no ladder to climb yet.
		if prop != nil && prop.Clarify != "" {
			writeJSON(w, http.StatusOK, proposeResp{Clarify: prop.Clarify})
			return
		}
		writeJSON(w, http.StatusOK, proposeResp{
			Ladder:   ladder,
			Proposal: viewRung(prop.Rung),
		})
	}
}

// --- /api/approve ------------------------------------------------------------

type approveReq struct {
	Ladder    *brain.Ladder `json:"ladder"`
	RungIndex int           `json:"rungIndex"`
}

// Status values for a rung. A trace no longer completes inside one request: the
// deployed control plane cannot reach the worker, so the graph is handed over at launch
// and the worker writes its result back (#66). Approve therefore returns `running`, and
// the page polls /api/result until `done`.
const (
	StatusRunning = "running"
	StatusDone    = "done"
)

// approveResp is one rung's outcome, and also what /api/result returns — the two share
// a shape so the page has one thing to render.
//
// While Status is `running`, Result and Recommendation are absent: the trace is still
// going, and inventing a placeholder finding would be worse than showing none. It
// carries references only, never tensors.
type approveResp struct {
	Status         string          `json:"status"`
	SessionID      string          `json:"sessionId"`
	Result         *resultView     `json:"result,omitempty"`
	Recommendation *recommendation `json:"recommendation,omitempty"`
	Ladder         *brain.Ladder   `json:"ladder"`
	NextProposal   *rungView       `json:"nextProposal,omitempty"`
	SpentUSD       float64         `json:"spentUSD"`
	BudgetUSD      float64         `json:"budgetUSD"`
}

// handleApprove is the HITL acceptance node over HTTP: it runs exactly one
// iteration of the CLI's runLoop. Approve (Cedar gate + launch) is the only
// place a GPU starts; climbing is never automatic — the brain recommends and
// the client must POST a fresh approve on the next rung.
func (d Deps) handleApprove(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req approveReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Warn("approve: decode", "err", err)
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		l := req.Ladder
		if l == nil || req.RungIndex < 0 || req.RungIndex >= len(l.Rungs) {
			writeErr(w, http.StatusBadRequest, "ladder with a valid rungIndex is required")
			return
		}
		ctx := r.Context()
		rung := &l.Rungs[req.RungIndex]
		prop := &brain.Proposal{Rung: rung}

		// Approve is the sole acceptance node: Cedar, then launch. A policy denial
		// (e.g. tier exceeds budget) surfaces here with the reason verbatim.
		sid, err := d.Brain.Approve(ctx, l, prop)
		if err != nil {
			log.Warn("approve", "rung", req.RungIndex, "err", err)
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}

		// Record the session first: Collect resolves through it, and the idle bridge
		// needs a row to stamp.
		if err := d.register(ctx, sid); err != nil {
			log.Warn("approve: register", "session", sid, "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}

		// Hand the graph to the instance instead of reaching the worker.
		//
		// The deployed control plane has no path to the worker — a Lambda cannot hold
		// the SSH forward the CLI's `spawn service` tunnel uses, and the alternatives
		// broke either the ~$0 invariant or the loopback-only posture (#66). It does not
		// need one: the rung runs exactly one trace, so the work is fully known and is
		// written to the session's own bucket prefix for the booting worker to collect.
		//
		// The graph goes over AFTER the launch because its key contains the instance id.
		// The worker waits for it (worker/batch.py), so being a moment late is fine.
		if err := d.Gateway.HandOff(ctx, sid, gateway.Graph{
			Engine:  string(rung.Engine),
			Payload: []byte(rung.NNSight),
		}); err != nil {
			log.Warn("approve: handoff", "session", sid, "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}

		// The receipt records the spend the brain already booked in Approve. It is
		// written here rather than after the trace because the money is committed at
		// launch: the instance is running whether or not the trace succeeds, so a page
		// reload must show that cost. Best-effort — a lost receipt must not fail a rung.
		if err := gateway.RecordReceipt(ctx, d.Gateway.Store, d.receiptFor(l, rung, sid)); err != nil {
			log.Warn("approve: record receipt", "session", sid, "rung", rung.Index, "err", err)
		}

		writeJSON(w, http.StatusOK, approveResp{
			Status:    StatusRunning,
			SessionID: sid,
			Ladder:    l,
			SpentUSD:  l.Spent,
			BudgetUSD: l.Question.BudgetUSD,
		})
	}
}

// receiptFor builds the per-question cost receipt for one approved rung.
//
// It records a fact — the brain already booked the spend in Approve — and never settles
// acceptance. Persisted so a page reload still shows an authoritative $-so-far: the
// stateless contract discards the live Ladder between calls.
func (d Deps) receiptFor(l *brain.Ladder, rung *brain.Rung, sid string) gateway.Receipt {
	return gateway.Receipt{
		QuestionID:   gateway.QuestionID(l.Question.Text),
		QuestionText: l.Question.Text,
		Rung:         rung.Index,
		SessionID:    sid,
		Technique:    rung.Technique,
		Model:        rung.Model.Name,
		EstCostUSD:   rung.EstCostUSD,
		SpentUSD:     l.Spent,
		BudgetUSD:    l.Question.BudgetUSD,
		At:           d.now(),
	}
}

// --- /api/result -------------------------------------------------------------

// resultReq asks whether a launched rung has finished. The ladder rides along because
// the contract is stateless (the server keeps no live Ladder between calls) and
// interpreting a result needs the question it serves.
type resultReq struct {
	Ladder    *brain.Ladder `json:"ladder"`
	RungIndex int           `json:"rungIndex"`
	SessionID string        `json:"sessionId"`
}

// handleResult collects a handed-off trace and, once it lands, does the rest of the
// rung: interpret the finding against the question, assess climb-or-stop, and offer the
// next rung — which still awaits its own fresh Go.
//
// This is the second half of what used to happen inside approve. Splitting it is what
// launch-time handoff requires, and it is also better shaped for a cold Lambda: nothing
// sits holding a connection for the length of a GPU trace.
func (d Deps) handleResult(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req resultReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		l := req.Ladder
		if l == nil || req.RungIndex < 0 || req.RungIndex >= len(l.Rungs) {
			writeErr(w, http.StatusBadRequest, "ladder with a valid rungIndex is required")
			return
		}
		if req.SessionID == "" {
			writeErr(w, http.StatusBadRequest, "sessionId is required")
			return
		}
		ctx := r.Context()
		rung := &l.Rungs[req.RungIndex]

		tr, pending, err := d.Gateway.Collect(ctx, req.SessionID)
		if err != nil {
			// A worker that reported a failure, or a session we cannot resolve. Either
			// way the rung is over; say why rather than let the page poll forever.
			log.Warn("result: collect", "session", req.SessionID, "err", err)
			status := http.StatusBadGateway
			if errors.Is(err, gateway.ErrUnknownSession) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err.Error())
			return
		}
		if pending {
			writeJSON(w, http.StatusOK, approveResp{
				Status:    StatusRunning,
				SessionID: req.SessionID,
				Ladder:    l,
				SpentUSD:  l.Spent,
				BudgetUSD: l.Question.BudgetUSD,
			})
			return
		}

		res, err := d.Brain.Interpret(ctx, l, rung, brain.RawResult{
			SaveRef: tr.SaveRef, VizRef: tr.VizRef, NNSight: tr.NNSight,
		})
		if err != nil {
			log.Warn("result: interpret", "session", req.SessionID, "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}

		rec, err := d.Brain.Assess(ctx, l, res)
		if err != nil {
			log.Warn("result: assess", "session", req.SessionID, "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}

		resp := approveResp{
			Status:         StatusDone,
			SessionID:      req.SessionID,
			Result:         viewResult(res, tr),
			Recommendation: &recommendation{Decision: string(rec.Decision), Reason: rec.Reason},
			Ladder:         l,
			SpentUSD:       l.Spent,
			BudgetUSD:      l.Question.BudgetUSD,
		}
		// The brain recommends climbing — but the next rung is a fresh proposal
		// awaiting its own Go. NextProposal launches nothing.
		if rec.Decision == brain.Climb {
			if next := d.Brain.NextProposal(ctx, l); next != nil {
				resp.NextProposal = viewRung(next.Rung)
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- /api/receipt ------------------------------------------------------------

// receiptResp is a question's persisted cost receipt for the page: rungs run and
// dollars spent against the envelope, authoritative across reloads (the live
// Ladder is gone between calls). Per-rung rows ride along so the page can show a
// breakdown. References only — no tensors.
type receiptResp struct {
	QuestionID   string           `json:"questionId"`
	QuestionText string           `json:"questionText"`
	SpentUSD     float64          `json:"spentUSD"`
	BudgetUSD    float64          `json:"budgetUSD"`
	RungsRun     int              `json:"rungsRun"`
	Rungs        []receiptRowView `json:"rungs"`
}

// receiptRowView is one approved rung's spend in the receipt.
type receiptRowView struct {
	Rung       int     `json:"rung"`
	Model      string  `json:"model"`
	Technique  string  `json:"technique"`
	SessionID  string  `json:"sessionId"`
	EstCostUSD float64 `json:"estCostUSD"`
	SpentUSD   float64 `json:"spentUSD"`
}

// handleReceipt returns the persisted $-so-far for a question, looked up by the
// stable QuestionID the client passes (?question=<text> or ?id=<id>). It is a
// read of recorded facts — nothing launches, nothing is interpreted. When the
// store can't keep receipts (the offline MemStore does; a future store might
// not) it answers an empty receipt, not an error.
func (d Deps) handleReceipt(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			if q := strings.TrimSpace(r.URL.Query().Get("question")); q != "" {
				id = gateway.QuestionID(q)
			}
		}
		if id == "" {
			writeErr(w, http.StatusBadRequest, "id or question query parameter is required")
			return
		}
		sum, _, err := gateway.LoadReceipts(r.Context(), d.Gateway.Store, id)
		if err != nil {
			log.Warn("receipt", "question", id, "err", err)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		resp := receiptResp{
			QuestionID:   sum.QuestionID,
			QuestionText: sum.QuestionText,
			SpentUSD:     sum.SpentUSD,
			BudgetUSD:    sum.BudgetUSD,
			RungsRun:     sum.RungsRun,
			Rungs:        make([]receiptRowView, 0, len(sum.Rungs)),
		}
		if resp.QuestionID == "" {
			resp.QuestionID = id // no rows yet — echo the asked-for id
		}
		for _, row := range sum.Rungs {
			resp.Rungs = append(resp.Rungs, receiptRowView{
				Rung:       row.Rung,
				Model:      row.Model,
				Technique:  row.Technique,
				SessionID:  row.SessionID,
				EstCostUSD: row.EstCostUSD,
				SpentUSD:   row.SpentUSD,
			})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- /api/export -------------------------------------------------------------

type exportReq struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
}

type exportResp struct {
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	ExpiresAt string `json:"expiresAt"`
}

// handleExport mints a presigned download of the user's own saved values — the
// opt-in egress path (ARCHITECTURE.md §6.9). The Cedar export gate runs first;
// a residency/ownership denial surfaces verbatim as 403. In real mode the
// presigner is still a labeled stub until the deploy step (#25), and its honest
// "not wired" message comes back as the error.
func (d Deps) handleExport(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req exportReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Warn("export: decode", "err", err)
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.SessionID == "" {
			writeErr(w, http.StatusBadRequest, "sessionId is required")
			return
		}
		kind := export.Kind(req.Kind)
		if kind == "" {
			kind = export.KindBundle
		}
		link, err := d.Exporter.Export(r.Context(), export.Request{SessionID: req.SessionID, Kind: kind})
		if err != nil {
			var denied *export.Denied
			status := http.StatusBadGateway
			if errors.As(err, &denied) {
				status = http.StatusForbidden
			}
			log.Warn("export", "session", req.SessionID, "err", err)
			writeErr(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, exportResp{
			URL:       link.URL,
			Kind:      string(link.Kind),
			ExpiresAt: link.ExpiresAt.Format("15:04 MST"),
		})
	}
}

// --- /healthz ----------------------------------------------------------------

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- gateway tracer (in-process, mirroring cmd/foray's tracer) ---------------

// register maps a just-launched session to its worker so Route can resolve it,
// exactly as the CLI's tracer does. spawn.Status finds the instance the brain's
// executor launched (in the fake, executor and gateway share one fake spawn).
// register records the session→instance mapping so Collect can resolve it and the idle
// bridge has a row to stamp.
//
// It no longer looks up an address. Under launch-time handoff the deployed path never
// dials the worker, so it does not need the instance's public IP — which also means one
// fewer spawn call per rung, and no dependency on the instance being reachable at all.
// The session id IS the instance id (brain.SpawnExecutor returns it).
func (d Deps) register(ctx context.Context, sid string) error {
	return d.Gateway.Store.Put(ctx, gateway.Session{ID: sid, InstanceID: sid})
}

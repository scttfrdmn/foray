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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Handler returns the gateway's HTTP surface. It is a plain stdlib ServeMux
// (Go 1.22 method+path patterns) so the same handler serves cmd/forayd locally
// and, later, a Lambda adapter — no framework, no router dependency
// (CLAUDE.md §"stdlib-first"). Routes:
//
//	POST /sessions/{id}/trace   route a serialized nnsight graph to the worker
//	GET  /healthz               liveness + per-session last_request_time
//
// log may be nil; a discarding logger is substituted so callers needn't wire one.
func (g *Gateway) Handler(log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{id}/trace", g.handleTrace(log))
	mux.HandleFunc("GET /healthz", g.handleHealthz(log))
	return mux
}

// handleTrace decodes a graph, routes it, and returns the result reference as
// JSON. Quiet on success (one debug line); a warn line keyed by session on
// failure (CLAUDE.md §"quiet on success", issue #46 structured logging).
func (g *Gateway) handleTrace(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		l := log.With("session", sessionID, "route", "trace")

		var graph Graph
		if err := json.NewDecoder(r.Body).Decode(&graph); err != nil {
			l.Warn("decode graph", "err", err)
			writeErr(w, http.StatusBadRequest, "invalid graph payload")
			return
		}

		res, err := g.Route(r.Context(), sessionID, graph)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, ErrUnknownSession) {
				status = http.StatusNotFound
			}
			l.Warn("route", "err", err, "status", status)
			writeErr(w, status, err.Error())
			return
		}

		l.Debug("routed", "engine", graph.Engine, "save_ref", res.SaveRef)
		writeJSON(w, http.StatusOK, res)
	}
}

// healthz is the gateway's liveness + idle-bridge observability payload. Sessions
// reports the count it can see and the most recent request time across them, so
// an operator can confirm the load-bearing timestamp is advancing (issue #46).
type healthz struct {
	Status   string    `json:"status"`
	Time     time.Time `json:"time"`
	Sessions int       `json:"sessions"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// handleHealthz reports liveness and, when the store can enumerate sessions,
// the freshest last_request_time — the metric that proves the idle bridge is
// firing. Stores that don't enumerate (the prod DynamoDB-backed one) simply
// report liveness; the gateway never fails health on a missing capability.
func (g *Gateway) handleHealthz(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := healthz{Status: "ok", Time: g.now()}
		if e, ok := g.Store.(enumerator); ok {
			if sessions, err := e.List(r.Context()); err == nil {
				h.Sessions = len(sessions)
				for _, s := range sessions {
					if s.LastRequest.After(h.LastSeen) {
						h.LastSeen = s.LastRequest
					}
				}
			} else {
				log.Warn("healthz: list sessions", "err", err)
			}
		}
		writeJSON(w, http.StatusOK, h)
	}
}

// enumerator is the optional Store capability /healthz uses to summarize
// last_request_time. The in-memory fake implements it; the prod DynamoDB store
// need not (a Scan per health check would be wasteful), and /healthz degrades to
// plain liveness when it is absent.
type enumerator interface {
	List(ctx context.Context) ([]Session, error)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

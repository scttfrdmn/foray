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

// Command forayd is the gateway: it routes serialized nnsight intervention
// graphs to the live worker for a session and bridges per-session
// last_request_time into spawn's idle signal (ARCHITECTURE.md §6.1). It is the
// one load-bearing new contract; everything else is plumbing.
//
// The gateway logic is per-invocation (no daemon state) so it drops onto a cold
// Lambda and the control plane rests at ~$0. This entrypoint wraps the same
// http.Handler in a local http.Server for dev and for `make demo-fake`-style
// rehearsal; the Lambda adapter (deploy step) wraps Handler() unchanged.
//
// Under FORAY_FAKE=1 it serves the in-memory fake (one seeded session, canned
// worker) with zero AWS — the dev/rehearse path and the CI gate.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/spore"
)

// version/commit are stamped by the Makefile via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var g *gateway.Gateway
	if spore.Enabled() {
		g, _ = gateway.NewFake()
		log.Info("forayd starting (FORAY_FAKE — in-memory, no AWS)", "version", version, "commit", commit, "addr", *addr)
	} else {
		// Real path: stdlib HTTP worker + spore.host spawn for the idle bridge.
		// The session store is the prod DynamoDB-backed Store, wired with the
		// deploy step (worker.go TODO). Until then forayd has no place to read the
		// session<->instance mapping, so refuse rather than pretend.
		log.Error("forayd: real session store not wired yet — run with FORAY_FAKE=1, or wait for the deploy step",
			"version", version, "commit", commit)
		os.Exit(1)
	}

	srv := &http.Server{Addr: *addr, Handler: g.Handler(log)}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("forayd: serve", "err", err)
		os.Exit(1)
	}
}

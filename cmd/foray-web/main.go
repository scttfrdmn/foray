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

// Command foray-web is the dev server for the page (ARCHITECTURE.md §6.8): it
// serves the static SPA in web/ alongside the brain-over-HTTP loop
// (internal/webapi) so local rehearsal is one command. The page becomes a thin
// client over the same result-gated loop the CLI walks.
//
// Like cmd/forayd, the API logic is per-invocation with no daemon state, so the
// deploy step's Lambda adapter wraps webapi.Handler() unchanged and the control
// plane rests at ~$0 (CLAUDE.md invariants). This entrypoint exists only for
// local/dev and `make web-fake`; it is not the production surface.
//
// Under FORAY_FAKE=1 it serves the offline fake loop (the brain's canned
// GPT-2→8B ladder, the gateway's canned worker, the fake exporter) with zero
// AWS — the dev/rehearse path. The real path is refused here, exactly as
// cmd/forayd refuses: the production surface is API Gateway + Lambda over a
// persistent session store, wired in the deploy step.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/scttfrdmn/foray/internal/spore"
	"github.com/scttfrdmn/foray/internal/webapi"
)

// version/commit are stamped by the Makefile via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	webDir := flag.String("web", "web", "directory of the static SPA to serve")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if !spore.Enabled() {
		// Match cmd/forayd's posture: the real surface is API Gateway + Lambda over
		// a persistent session store (deploy step). Until then refuse rather than
		// stand up a half-wired real server.
		log.Error("foray-web: real path is the deploy step's Lambda — run with FORAY_FAKE=1 for local rehearsal",
			"version", version, "commit", commit)
		os.Exit(1)
	}

	deps := webapi.NewFakeDeps()
	log.Info("foray-web starting (FORAY_FAKE — in-memory, no AWS)",
		"version", version, "commit", commit, "addr", *addr, "web", *webDir)

	api := webapi.Handler(deps, log)
	mux := http.NewServeMux()
	// The brain loop API + liveness; the static SPA fetches these.
	mux.Handle("/api/", api)
	mux.Handle("/healthz", api)
	// The page itself.
	mux.Handle("/", http.FileServer(http.Dir(*webDir)))

	srv := &http.Server{Addr: *addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("foray-web: serve", "err", err)
		os.Exit(1)
	}
}

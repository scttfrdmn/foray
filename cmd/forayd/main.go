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
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

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
		var err error
		g, err = buildReal()
		if err != nil {
			log.Error("forayd: wire real gateway", "err", err, "version", version, "commit", commit)
			os.Exit(1)
		}
		log.Info("forayd starting (real — DynamoDB store + HTTP worker)", "version", version, "commit", commit, "addr", *addr)
	}

	srv := &http.Server{Addr: *addr, Handler: g.Handler(log)}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("forayd: serve", "err", err)
		os.Exit(1)
	}
}

// buildReal wires the deployed gateway: a DynamoDB-backed session store, the
// stdlib HTTP worker, and the DynamoDB idle-bridge shim. The gateway never
// launches or terminates instances — it only routes traces and stamps
// last_request_time — so it needs no exec-backed spawn (the `spawn` binary isn't
// present in a Lambda runtime). Touch writes the durable idle signal to DynamoDB;
// a spawn-side consumer reads it (ARCHITECTURE.md §6.1).
func buildReal() (*gateway.Gateway, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	table := os.Getenv("FORAY_SESSIONS_TABLE")
	if table == "" {
		table = "foray-sessions"
	}
	return &gateway.Gateway{
		Store:  gateway.NewDynamoStore(dynamodb.NewFromConfig(cfg), table),
		Worker: gateway.HTTPWorker{Client: &http.Client{Timeout: 10 * time.Minute}},
		Spawn:  spore.NewDynamoIdleBridge(),
	}, nil
}

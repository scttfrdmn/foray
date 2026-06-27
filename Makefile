# foray — Makefile. See CLAUDE.md "Make targets" for the contract.
# Copyright 2026 Scott Friedman. Apache License 2.0.

SHELL       := bash
GO          ?= go
BIN         := bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo 0.0.0)
GIT_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS     := -X main.version=$(VERSION) -X main.commit=$(GIT_COMMIT)

# AWS profile for any AWS-touching target (see CLAUDE.md / project memory).
AWS_PROFILE ?= aws
export AWS_PROFILE

# Worker image (the one Python boundary, build step 5). Device is injected by the
# control plane at run time, not baked in — cuda now, neuron the day TorchNeuron GAs.
PYTHON       ?= python3
WORKER_IMAGE ?= foray-worker:dev
WORKER_DEVICE ?= cuda

.DEFAULT_GOAL := help

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F: '{printf "  \033[1m%-14s\033[0m%s\n", $$1, $$2}'

## build: build foray + forayd + foray-web
.PHONY: build
build:
	@mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/foray ./cmd/foray
	@if [ -d ./cmd/forayd ]; then $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/forayd ./cmd/forayd; \
	 else echo "note: cmd/forayd not implemented yet (build step 4)"; fi
	@if [ -d ./cmd/foray-web ]; then $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/foray-web ./cmd/foray-web; \
	 else echo "note: cmd/foray-web not implemented yet (build step 8)"; fi

## lint: gofmt + go vet + staticcheck
.PHONY: lint
lint: fmt-check
	$(GO) vet ./... || true
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./... || true; \
	 elif command -v golangci-lint >/dev/null 2>&1; then golangci-lint run || true; \
	 else echo "note: install staticcheck or golangci-lint for full lint"; fi

## fmt: format the tree
.PHONY: fmt
fmt:
	gofmt -w .

## fmt-check: fail if any Go file is not gofmt-clean
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## test: go test ./... (no AWS)
.PHONY: test
test:
	$(GO) test ./...

## demo-fake: full intent->plan->Go->fake-spawn->receipt, no AWS (CI gate)
.PHONY: demo-fake
demo-fake:
	@echo "==> demo-fake: walking the loop with FORAY_FAKE=1 (no AWS)"
	FORAY_FAKE=1 $(GO) run ./cmd/foray run "why does the model store France as Paris?" --yes

## web-fake: serve the page + brain-loop API offline (FORAY_FAKE, no AWS)
.PHONY: web-fake
web-fake:
	@echo "==> web-fake: serving web/ + /api over the fake loop on http://localhost:8090 (no AWS)"
	FORAY_FAKE=1 $(GO) run ./cmd/foray-web

## license-check: verify every source file carries an Apache header
.PHONY: license-check
license-check:
	@bash scripts/license-check.sh

## worker: build the nnsight worker image (LanguageModel + VLLM, one image)
.PHONY: worker
worker:
	docker build -t $(WORKER_IMAGE) --build-arg FORAY_DEVICE=$(WORKER_DEVICE) -f worker/Dockerfile .

## worker-test: pytest the worker under FORAY_FAKE=1 (no GPU, no AWS) — CI gate
.PHONY: worker-test
worker-test:
	@echo "==> worker-test: pytest with FORAY_FAKE=1 (no GPU, no AWS)"
	FORAY_FAKE=1 $(PYTHON) -m pytest worker/tests -q

## worker-fake: run the worker locally in fake mode (uvicorn on :8000)
.PHONY: worker-fake
worker-fake:
	FORAY_FAKE=1 $(PYTHON) -m uvicorn worker.app:app --host 127.0.0.1 --port 8000

## worker-smoke: MANUAL real GPU/AWS smoke — never run in CI (see worker/README.md)
.PHONY: worker-smoke
worker-smoke:
	@echo "==> worker-smoke: real GPU + AWS (gpt2 logit-lens -> S3). Not a CI target."
	$(PYTHON) -m worker.smoke

## deploy: IaC up (S3+CloudFront, API GW+Lambda, IAM, Cedar, DDB)
.PHONY: deploy
deploy:
	@echo "note: deploy (IaC) not implemented yet (build step 9). See issues."

## teardown: IaC down — leave nothing running, nothing billing
.PHONY: teardown
teardown:
	@echo "note: teardown not implemented yet (build step 9). See issues."

## clean: remove build artifacts
.PHONY: clean
clean:
	rm -rf $(BIN) dist coverage.out coverage.html

<!--
Copyright 2026 Scott Friedman. Apache License 2.0.
-->

# foray worker

The one Python boundary in foray (ARCHITECTURE.md §6.7): it runs a serialized
`nnsight` intervention graph interleaved with the forward pass on the session's
ephemeral GPU and returns **references** to saved values in S3 (in-region) — never
tensors.

## How a rung reaches it (`spawn task run`)

**Nothing connects to the worker.** A session runs exactly one trace, so the work is
fully known before the instance exists, and the control plane hands it over through
the user's own bucket instead of a network path (issues #66, #103):

```
s3://<bucket>/sessions/<id>/graph.json   →  /tmp/graph.json     staged in by spawn
/tmp/result.json                         →  .../result.json     staged out by spawn
```

`spawn task run` pulls this directory's image from ECR onto an AMI carrying the NVIDIA
driver, stages the graph in **before the container execs**, runs
`python3 -m worker.batch`, stages the result out after it exits, and terminates the
instance — including when the trace failed. So `worker/batch.py` does no S3 I/O for the
handoff at all: it reads a file and writes a file.

There is no inbound path, no open port, and no VPC, interface endpoint or NAT — which
is what keeps the control plane at ~$0, and what lets the CLI and the deployed page use
the same code.

A failure is written as `{"error": "..."}` rather than left absent, because an absent
result is indistinguishable from a trace still running. spawn's own completion record
covers the harder case: a container that never reaches Python at all.

## The HTTP server (`worker/app.py`)

The FastAPI server predates the task path and remains for local development and the
manual GPU smoke — `POST /trace` runs the same `engine.run` that `worker.batch` does,
so the two entrypoints cannot diverge in what a trace actually does, only in how the
work arrives. It is **not** how foray runs a rung.

## Wire contract

| Endpoint | Request | Response |
| --- | --- | --- |
| `POST /trace` | `{"engine": "eager"\|"vllm"\|"", "payload": "<base64>"}` | `{"session_id", "save_ref", "viz_ref", "nnsight"}` |
| `GET /healthz` | — | `{"status", "device", "engine_default", "fake"}` |

The same envelope is what the staged `graph.json` holds, so both entrypoints reach
`worker/graph.py` identically.

`payload` is base64 because Go marshals `[]byte` as a base64 JSON string. Its
*interior* (the intervention envelope: `{prompt, saves[], layers[], backward,
engine}`) is opaque to the control plane by design — `worker/graph.py` owns it and is
the seam where real `nnsight` graph deserialization plugs in.

The response carries **references only**. Saved activations land in the user's own
S3 bucket in-region (`worker/saves.py`); only the `s3://` ref, a rendered-viz ref,
and the generated `nnsight` code cross back. This is the no-automatic-egress
invariant — a user exports their own bytes through the separate opt-in path
(`internal/export`).

## Engines (routed per request, §3)

- **`eager`** — `nnsight.LanguageModel`. Full transparency, arbitrary module
  access, activation edits, **gradients**. The universal path; empty/unknown
  `engine` defers here.
- **`vllm`** — `nnsight` VLLM. Paged-attention throughput, text-gen only, **no
  gradients**. A gradient (`backward`) request on `vllm` is rejected with a `400`
  (`worker/engine.py`, issue #49) rather than silently differing.

## Device target

Selected by name (`FORAY_DEVICE`, default `cuda`), never hardcoded — `nnsight`
needs eager PyTorch with live module boundaries and autograd, not CUDA
specifically. `cuda` is enabled; `neuron` (Trainium) is registered-but-disabled and
refused until TorchNeuron GAs (`worker/device.py`), the same three-layer gate as the
Go registry (`internal/device/neuron.go`) and Cedar (`engine == "neuron"` forbidden).

## Develop & test (no GPU, no AWS)

Python here is managed with **[uv](https://docs.astral.sh/uv/)**.
`worker/pyproject.toml` declares the dependencies and `worker/uv.lock` pins them,
so CI, the image and your laptop resolve identically. There are no
`requirements*.txt` files.

Heavy deps (`torch`/`nnsight`/`vllm`) live in the **`gpu` extra** and are imported
lazily inside the real paths, so the fake path and the unit tests need only the
base set — no GPU, no AWS, no torch.

```bash
make worker-sync     # uv sync --project worker  (base + dev, from the lock)

make worker-test     # pytest under FORAY_FAKE=1 — the CI gate
make worker-lint     # ruff
make worker-fake     # the server in fake mode (uvicorn on :8000)
make worker-serve-fake   # loopback + a readiness
                         # line on stdout — handy for eyeballing the #66 contract
```

Run uv from the repo root with `--project worker` (what the Make targets do): the
package imports as `worker.*`, so the repo root must be the working directory.

The GPU stack installs only where there is a GPU:

```bash
uv sync --project worker --extra gpu     # the smoke box / the image
```

Poke the fake server:

```bash
curl localhost:8000/healthz
PAYLOAD=$(printf '{"prompt":"France'\''s capital is"}' | base64)
curl -s localhost:8000/trace -H 'content-type: application/json' \
  -d "{\"engine\":\"eager\",\"payload\":\"$PAYLOAD\"}"
```

## Container image (issues #50, #103)

One image holds both engines; the device is injected at run time. The image is not
optional infrastructure — it *is* how the worker gets onto the GPU, since
`spawn task run` launches from it.

```bash
make worker        # build locally -> $(WORKER_IMAGE)
make worker-push   # build linux/amd64 and push to the ECR repo `foray deploy` created
```

`worker-push` prints the `FORAY_WORKER_IMAGE` to export. A rung refuses to launch
without it rather than starting a GPU with nothing to run.

## Manual GPU/AWS smoke (not CI)

CI never runs this. It exercises the real path on a real NVIDIA GPU with real AWS
credentials: load a small model, run a logit-lens trace, confirm a `save_ref` lands
in S3. It refuses to run unless you opt in explicitly.

```bash
FORAY_GPU_SMOKE=1 AWS_PROFILE=aws \
  FORAY_SAVE_BUCKET=your-bucket-us-east-1 FORAY_SAVE_REGION=us-east-1 \
  make worker-smoke
```

### Reproducible EC2 recipe

Run from a workstation with the spore `spawn` CLI and `AWS_PROFILE=aws`. This
launches an ephemeral G7e, builds + smoke-tests the image on it, and tears it down —
the same ephemeral-by-default shape the architecture promises.

```bash
# 1. Launch an ephemeral GPU box (spawn = spore.host; see internal/spore).
#    g7e.2xlarge = RTX PRO 6000, the "mid" tier (internal/device/device.go).
spawn launch --name foray-worker-smoke --instance-type g7e.2xlarge \
  --ami-tag nvidia-pytorch --ttl 60m -o json | tee /tmp/smoke.json
INSTANCE_ID=$(jq -r .id /tmp/smoke.json)
HOST=$(jq -r .public_dns /tmp/smoke.json)

# 2. Ship the repo and build the image on the instance.
rsync -a --exclude .git ./ "ec2-user@$HOST:~/foray/"
ssh "ec2-user@$HOST" 'cd foray && make worker'

# 3. Run the smoke on the GPU (gpt2 logit-lens -> save_ref in S3).
ssh "ec2-user@$HOST" \
  "cd foray && FORAY_GPU_SMOKE=1 AWS_PROFILE=aws \
     FORAY_SAVE_BUCKET=your-bucket-us-east-1 FORAY_SAVE_REGION=us-east-1 \
     make worker-smoke"

# 4. Tear down — leave nothing running, nothing billing.
spawn terminate "$INSTANCE_ID"
```

In production this whole dance is one `spawn task run`: `make worker-push` publishes
the image once, and each rung launches from it, stages its graph in, and terminates.
The recipe above is the by-hand version for validating the worker against real
hardware without involving the control plane.

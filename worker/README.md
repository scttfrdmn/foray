<!--
Copyright 2026 Scott Friedman. Apache License 2.0.
-->

# foray worker

The one Python boundary in foray (ARCHITECTURE.md §6.7): a FastAPI server that
receives a serialized `nnsight` intervention graph from `forayd`, runs it
interleaved with the forward pass on the session's ephemeral GPU, and returns
**references** to saved values in S3 (in-region) — never tensors.

The Go control plane (`forayd`) routes graphs to this server over the VPC. The wire
contract is fixed by `internal/gateway/worker.go`; this server matches it verbatim.

## How it is reached (`spawn service`)

The worker is **not** exposed on a public port. `forayd` has no free network path
to a GPU instance — VPC-attaching a Lambda to reach a private one pulls in
interface endpoints and NAT, which bill hourly and break the "control plane rests
at ~$0" invariant — so foray reaches the worker through spawn's own verb for this
(issue #66):

```
spawn service --host <instance> -o json -- \
  env FORAY_SESSION_ID=… FORAY_MODEL_URI=… python3 -m worker.serve
```

spawn appends `--addr 127.0.0.1:0`, runs the command on the instance, and forwards
a local port to whatever the worker binds. The worker listens on **loopback only**
and is reachable solely through that forward.

`worker/serve.py` implements spawn's readiness contract
(spawn `docs/service-readiness-contract.md`): after binding — never before, since
the resolved port is the whole point — it prints exactly one line to stdout and
flushes it.

```json
{"event":"ready","addr":"127.0.0.1:54321","token":"…","provenance":{"service":"foray-worker"}}
```

spawn carries the `token` into the URL it reports, and foray presents it back as
`Authorization: Bearer <token>` on `/trace` (a header, so the credential stays out
of logs and error strings; `?token=` is also accepted so the URL spawn prints
works if a human pastes it). `/healthz` needs no token — a liveness probe that
fails for want of a credential is a liveness probe that fails for the wrong
reason.

Binding anything but loopback is refused outright rather than honored: the tunnel
is the only intended way in, and a typo should not quietly widen the blast radius.

## Wire contract

| Endpoint | Request | Response |
| --- | --- | --- |
| `POST /trace` | `{"engine": "eager"\|"vllm"\|"", "payload": "<base64>"}` | `{"session_id", "save_ref", "viz_ref", "nnsight"}` |
| `GET /healthz` | — | `{"status", "device", "engine_default", "fake"}` |

`payload` is base64 because Go marshals `[]byte` as a base64 JSON string. Its
*interior* (the intervention envelope: `{prompt, saves[], layers[], backward,
engine}`) is opaque to `forayd` by design — `worker/graph.py` owns it and is the
seam where real `nnsight` graph deserialization plugs in.

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

Heavy deps (`torch`/`nnsight`/`vllm`/`boto3`) are imported lazily inside the real
paths, so the fake path and the unit tests need only the base requirements.

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install -r worker/requirements.txt

make worker-test     # pytest under FORAY_FAKE=1 — the CI gate
make worker-fake     # run the server locally in fake mode (uvicorn on :8000)
```

Poke the fake server:

```bash
curl localhost:8000/healthz
PAYLOAD=$(printf '{"prompt":"France'\''s capital is"}' | base64)
curl -s localhost:8000/trace -H 'content-type: application/json' \
  -d "{\"engine\":\"eager\",\"payload\":\"$PAYLOAD\"}"
```

## Container image (issue #50)

One image holds both engines; the device is injected at run time.

```bash
make worker                       # docker build -> $(WORKER_IMAGE), WORKER_DEVICE=cuda
```

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

In production this whole dance is what `forayd` + `spawn` automate per session; the
recipe is the by-hand version for validating the worker against real hardware.

# Copyright 2026 Scott Friedman
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Single foray worker image (issue #50): one image holds both serving engines —
# nnsight LanguageModel (eager) and VLLM — and routes per request (§3). The device
# target is injected by the control plane at run time (FORAY_DEVICE), not baked in,
# so the same image serves cuda today and neuron the day TorchNeuron GAs.
#
# This image is how the worker reaches the GPU: `spawn task run` pulls it from ECR
# onto an instance whose AMI carries the NVIDIA driver, stages the graph in, and runs
# `python3 -m worker.batch` (issue #103). Before that, nothing put the worker on the
# instance at all and no real trace could run.
#
# Build + publish:  make worker-push     (builds linux/amd64, pushes to ECR)
# Build only:       docker build -t foray-worker:dev -f worker/Dockerfile .
# Run one trace by hand, as the task does:
#   docker run --gpus all -e FORAY_SESSION_ID=... -e FORAY_MODEL_URI=... \
#     -e FORAY_SAVE_BUCKET=... -v /tmp/graph.json:/tmp/graph.json \
#     foray-worker:dev python3 -m worker.batch

# CUDA + cuDNN runtime base; torch wheels bring their own CUDA libs but the GDS
# (cuFile) fast path wants the system CUDA runtime present.
FROM nvidia/cuda:12.6.2-cudnn-runtime-ubuntu24.04

ARG FORAY_DEVICE=cuda
ENV FORAY_DEVICE=${FORAY_DEVICE} \
    PYTHONUNBUFFERED=1 \
    UV_NO_CACHE=1 \
    # uv sync builds the environment at worker/.venv; put it first on PATH so the
    # CMD below runs the resolved interpreter without an activation dance.
    PATH="/app/worker/.venv/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# uv is the project's Python toolchain (see worker/pyproject.toml). Copied from
# the official image rather than curl|sh so the version is pinned and the build
# fetches no scripts.
COPY --from=ghcr.io/astral-sh/uv:0.12.19 /uv /usr/local/bin/uv

WORKDIR /app

# Install deps first for layer caching: the GPU stack is the slow layer.
#
#   --locked   install exactly uv.lock, and fail if it is stale, so the image and
#              CI cannot silently diverge
#   --extra gpu  the torch/nnsight/vllm stack — the whole point of this image, and
#              the one thing CI's environment does not have
#   --no-dev   pytest/ruff are for developing the worker, not running it
COPY worker/pyproject.toml worker/uv.lock ./worker/
RUN uv sync --project worker --locked --extra gpu --no-dev

# The worker package imports as `worker.*` from the build context root. The
# repo-root .dockerignore excludes worker/.venv — without it a host venv would
# land on top of the one uv just built.
COPY worker/ ./worker/

EXPOSE 8000
# Single-tenant: the user's own code on the user's own GPU; no sandbox layer.
#
# The default CMD is the HTTP server, for reaching a container over a published port
# — the manual smoke shape and local development. It is NOT how foray runs a rung:
# `spawn task run` passes `python3 -m worker.batch` as the command, which replaces
# this, reads the staged /tmp/graph.json, runs exactly one trace, writes
# /tmp/result.json and exits. Keeping the server as the default is deliberate: an
# image whose default did nothing but exit would be confusing to run by hand.
CMD ["uvicorn", "worker.app:app", "--host", "0.0.0.0", "--port", "8000"]

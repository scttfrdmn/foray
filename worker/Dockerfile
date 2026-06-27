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
# Build:  docker build -t foray-worker:dev -f worker/Dockerfile .
# Run:    docker run --gpus all -e FORAY_SESSION_ID=... -e FORAY_MODEL_URI=... \
#                    -e FORAY_SAVE_BUCKET=... -p 8000:8000 foray-worker:dev

# CUDA + cuDNN runtime base; torch wheels bring their own CUDA libs but the GDS
# (cuFile) fast path wants the system CUDA runtime present.
FROM nvidia/cuda:12.6.2-cudnn-runtime-ubuntu24.04

ARG FORAY_DEVICE=cuda
ENV FORAY_DEVICE=${FORAY_DEVICE} \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install deps first for layer caching: the GPU stack is the slow layer.
COPY worker/requirements.txt worker/requirements-gpu.txt ./worker/
RUN python3 -m pip install --break-system-packages -r worker/requirements-gpu.txt

# The worker package imports as `worker.*` from the build context root.
COPY worker/ ./worker/

EXPOSE 8000
# Single-tenant: the user's own code on the user's own GPU; no sandbox layer.
CMD ["uvicorn", "worker.app:app", "--host", "0.0.0.0", "--port", "8000"]

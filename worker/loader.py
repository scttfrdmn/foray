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

"""GDS loader — stream weights S3 -> HBM on boot (issue #14).

GPUDirect Storage streams a large checkpoint from S3 straight into GPU memory in
tens of seconds, which is the enabling fact behind "nothing kept warm" (§1): cold
start is no longer a reason to keep a model resident. This loader tries the GDS fast
path (cuFile via kvikio) and falls back to a plain download + from_pretrained when
GDS isn't available, so the worker runs on any NVIDIA box. The path is sharded-ready
for the multi-GPU large tier. Everything heavy is imported lazily.
"""

from __future__ import annotations

from dataclasses import dataclass

from .device import Device


@dataclass(frozen=True)
class ModelHandle:
    """What the engine needs to construct a LanguageModel/VLLM. `model` is either a
    local path/HF id (transformers resolves it) or an in-memory model when the GDS
    path has already materialized weights in HBM."""

    model: object
    loaded_via: str  # "gds" | "download" | "hf" — for the boot log / smoke check


def load(model_uri: str, device: Device, shards: int = 1) -> ModelHandle:
    """Stream the checkpoint into HBM and return a handle.

    model_uri is a resolved checkpoint location (an s3:// URI for GDS, or an HF id /
    local path). The control plane (internal/catalog) has already resolved the model
    source to a HF-format checkpoint; the worker only loads.
    """
    if model_uri.startswith("s3://"):
        try:
            return _load_gds(model_uri, device, shards)
        except _GDSUnavailable:
            return _load_download(model_uri, device)
    # HF id or local path: transformers resolves and caches it.
    return ModelHandle(model=model_uri, loaded_via="hf")


class _GDSUnavailable(Exception):
    """Raised internally when the cuFile/kvikio GDS path can't be used, so load()
    falls back to a plain download."""


def _load_gds(model_uri: str, device: Device, shards: int) -> ModelHandle:
    """GPUDirect Storage fast path: cuFile (via kvikio) streams safetensors shards
    from S3 directly into device memory, bypassing the CPU bounce buffer."""
    try:
        import kvikio  # noqa: F401, PLC0415  (presence check; real impl streams here)
    except ImportError as exc:
        raise _GDSUnavailable(str(exc)) from exc
    # TODO(worker): sharded cuFile reads of the safetensors index into HBM across
    # `shards` GPUs. Until the multi-GPU large tier ships we validate the fast path
    # exists and defer to download; the seam (shards, device) is in place.
    raise _GDSUnavailable("sharded GDS read not yet implemented; using download")


def _load_download(model_uri: str, device: Device) -> ModelHandle:
    """Fallback: download the checkpoint from S3 to local disk, then let
    transformers load it. Slower than GDS but works on any instance."""
    import os  # noqa: PLC0415
    import tempfile  # noqa: PLC0415

    import boto3  # noqa: PLC0415  (lazy: real path only)

    bucket, _, key_prefix = model_uri[len("s3://"):].partition("/")
    dest = tempfile.mkdtemp(prefix="foray-model-")
    s3 = boto3.client("s3")
    paginator = s3.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, Prefix=key_prefix):
        for obj in page.get("Contents", []):
            key = obj["Key"]
            local = os.path.join(dest, os.path.basename(key))
            s3.download_file(bucket, key, local)
    return ModelHandle(model=dest, loaded_via="download")

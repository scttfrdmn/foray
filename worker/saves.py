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

"""Save activations to S3 in-region — and hand back only a reference.

This is where the no-automatic-egress invariant is enforced (CLAUDE.md, §8): saved
values are written to the user's own bucket in-region and the worker returns an
`s3://` URI, never the tensors. Pixels (a rendered viz) and references are the only
things that leave; a user who wants the bytes uses the separate opt-in export path
(internal/export). boto3/torch are imported lazily so the fake path needs neither.
"""

from __future__ import annotations


def put(settings, captured: dict) -> str:
    """Serialize captured activations and upload them to S3 in-region, returning the
    `s3://` prefix they live under. Never returns tensors.

    `captured` maps module path -> saved nnsight proxy (resolved to a tensor after
    the trace exits). We serialize with safetensors and stream to the user's bucket.
    """
    import io  # noqa: PLC0415

    import torch  # noqa: PLC0415  (lazy: real path only)
    from safetensors.torch import save as st_save  # noqa: PLC0415

    # Flatten proxies to detached CPU tensors keyed by a filesystem-safe name.
    tensors = {}
    for path, proxy in captured.items():
        value = getattr(proxy, "value", proxy)
        if isinstance(value, torch.Tensor):
            tensors[path.replace(".", "_")] = value.detach().to("cpu")

    buf = io.BytesIO(st_save(tensors))
    key = f"{settings.save_prefix}saves.safetensors"
    _upload(settings, key, buf.getvalue())

    # A reference, not the bytes. The bucket is in-region (asserted by the control
    # plane); nothing tensor-shaped crosses back to the gateway.
    return f"s3://{settings.save_bucket}/{settings.save_prefix}"


def _upload(settings, key: str, body: bytes) -> None:
    """Upload one object to the in-region save bucket."""
    import boto3  # noqa: PLC0415  (lazy: real path only)

    s3 = boto3.client("s3", region_name=settings.save_region)
    s3.put_object(Bucket=settings.save_bucket, Key=key, Body=body)

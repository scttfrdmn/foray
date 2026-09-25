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

"""Entrypoint that makes the worker spawnable by `spawn service` (issue #66).

`forayd` has no network path to a GPU worker: VPC-attaching a Lambda to reach a
private instance pulls in interface endpoints and NAT, which bill hourly and
break the "control plane rests at ~$0" invariant. spawn already solves this —
`spawn service` runs a long-lived HTTP service on an instance and forwards a
local port to it over SSH, so the worker binds **loopback only** and is never
exposed to the internet.

The price of admission is one line of stdout. spawn's readiness contract
(spawn docs/service-readiness-contract.md) is:

    {"event":"ready","addr":"127.0.0.1:54321","token":"...","provenance":{...}}

  event   discriminator, so ordinary log lines are ignored
  addr    the address actually bound, after :0 resolution
  token   optional access credential, carried into the URL spawn prints

Three details the contract is emphatic about, all honored below: announce
*after* binding (the point of the line is the resolved port), flush (a buffered
line is indistinguishable from a service that never started), and keep logs on
stderr so stdout stays one parseable line.

Run it the way spawn does:

    python3 -m worker.serve --addr 127.0.0.1:0
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
import socket
import sys

DEFAULT_ADDR = "127.0.0.1:0"

# Bytes of entropy for the session token. 32 bytes -> a 43-char urlsafe string;
# far past guessing, and the token never leaves the machine pair (foray's client
# and this process) except through spawn's own stdout.
TOKEN_BYTES = 32


def parse_addr(addr: str) -> tuple[str, int]:
    """Split HOST:PORT. Port 0 means "let the OS choose", which is the normal case
    under spawn — only the service can know what is free."""
    host, sep, port = addr.rpartition(":")
    if not sep:
        raise ValueError(f"--addr must be HOST:PORT, got {addr!r}")
    try:
        return host, int(port)
    except ValueError as exc:
        raise ValueError(f"--addr port must be a number, got {port!r}") from exc


def bind(addr: str) -> socket.socket:
    """Bind a listening socket, refusing anything but loopback.

    The refusal is deliberate rather than defensive: the tunnel is the only
    intended way in, and an unauthenticated-looking service on 0.0.0.0 is
    reachable by anything that can route to the instance. A typo in --addr should
    fail loudly here, not quietly widen the blast radius.
    """
    host, port = parse_addr(addr)
    if host not in {"127.0.0.1", "localhost", "::1"}:
        raise ValueError(
            f"refusing to bind {host!r}: the worker must listen on loopback and be "
            "reached through spawn's tunnel, never exposed directly"
        )
    sock = socket.socket(socket.AF_INET6 if host == "::1" else socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, port))
    sock.listen(128)
    return sock


def announce(sock: socket.socket, token: str, out=None) -> None:
    """Print the readiness line for the bound socket, then flush.

    The address comes from the socket, never from the requested --addr: after
    ":0" the requested value still says port 0 while the socket knows the real
    one, and the resolved port is the entire reason this line exists.
    """
    out = sys.stdout if out is None else out
    host, port = sock.getsockname()[:2]
    line = {
        "event": "ready",
        "addr": f"{host}:{port}",
        "token": token,
        # Opaque to spawn; it logs this as "what exactly got spawned".
        "provenance": {
            "service": "foray-worker",
            "device": os.environ.get("FORAY_DEVICE", "cuda"),
            "fake": os.environ.get("FORAY_FAKE", "") == "1",
        },
    }
    out.write(json.dumps(line) + "\n")
    out.flush()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="worker.serve", description=__doc__)
    parser.add_argument(
        "--addr",
        default=DEFAULT_ADDR,
        help="loopback address to bind (spawn appends --addr 127.0.0.1:0)",
    )
    args = parser.parse_args(argv)

    sock = bind(args.addr)
    token = secrets.token_urlsafe(TOKEN_BYTES)

    # Import uvicorn and the app only once the socket is bound: a bind failure
    # should not pay for loading the app (and, on a real box, torch).
    import uvicorn

    from .app import app, set_token

    set_token(token)
    announce(sock, token)

    # uvicorn serves the socket we bound and announced — letting it bind its own
    # would race with the line we just printed and could land on a different port.
    config = uvicorn.Config(app, log_config=None, access_log=False)
    uvicorn.Server(config).run(sockets=[sock])
    return 0


if __name__ == "__main__":  # pragma: no cover - process entrypoint
    sys.exit(main())

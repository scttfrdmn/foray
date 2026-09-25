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

"""Tests for the spawn-service entrypoint (issue #66).

The readiness line is a contract with another program, so these assert its exact
shape rather than just that the code runs. No GPU, no AWS, no uvicorn: binding a
loopback socket is all the real machinery needed.
"""

from __future__ import annotations

import io
import json
import socket

import pytest

from worker import serve


class TestParseAddr:
    def test_host_and_port(self) -> None:
        assert serve.parse_addr("127.0.0.1:8000") == ("127.0.0.1", 8000)

    def test_zero_port_is_allowed(self) -> None:
        # The normal case under spawn: only the service can know what is free.
        assert serve.parse_addr("127.0.0.1:0") == ("127.0.0.1", 0)

    def test_ipv6_loopback(self) -> None:
        assert serve.parse_addr("::1:0") == ("::1", 0)

    @pytest.mark.parametrize("bad", ["127.0.0.1", "", "127.0.0.1:http"])
    def test_rejects_malformed(self, bad: str) -> None:
        with pytest.raises(ValueError):
            serve.parse_addr(bad)


class TestBind:
    def test_binds_loopback_and_resolves_port(self) -> None:
        sock = serve.bind("127.0.0.1:0")
        try:
            host, port = sock.getsockname()[:2]
            assert host == "127.0.0.1"
            # :0 must have been resolved to a real port — the whole point of the
            # readiness line is reporting this number.
            assert port > 0
        finally:
            sock.close()

    @pytest.mark.parametrize("host", ["0.0.0.0", "10.0.1.7", "example.internal"])
    def test_refuses_non_loopback(self, host: str) -> None:
        """A non-loopback bind is refused, not quietly honored.

        The tunnel is the only intended way in; binding 0.0.0.0 would expose the
        worker to anything that can route to the instance.
        """
        with pytest.raises(ValueError, match="loopback"):
            serve.bind(f"{host}:0")


class TestAnnounce:
    def test_readiness_line_shape(self) -> None:
        sock = serve.bind("127.0.0.1:0")
        try:
            out = io.StringIO()
            serve.announce(sock, "tok-123", out=out)
            lines = out.getvalue().splitlines()
            assert len(lines) == 1, "spawn scans stdout line by line; emit exactly one"

            line = json.loads(lines[0])
            assert line["event"] == "ready", "the discriminator spawn filters on"
            assert line["token"] == "tok-123"

            # The address must be the one actually bound, not the requested ":0".
            host, port = sock.getsockname()[:2]
            assert line["addr"] == f"{host}:{port}"
            assert not line["addr"].endswith(":0")
        finally:
            sock.close()

    def test_announces_after_binding(self) -> None:
        """The announced port must be live, since spawn forwards to it immediately.

        Connecting proves the socket is listening by the time the line is out —
        announcing before the bind would hand spawn a port nothing answers on.
        """
        sock = serve.bind("127.0.0.1:0")
        try:
            out = io.StringIO()
            serve.announce(sock, "tok", out=out)
            addr = json.loads(out.getvalue())["addr"]
            host, port = addr.rsplit(":", 1)
            with socket.create_connection((host, int(port)), timeout=2):
                pass
        finally:
            sock.close()

"""A fake transport for tests: records every byte, can be told to fail."""

from __future__ import annotations

import asyncio
from collections.abc import Callable

from .base import ConnectError, Transport, TransportTimeout, WriteError


class FakeTransport(Transport):
    name = "fake"

    def __init__(self, *, keep_alive: bool = True) -> None:
        self.keep_alive = keep_alive
        self.written = bytearray()
        self.writes: list[bytes] = []
        self.connected = False
        self.connects = 0
        self.closes = 0
        # Knobs.
        self.fail_connect = False
        self.fail_write_after: int | None = None  # fail once this many bytes are written
        self.timeout_write = False
        self.write_delay = 0.0
        # Reply to a written command: fn(command bytes) -> reply bytes or None.
        self.responder: Callable[[bytes], bytes | None] | None = None
        self._rx = bytearray()

    @property
    def is_connected(self) -> bool:
        return self.connected

    async def connect(self) -> None:
        if self.connected:
            return
        if self.fail_connect:
            raise ConnectError("Could not reach the printer over Bluetooth: fake offline")
        self.connected = True
        self.connects += 1

    async def close(self) -> None:
        if self.connected:
            self.closes += 1
        self.connected = False

    def feed(self, data: bytes) -> None:
        """Queue bytes as if the printer sent them."""
        self._rx += data

    async def write(self, data: bytes) -> int:
        if not self.connected:
            raise WriteError("Not connected to the printer")
        if self.write_delay:
            await asyncio.sleep(self.write_delay)
        if self.timeout_write:
            raise TransportTimeout("Writing to the printer timed out")
        if self.fail_write_after is not None and len(self.written) + len(data) > self.fail_write_after:
            keep = max(0, self.fail_write_after - len(self.written))
            self.written += data[:keep]
            self.connected = False
            raise WriteError(f"Writing to the printer failed after {len(self.written)} bytes: fake")
        self.written += data
        self.writes.append(bytes(data))
        if self.responder is not None:
            reply = self.responder(bytes(data))
            if reply:
                self._rx += reply
        return len(data)

    async def read(self, timeout: float) -> bytes:
        if not self.connected:
            raise ConnectError("Not connected to the printer")
        if not self._rx:
            if timeout > 0:
                await asyncio.sleep(min(timeout, 0.01))
            return b""
        data = bytes(self._rx)
        self._rx.clear()
        return data

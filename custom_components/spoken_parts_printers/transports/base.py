"""Transport interface: a byte pipe to one printer.

Every method is async. Failures raise TransportError with a contract code
(offline, write_failed, timeout) and a short message a person can read.
Nothing here imports Home Assistant.
"""

from __future__ import annotations

import asyncio
import time
from abc import ABC, abstractmethod

from ..drivers.base import OFFLINE, TIMEOUT, WRITE_FAILED


class TransportError(Exception):
    """A transport failure. code is a contract failure code."""

    code = WRITE_FAILED

    def __init__(self, message: str, code: str | None = None) -> None:
        super().__init__(message)
        if code is not None:
            self.code = code
        self.message = message[:200]


class ConnectError(TransportError):
    code = OFFLINE


class WriteError(TransportError):
    code = WRITE_FAILED


class TransportTimeout(TransportError):
    code = TIMEOUT


class Transport(ABC):
    """A connection to one printer.

    keep_alive: stay connected between jobs (Classic SPP for a printer that is
    plugged in). When False the runtime closes the link after each job.
    """

    name: str = ""
    keep_alive: bool = False

    @property
    @abstractmethod
    def is_connected(self) -> bool: ...

    @abstractmethod
    async def connect(self) -> None:
        """Connect if not connected. Raises ConnectError."""

    @abstractmethod
    async def close(self) -> None:
        """Drop the link. Never raises."""

    @abstractmethod
    async def write(self, data: bytes) -> int:
        """Write every byte or raise WriteError. Returns the bytes written."""

    @abstractmethod
    async def read(self, timeout: float) -> bytes:
        """Return whatever bytes arrive within timeout (b"" if none)."""

    async def drain(self) -> None:
        """Discard bytes that are already waiting (old replies)."""
        while await self.read(0.0):
            pass

    async def request(
        self, command: bytes, parse, timeout: float = 2.0
    ):  # parse(bytes) -> value or None while more bytes are needed
        """Send a query and collect the reply until parse() returns a value.

        Raises TransportTimeout if no complete reply arrives in time.
        """
        await self.drain()
        await self.write(command)
        buf = b""
        deadline = time.monotonic() + timeout
        while True:
            left = deadline - time.monotonic()
            if left <= 0:
                raise TransportTimeout("The printer did not answer")
            chunk = await self.read(min(left, 0.5))
            if chunk:
                buf += chunk
                value = parse(buf)
                if value is not None:
                    return value
            else:
                await asyncio.sleep(0)

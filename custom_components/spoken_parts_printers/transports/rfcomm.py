"""Bluetooth Classic serial port (RFCOMM) transport.

Uses Python's own socket.AF_BLUETOOTH / BTPROTO_RFCOMM (Linux BlueZ), which
exists in the Home Assistant OS core container. Blocking socket calls run in a
worker thread with timeouts. The connection stays open between jobs (keep_alive)
and is opened again on the next use after any error.

The printer must be paired with the Home Assistant host first (the P21 needs no
PIN). The P21 accepts one serial connection at a time, so the phone app cannot
connect while Home Assistant holds the link.
"""

from __future__ import annotations

import asyncio
import errno
import functools
import socket
from collections.abc import Callable
from typing import Any

from ..drivers.base import SppParams
from .base import ConnectError, Transport, TransportTimeout, WriteError

_REASONS = {
    errno.EHOSTDOWN: "the printer is off or out of range",
    errno.EHOSTUNREACH: "the printer is off or out of range",
    errno.ECONNREFUSED: "the printer refused the connection (another device may be connected to it)",
    errno.EBUSY: "the printer is busy with another connection",
    errno.ECONNRESET: "the printer dropped the connection",
    errno.ETIMEDOUT: "the printer did not answer in time",
    errno.EACCES: "Home Assistant is not allowed to use Bluetooth Classic sockets",
    errno.EPERM: "Home Assistant is not allowed to use Bluetooth Classic sockets",
}


def _reason(err: OSError) -> str:
    if isinstance(err, TimeoutError):
        return "the printer did not answer in time"
    return _REASONS.get(err.errno or 0, f"Bluetooth error {err.errno or ''}".strip())


def default_socket_factory() -> Any:
    af = getattr(socket, "AF_BLUETOOTH", None)
    proto = getattr(socket, "BTPROTO_RFCOMM", None)
    if af is None or proto is None:
        raise ConnectError(
            "This system has no Bluetooth Classic (RFCOMM) sockets. Use the ble transport."
        )
    return socket.socket(af, socket.SOCK_STREAM, proto)


class RfcommTransport(Transport):
    name = "spp"

    def __init__(
        self,
        address: str,
        params: SppParams,
        *,
        channel: int | None = None,
        keep_alive: bool = True,
        socket_factory: Callable[[], Any] = default_socket_factory,
        sleep: Callable[[float], Any] = asyncio.sleep,
    ) -> None:
        self.address = address.upper()
        self.params = params
        self.channel = channel or params.channel
        self.keep_alive = keep_alive
        self._factory = socket_factory
        self._sleep = sleep
        self._sock: Any = None

    @property
    def is_connected(self) -> bool:
        return self._sock is not None

    async def _run(self, fn: Callable[[], Any], timeout: float) -> Any:
        """Run a blocking call in a worker thread. Errors from the call itself
        (including socket timeouts) are raised as they are; only a stuck
        worker becomes TransportTimeout."""

        def call() -> tuple[bool, Any]:
            try:
                return True, fn()
            except Exception as err:  # noqa: BLE001 (re-raised below)
                return False, err

        loop = asyncio.get_running_loop()
        try:
            ok, value = await asyncio.wait_for(
                loop.run_in_executor(None, call), timeout + 2.0
            )
        except TimeoutError:
            await self.close()
            raise TransportTimeout("The printer did not answer in time") from None
        if not ok:
            raise value
        return value

    async def connect(self) -> None:
        if self._sock is not None:
            return
        p = self.params

        def _connect() -> Any:
            sock = self._factory()
            try:
                sock.settimeout(p.connect_timeout_s)
                sock.connect((self.address, self.channel))
            except BaseException:
                sock.close()
                raise
            return sock

        try:
            self._sock = await self._run(_connect, p.connect_timeout_s)
        except ConnectError:
            raise
        except TransportTimeout:
            raise ConnectError("Could not reach the printer over Bluetooth: it did not answer") from None
        except OSError as err:
            raise ConnectError(f"Could not reach the printer over Bluetooth: {_reason(err)}") from None
        except Exception as err:  # noqa: BLE001 (anything else still means offline)
            raise ConnectError(
                f"Could not reach the printer over Bluetooth: {type(err).__name__}"
            ) from None

    async def close(self) -> None:
        sock, self._sock = self._sock, None
        if sock is None:
            return
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            sock.close()
        except OSError:
            pass

    async def write(self, data: bytes) -> int:
        if self._sock is None:
            raise WriteError("Not connected to the printer")
        sock = self._sock
        p = self.params
        size = max(1, p.chunk_size)
        sent = 0

        def _send(chunk: bytes) -> None:
            sock.settimeout(p.io_timeout_s)
            sock.sendall(chunk)

        for start in range(0, len(data), size):
            chunk = data[start : start + size]
            try:
                await self._run(functools.partial(_send, chunk), p.io_timeout_s)
            except TransportTimeout:
                raise TransportTimeout(
                    f"Writing to the printer timed out after {sent} of {len(data)} bytes"
                ) from None
            except OSError as err:
                await self.close()
                if isinstance(err, TimeoutError):
                    raise TransportTimeout(
                        f"Writing to the printer timed out after {sent} of {len(data)} bytes"
                    ) from None
                raise WriteError(
                    f"Writing to the printer failed after {sent} of {len(data)} bytes: {_reason(err)}"
                ) from None
            sent += len(chunk)
            if p.pacing_s and sent < len(data):
                await self._sleep(p.pacing_s)
        return sent

    async def read(self, timeout: float) -> bytes:
        if self._sock is None:
            raise ConnectError("Not connected to the printer")
        sock = self._sock

        def _recv() -> bytes:
            sock.settimeout(timeout if timeout > 0 else 0.0)
            try:
                data = sock.recv(1024)
            except (TimeoutError, BlockingIOError, InterruptedError):
                return b""
            if not data:
                raise ConnectionResetError(errno.ECONNRESET, "closed")
            return data

        try:
            return await self._run(_recv, timeout)
        except OSError as err:
            await self.close()
            raise ConnectError(f"Lost the printer connection: {_reason(err)}") from None

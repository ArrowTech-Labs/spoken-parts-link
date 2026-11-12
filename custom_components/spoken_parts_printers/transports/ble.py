"""Bluetooth LE (GATT) transport.

Home Assistant's Bluetooth stack (bleak + habluetooth, local adapter or an
ESPHome proxy) finds the device and bleak-retry-connector opens the link. Those
are passed in as two callables so this module has no Home Assistant import and
can be tested with a fake client:

  device_getter() -> BLEDevice | None
      In Home Assistant: async_ble_device_from_address(hass, address, connectable=True)
  connector(device, on_disconnect) -> client
      In Home Assistant: establish_connection(BleakClientWithServiceCache, device,
      name, disconnected_callback=on_disconnect, ...)

Writes go to the write characteristic in chunks (20 bytes for the P21, MTU 23)
paced a few milliseconds apart. Replies arrive as notifications.

Best effort: the P21 only accepts an LE link while no Classic link is up, and
because its LE address equals its Classic address, BlueZ may not list it as an
LE device at all. If Home Assistant has not seen it over LE, connect() says so.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from typing import Any

from ..drivers.base import BleParams
from .base import ConnectError, Transport, TransportTimeout, WriteError


class BleTransport(Transport):
    name = "ble"

    def __init__(
        self,
        address: str,
        params: BleParams,
        *,
        device_getter: Callable[[], Any],
        connector: Callable[[Any, Callable[[Any], None]], Awaitable[Any]],
        keep_alive: bool = False,
        sleep: Callable[[float], Awaitable[Any]] = asyncio.sleep,
    ) -> None:
        self.address = address.upper()
        self.params = params
        self.keep_alive = keep_alive
        self._device_getter = device_getter
        self._connector = connector
        self._sleep = sleep
        self._client: Any = None
        self._rx = bytearray()
        self._rx_event = asyncio.Event()

    @property
    def is_connected(self) -> bool:
        return self._client is not None and bool(getattr(self._client, "is_connected", True))

    def _on_disconnect(self, _client: Any) -> None:
        self._client = None

    def _on_notify(self, _sender: Any, data: bytearray) -> None:
        self._rx += bytes(data)
        self._rx_event.set()

    async def connect(self) -> None:
        if self.is_connected:
            return
        self._client = None
        device = self._device_getter()
        if device is None:
            raise ConnectError(
                "Home Assistant has not seen this printer over Bluetooth LE. "
                "Turn it on, close the phone app, or use the spp transport."
            )
        try:
            async with asyncio.timeout(self.params.connect_timeout_s):
                client = await self._connector(device, self._on_disconnect)
                await client.start_notify(self.params.notify_uuid, self._on_notify)
        except TimeoutError:
            raise ConnectError("Could not connect to the printer over Bluetooth LE: it did not answer") from None
        except ConnectError:
            raise
        except Exception as err:  # noqa: BLE001 (bleak raises many types)
            raise ConnectError(
                f"Could not connect to the printer over Bluetooth LE: {type(err).__name__}"
            ) from None
        self._rx.clear()
        self._client = client

    async def close(self) -> None:
        client, self._client = self._client, None
        if client is None:
            return
        try:
            await client.disconnect()
        except Exception:  # noqa: BLE001 (closing never raises)
            pass

    async def write(self, data: bytes) -> int:
        if not self.is_connected:
            raise WriteError("Not connected to the printer")
        p = self.params
        size = max(1, p.chunk_size)
        sent = 0
        for start in range(0, len(data), size):
            chunk = data[start : start + size]
            try:
                async with asyncio.timeout(10):
                    await self._client.write_gatt_char(
                        p.write_uuid, chunk, response=p.write_with_response
                    )
            except TimeoutError:
                await self.close()
                raise TransportTimeout(
                    f"Writing to the printer timed out after {sent} of {len(data)} bytes"
                ) from None
            except Exception as err:  # noqa: BLE001
                await self.close()
                raise WriteError(
                    f"Writing to the printer failed after {sent} of {len(data)} bytes: {type(err).__name__}"
                ) from None
            sent += len(chunk)
            if p.pacing_s and sent < len(data):
                await self._sleep(p.pacing_s)
        return sent

    async def read(self, timeout: float) -> bytes:
        if not self._rx and timeout > 0:
            self._rx_event.clear()
            try:
                async with asyncio.timeout(timeout):
                    await self._rx_event.wait()
            except TimeoutError:
                pass
        data = bytes(self._rx)
        self._rx.clear()
        return data

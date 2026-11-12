"""Byte transports to printers: spp (RFCOMM), ble (GATT), fake (tests)."""

from .base import ConnectError, Transport, TransportError, TransportTimeout, WriteError

__all__ = ["ConnectError", "Transport", "TransportError", "TransportTimeout", "WriteError"]

"""Driver interface for Spoken Parts Printers.

A driver knows one printer model: what it can print (capabilities), how to turn a
job into the bytes the printer understands (encode), how to ask it for status, and
the per transport parameters (RFCOMM channel, BLE UUIDs, chunk size, pacing).

Nothing in this module imports Home Assistant, so drivers can be unit tested alone.
"""

from __future__ import annotations

import re
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Any, Literal

MediaKind = Literal["die_cut", "continuous", "shipping_4x6"]
MEDIA_KINDS: tuple[str, ...] = ("die_cut", "continuous", "shipping_4x6")
KNOWN_TRANSPORTS: tuple[str, ...] = ("ble", "spp", "usb", "ipp")

# Failure codes from the cross repo contract.
OFFLINE = "offline"
BUSY = "busy"
NO_MEDIA = "no_media"
COVER_OPEN = "cover_open"
BAD_FORMAT = "bad_format"
TOO_BIG = "too_big"
WRITE_FAILED = "write_failed"
TIMEOUT = "timeout"
NO_INTEGRATION = "no_integration"
UNKNOWN_PRINTER = "unknown_printer"
PRINTER_ERROR = "printer_error"
# The job was made for other labels than the ones this printer has loaded.
WRONG_MEDIA = "wrong_media"
FAILURE_CODES: tuple[str, ...] = (
    OFFLINE,
    BUSY,
    NO_MEDIA,
    COVER_OPEN,
    BAD_FORMAT,
    TOO_BIG,
    WRITE_FAILED,
    TIMEOUT,
    NO_INTEGRATION,
    UNKNOWN_PRINTER,
    PRINTER_ERROR,
    WRONG_MEDIA,
)

_MEDIA_ID = re.compile(r"^(\d+(?:\.\d+)?)x(\d+(?:\.\d+)?|C)$")


@dataclass(frozen=True)
class Media:
    """One label size. Ids are "<w>x<h>" in mm, or "<w>xC" for continuous."""

    id: str
    kind: MediaKind
    width_mm: float
    length_mm: float | None = None
    gap_mm: float = 0.0

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "kind": self.kind,
            "width_mm": self.width_mm,
            "length_mm": self.length_mm,
        }


def parse_media_id(media_id: str) -> tuple[float, float | None] | None:
    """Return (width_mm, length_mm or None for continuous), or None if malformed."""
    m = _MEDIA_ID.match(media_id or "")
    if not m:
        return None
    width = float(m.group(1))
    length = None if m.group(2) == "C" else float(m.group(2))
    return width, length


@dataclass(frozen=True)
class Capabilities:
    dpi: int
    print_width_dots: int
    media: tuple[Media, ...]
    default_media: str
    color: bool = False
    transports: tuple[str, ...] = ()
    formats: tuple[str, ...] = ("raster",)
    battery: bool = False

    def __post_init__(self) -> None:
        for t in self.transports:
            if t not in KNOWN_TRANSPORTS and t != "fake":
                raise ValueError(f"unknown transport {t!r}")
        for m in self.media:
            if m.kind not in MEDIA_KINDS:
                raise ValueError(f"unknown media kind {m.kind!r}")
        if self.default_media not in {m.id for m in self.media}:
            raise ValueError("default_media must be one of media")

    def as_dict(self) -> dict[str, Any]:
        return {
            "dpi": self.dpi,
            "print_width_dots": self.print_width_dots,
            "media": [m.as_dict() for m in self.media],
            "color": self.color,
            "transports": list(self.transports),
            "formats": list(self.formats),
        }


@dataclass(frozen=True)
class SppParams:
    """Bluetooth Classic serial port (RFCOMM) parameters."""

    channel: int = 1
    chunk_size: int = 512
    pacing_s: float = 0.0
    connect_timeout_s: float = 10.0
    io_timeout_s: float = 10.0


@dataclass(frozen=True)
class BleParams:
    """Bluetooth LE GATT parameters."""

    service_uuid: str
    write_uuid: str
    notify_uuid: str
    chunk_size: int = 20
    pacing_s: float = 0.012
    write_with_response: bool = False
    connect_timeout_s: float = 20.0


@dataclass(frozen=True)
class Page:
    """One page. For raster: 1 bit per dot, MSB first, rows padded to whole
    bytes, bit 1 = black (contract polarity). For other formats: the file."""

    width: int
    height: int
    data: bytes


@dataclass(frozen=True)
class PrintJob:
    format: str
    media: Media
    copies: int
    pages: tuple[Page, ...]
    job_id: str = ""


@dataclass(frozen=True)
class PrinterStatus:
    """What a status reply means. code is "" when ok, else a contract code."""

    ok: bool
    code: str = ""
    message: str = ""
    busy: bool = False
    raw: int | None = None


@dataclass(frozen=True)
class BatteryStatus:
    level: int
    charging: bool = False


class Driver(ABC):
    """Base class for a printer driver. Subclasses set the class attributes.

    Drivers are stateless: one instance serves every printer of that model.
    """

    id: str = ""
    vendor: str = ""
    model: str = ""
    capabilities: Capabilities
    spp: SppParams | None = None
    ble: BleParams | None = None

    @abstractmethod
    def encode(self, job: PrintJob) -> bytes:
        """Return every byte to send to the printer for this job."""

    # Status. A driver that cannot read status returns None from status_query.
    def status_query(self) -> bytes | None:
        return None

    def parse_status(self, data: bytes) -> PrinterStatus | None:
        """Parse a status reply. Return None while more bytes are needed."""
        raise NotImplementedError

    def battery_query(self) -> bytes | None:
        return None

    def parse_battery(self, data: bytes) -> BatteryStatus | None:
        """Parse a battery reply. Return None while more bytes are needed."""
        raise NotImplementedError

    # Media.
    def media_for(self, media_id: str) -> Media | None:
        """Return the Media for an id, or None if this driver cannot print it."""
        for m in self.capabilities.media:
            if m.id.lower() == media_id.lower():
                return m
        return None

    def transport_params(self, transport: str) -> SppParams | BleParams | None:
        if transport == "spp":
            return self.spp
        if transport == "ble":
            return self.ble
        return None

    def describe(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "vendor": self.vendor,
            "model": self.model,
            "capabilities": self.capabilities.as_dict(),
        }

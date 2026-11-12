"""Nelko P21 and PM220 drivers (TSPL2 over Bluetooth).

Protocol facts and their sources are in PROTOCOL.md. This is original code: it
follows the published protocol notes, it does not reuse any other project's code.

Neither driver sends a settings command. The only commands are the print job
(SIZE, GAP, DIRECTION, DENSITY, CLS, BITMAP, PRINT) and read only queries
(ESC ! ?, BATTERY?). TIMER, BEEP, INITIALPRINTER and SELFTEST are never sent.
"""

from __future__ import annotations

import math

from .base import (
    BUSY,
    COVER_OPEN,
    NO_MEDIA,
    PRINTER_ERROR,
    BatteryStatus,
    BleParams,
    Capabilities,
    Driver,
    Media,
    PrinterStatus,
    PrintJob,
    SppParams,
    parse_media_id,
)
from .registry import register
from .tspl import TsplJob, invert

DPI = 203


def mm_to_dots(mm: float, dpi: int = DPI) -> int:
    return round(mm * dpi / 25.4)


def _ble_uuid(short: int) -> str:
    return f"0000{short:04x}-0000-1000-8000-00805f9b34fb"


# Status byte returned by ESC ! ? (P21). Source: merlinschumacher/nelko-p21-print
# README, "Status byte". Only 0x00 and 0x04 were observed on a real P21 there;
# the other values are from the same table (vendor app), not tested by us.
_P21_STATUS: dict[int, tuple[str, str, bool]] = {
    0x00: ("", "Ready", False),
    0x01: (COVER_OPEN, "The lid is open", False),
    0x02: (BUSY, "The printer is feeding labels", True),
    0x03: (PRINTER_ERROR, "Label paper error", False),
    0x04: (NO_MEDIA, "No labels loaded", False),
    0x10: (PRINTER_ERROR, "The printer is paused", False),
    0x20: (BUSY, "The printer is busy", True),
    0x24: (NO_MEDIA, "Out of labels", False),
    0x30: (PRINTER_ERROR, "The battery is too low to print", False),
    0x80: (PRINTER_ERROR, "The printer is too hot", False),
    0x90: (PRINTER_ERROR, "The printer has no power", False),
}


def parse_p21_status_byte(value: int) -> PrinterStatus:
    known = _P21_STATUS.get(value)
    if known is None:
        # Unknown combination: read it as the bitfield the README describes.
        if value & 0x01:
            known = (COVER_OPEN, "The lid is open", False)
        elif value & 0x04:
            known = (NO_MEDIA, "No labels loaded", False)
        elif value & 0x80:
            known = (PRINTER_ERROR, "The printer is too hot", False)
        elif value & 0x22 and not value & ~0x22:
            known = (BUSY, "The printer is busy", True)
        else:
            known = (PRINTER_ERROR, f"The printer reported status 0x{value:02x}", False)
    code, message, busy = known
    return PrinterStatus(ok=code == "", code=code, message=message, busy=busy, raw=value)


def _bcd(value: int) -> int:
    hi, lo = value >> 4, value & 0x0F
    if hi > 9 or lo > 9:
        return min(value, 100)
    return hi * 10 + lo


class NelkoP21(Driver):
    """Nelko P21 pocket label printer. 96 dot head (12 mm), 203 dpi."""

    id = "nelko.p21"
    vendor = "Nelko"
    model = "P21"
    capabilities = Capabilities(
        dpi=DPI,
        print_width_dots=96,
        media=(
            # Gap 5 mm: what the official app sends for the 14x40 roll (capture).
            # The same gap for 14x50 and 14x75 is assumed, not captured.
            Media("14x40", "die_cut", 14, 40, gap_mm=5),
            Media("14x50", "die_cut", 14, 50, gap_mm=5),
            Media("14x75", "die_cut", 14, 75, gap_mm=5),
        ),
        default_media="14x40",
        color=False,
        transports=("spp", "ble"),
        formats=("raster",),
        battery=True,
    )
    # RFCOMM channel 1 is the printer's serial port (README, SDP record).
    # 512 byte writes: the largest write OpenTLP's P21 table lists.
    spp = SppParams(channel=1, chunk_size=512, pacing_s=0.0)
    # GATT service ff00: ff02 write without response (commands in), ff01 notify
    # (answers out). Server MTU 23, so 20 byte writes, paced about 12 ms apart.
    ble = BleParams(
        service_uuid=_ble_uuid(0xFF00),
        write_uuid=_ble_uuid(0xFF02),
        notify_uuid=_ble_uuid(0xFF01),
        chunk_size=20,
        pacing_s=0.012,
        write_with_response=False,
    )

    # The official app's job uses DENSITY 15 on every print (capture). DENSITY
    # is a per job line: CONFIG? does not report it, and the README lists only
    # BEEP and TIMER as settings. We send the same value the app sends.
    density = 15

    def encode(self, job: PrintJob) -> bytes:
        if job.format != "raster":
            raise ValueError(f"{self.id} prints raster only")
        media = job.media
        if media.length_mm is None:
            raise ValueError("the P21 has no continuous media")
        # Same lines, same order and same number format as the official app:
        # SIZE 14.0 mm,40.0 mm / GAP 5.0 mm,0 mm / DIRECTION 0,0 / DENSITY 15.
        t = TsplJob(mm_decimals=1)
        t.size(media.width_mm, media.length_mm)
        t.gap(media.gap_mm, 0)
        t.direction(0, 0)
        t.density(self.density)
        for page in job.pages:
            wb = math.ceil(page.width / 8)
            t.cls()
            # Mode 1 (OR) after CLS, as the official app does.
            t.bitmap(0, 0, wb, page.height, invert(page.data), mode=1)
            t.print_(job.copies)
        return t.to_bytes()

    def status_query(self) -> bytes | None:
        # ESC ! ? ; the official app follows it with CRLF (capture). The README
        # says no terminator is needed; the extra empty line is harmless.
        return b"\x1b!?\r\n"

    def parse_status(self, data: bytes) -> PrinterStatus | None:
        if not data:
            return None
        # One raw byte, no CRLF. The runtime drains old bytes before asking.
        return parse_p21_status_byte(data[0])

    def battery_query(self) -> bytes | None:
        return b"BATTERY?\r\n"

    def parse_battery(self, data: bytes) -> BatteryStatus | None:
        # "BATTERY " + level (BCD, 0x99 = 99 %) + charging flag + CRLF.
        i = data.find(b"BATTERY ")
        if i < 0:
            if len(data) > 64:
                raise ValueError("no BATTERY reply")
            return None
        body = data[i + 8 :]
        if len(body) < 2:
            return None
        return BatteryStatus(level=_bcd(body[0]), charging=body[1] == 1)


class NelkoPM220(Driver):
    """Nelko PM220 2 inch label printer, TSPL over Bluetooth Classic SPP.

    UNVERIFIED: the 384 dot (48 mm) print width. 203 dpi and a 2 inch class
    head suggest 384 dots, but no source we found states it.
    UNVERIFIED: BLE. Not offered.
    UNVERIFIED: the RFCOMM channel. Channel 1 is assumed (the usual SPP channel);
    the source used /dev/rfcomm0 without naming a channel. It can be changed in
    the printer's options.
    """

    id = "nelko.pm220"
    vendor = "Nelko"
    model = "PM220"
    min_width_mm = 23
    max_width_mm = 54
    max_length_mm = 300
    # Gap for die cut rolls. UNVERIFIED: the PM220 finds the gap itself when
    # the lid closes (alpharesearch.de); 2 mm is a common gap for these rolls.
    die_cut_gap_mm = 2
    capabilities = Capabilities(
        dpi=DPI,
        print_width_dots=384,
        media=(
            Media("50x30", "die_cut", 50, 30, gap_mm=2),
            Media("40x30", "die_cut", 40, 30, gap_mm=2),
            Media("50x25", "die_cut", 50, 25, gap_mm=2),
            Media("40x60", "die_cut", 40, 60, gap_mm=2),
            Media("50x50", "die_cut", 50, 50, gap_mm=2),
            Media("30x20", "die_cut", 30, 20, gap_mm=2),
            Media("50xC", "continuous", 50, None),
            Media("40xC", "continuous", 40, None),
        ),
        default_media="50x30",
        color=False,
        transports=("spp",),
        formats=("raster",),
        battery=False,
    )
    spp = SppParams(channel=1, chunk_size=512, pacing_s=0.0)
    ble = None

    def media_for(self, media_id: str) -> Media | None:
        known = super().media_for(media_id)
        if known is not None:
            return known
        parsed = parse_media_id(media_id)
        if parsed is None:
            return None
        width, length = parsed
        if not self.min_width_mm <= width <= self.max_width_mm:
            return None
        if length is None:
            return Media(media_id, "continuous", width, None)
        if not 5 <= length <= self.max_length_mm:
            return None
        return Media(media_id, "die_cut", width, length, gap_mm=self.die_cut_gap_mm)

    def encode(self, job: PrintJob) -> bytes:
        if job.format != "raster":
            raise ValueError(f"{self.id} prints raster only")
        media = job.media
        # Integer mm like the alpharesearch.de example ("SIZE 40 mm,30 mm").
        # No DENSITY and no DIRECTION: that example printed without them, so the
        # printer keeps its own darkness and direction. SIZE and GAP are sent
        # for every page so a continuous job never leaves the printer in
        # continuous mode for the next die cut job.
        t = TsplJob(mm_decimals=0)
        for page in job.pages:
            wb = math.ceil(page.width / 8)
            if media.length_mm is None:
                length = round(page.height * 25.4 / DPI, 1)
                t.size(media.width_mm, length)
                t.gap(0, 0)
            else:
                t.size(media.width_mm, media.length_mm)
                t.gap(media.gap_mm, 0)
            t.cls()
            t.bitmap(0, 0, wb, page.height, invert(page.data), mode=0)
            t.print_(job.copies)
        return t.to_bytes()


P21 = register(NelkoP21())
PM220 = register(NelkoPM220())

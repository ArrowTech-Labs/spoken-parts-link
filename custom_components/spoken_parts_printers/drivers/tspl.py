"""A small TSPL2 encoder (the subset the Nelko printers use).

Commands are ASCII lines ending in CRLF. BITMAP is the one exception: its ASCII
header ends with a comma and is followed directly by the raw bitmap bytes, then
CRLF. Sources and what each one verifies are listed in PROTOCOL.md.

Polarity: the contract sends bit 1 = black. TSPL BITMAP data uses the opposite
convention, a 0 bit burns a dot and a 1 bit is white. We rely on:
  * merlinschumacher/nelko-p21-print README, "Image format": "A 0 bit prints
    black, a 1 bit is white" (Nelko P21).
  * The packet capture in that repository (nelko_bt_capture.pcapng, a print of
    the mostly white default template by the official app): the BITMAP data is
    almost all 0xFF bytes, which only makes sense if 1 = white.
  * The TSC TSPL2 programming manual convention for BITMAP (0 = print dot),
    which the PM220 is assumed to share (unverified on the PM220).
So every raster byte is inverted before it is sent.
"""

from __future__ import annotations

_INVERT = bytes(255 - i for i in range(256))


def invert(data: bytes) -> bytes:
    """Contract polarity (1 = black) to TSPL BITMAP polarity (0 = black)."""
    return data.translate(_INVERT)


def fmt_mm(value: float, decimals: int = 0) -> str:
    if decimals <= 0:
        if float(value) != int(value):
            # Never round a real size away; fall back to one decimal.
            return f"{value:.1f}"
        return str(int(value))
    return f"{value:.{decimals}f}"


class TsplJob:
    """Builds the bytes of one TSPL job."""

    def __init__(self, mm_decimals: int = 0) -> None:
        self._buf = bytearray()
        self._mm_decimals = mm_decimals

    def _mm(self, v: float) -> str:
        return fmt_mm(v, self._mm_decimals)

    def command(self, text: str) -> TsplJob:
        if "\r" in text or "\n" in text:
            raise ValueError("a TSPL command must be one line")
        self._buf += text.encode("ascii") + b"\r\n"
        return self

    def raw(self, data: bytes) -> TsplJob:
        self._buf += data
        return self

    def size(self, width_mm: float, length_mm: float) -> TsplJob:
        return self.command(f"SIZE {self._mm(width_mm)} mm,{self._mm(length_mm)} mm")

    def gap(self, gap_mm: float, offset_mm: float = 0) -> TsplJob:
        # The offset is always written as a whole number, as the official
        # P21 app does ("GAP 5.0 mm,0 mm").
        return self.command(f"GAP {self._mm(gap_mm)} mm,{fmt_mm(offset_mm)} mm")

    def density(self, level: int) -> TsplJob:
        if not 0 <= level <= 15:
            raise ValueError("density must be 0 to 15")
        return self.command(f"DENSITY {level}")

    def direction(self, direction: int, mirror: int = 0) -> TsplJob:
        if direction not in (0, 1) or mirror not in (0, 1):
            raise ValueError("direction and mirror must be 0 or 1")
        return self.command(f"DIRECTION {direction},{mirror}")

    def cls(self) -> TsplJob:
        return self.command("CLS")

    def bitmap(
        self, x: int, y: int, width_bytes: int, height: int, data: bytes, mode: int = 0
    ) -> TsplJob:
        """data must already be in TSPL polarity (use invert())."""
        if width_bytes <= 0 or height <= 0:
            raise ValueError("bitmap must not be empty")
        if len(data) != width_bytes * height:
            raise ValueError(
                f"bitmap data is {len(data)} bytes, want {width_bytes * height}"
            )
        if mode not in (0, 1, 2):
            raise ValueError("mode must be 0 (overwrite), 1 (or) or 2 (xor)")
        self._buf += f"BITMAP {x},{y},{width_bytes},{height},{mode},".encode("ascii")
        self._buf += data
        self._buf += b"\r\n"
        return self

    def print_(self, copies: int = 1) -> TsplJob:
        if copies < 1:
            raise ValueError("copies must be at least 1")
        return self.command(f"PRINT {copies}")

    def to_bytes(self) -> bytes:
        return bytes(self._buf)

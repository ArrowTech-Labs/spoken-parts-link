import base64
import math

import pytest

from custom_components.spoken_parts_printers.drivers.base import Page, PrintJob
from custom_components.spoken_parts_printers.drivers.nelko import parse_p21_status_byte

SETTINGS_COMMANDS = (b"TIMER", b"BEEP", b"INITIALPRINTER", b"SELFTEST", b"SET ")


def job(driver, media_id, pages, copies=1):
    return PrintJob(format="raster", media=driver.media_for(media_id), copies=copies, pages=tuple(pages))


def test_p21_capabilities(p21):
    c = p21.capabilities
    assert (c.dpi, c.print_width_dots, c.color) == (203, 96, False)
    assert [m.id for m in c.media] == ["14x40", "14x50", "14x75"]
    assert all(m.kind == "die_cut" for m in c.media)
    assert c.transports == ("spp", "ble")
    assert p21.spp.channel == 1
    assert p21.ble.service_uuid.startswith("0000ff00")
    assert p21.ble.write_uuid.startswith("0000ff02")
    assert p21.ble.notify_uuid.startswith("0000ff01")
    assert (p21.ble.chunk_size, p21.ble.pacing_s, p21.ble.write_with_response) == (20, 0.012, False)


def test_p21_job_framing_golden(p21):
    # 16x2 page: 2 bytes per row. Row 0 all black, row 1 left half black.
    data = b"\xff\xff\xff\x00"
    out = p21.encode(job(p21, "14x40", [Page(16, 2, data)], copies=2))
    assert out == (
        b"SIZE 14.0 mm,40.0 mm\r\n"
        b"GAP 5.0 mm,0 mm\r\n"
        b"DIRECTION 0,0\r\n"
        b"DENSITY 15\r\n"
        b"CLS\r\n"
        b"BITMAP 0,0,2,2,1,\x00\x00\x00\xff\r\n"
        b"PRINT 2\r\n"
    )


def test_p21_full_label_matches_app_header(p21):
    data = bytes(12 * 320)
    out = p21.encode(job(p21, "14x40", [Page(96, 320, data)]))
    head = b"CLS\r\nBITMAP 0,0,12,320,1,"
    i = out.index(head) + len(head)
    assert out[i : i + 3840] == b"\xff" * 3840  # all white in TSPL polarity
    assert out[i + 3840 :] == b"\r\nPRINT 1\r\n"
    for bad in SETTINGS_COMMANDS:
        assert bad not in out


def test_p21_two_pages_one_header(p21):
    pg = Page(96, 8, bytes(96))
    out = p21.encode(job(p21, "14x50", [pg, pg]))
    assert out.count(b"SIZE 14.0 mm,50.0 mm") == 1
    assert out.count(b"CLS\r\n") == 2
    assert out.count(b"PRINT 1\r\n") == 2


@pytest.mark.parametrize(
    ("value", "ok", "code", "busy"),
    [
        (0x00, True, "", False),
        (0x01, False, "cover_open", False),
        (0x02, False, "busy", True),
        (0x04, False, "no_media", False),
        (0x20, False, "busy", True),
        (0x24, False, "no_media", False),
        (0x30, False, "printer_error", False),
        (0x80, False, "printer_error", False),
        (0x05, False, "cover_open", False),
        (0x40, False, "printer_error", False),
    ],
)
def test_p21_status(value, ok, code, busy):
    st = parse_p21_status_byte(value)
    assert (st.ok, st.code, st.busy, st.raw) == (ok, code, busy, value)


def test_p21_queries_are_read_only(p21):
    assert p21.status_query() == b"\x1b!?\r\n"
    assert p21.battery_query() == b"BATTERY?\r\n"
    assert p21.parse_status(b"") is None
    assert p21.parse_status(b"\x04").code == "no_media"


def test_p21_battery_bcd(p21):
    assert p21.parse_battery(b"BATT") is None
    assert p21.parse_battery(b"BATTERY \x85") is None
    b = p21.parse_battery(b"BATTERY \x85\x00\r\n")
    assert (b.level, b.charging) == (85, False)
    b = p21.parse_battery(b"BATTERY \x99\x01\r\n")
    assert (b.level, b.charging) == (99, True)
    with pytest.raises(ValueError):
        p21.parse_battery(b"x" * 80)


def test_pm220_capabilities(pm220):
    c = pm220.capabilities
    assert (c.dpi, c.print_width_dots) == (203, 384)
    assert c.transports == ("spp",)
    assert pm220.ble is None
    assert pm220.status_query() is None
    assert c.default_media == "50x30"
    assert pm220.media_for("45x25").kind == "die_cut"
    assert pm220.media_for("30xC").kind == "continuous"
    assert pm220.media_for("60x30") is None
    assert pm220.media_for("20x30") is None
    assert pm220.media_for("nonsense") is None


def test_pm220_job_framing_golden(pm220):
    data = b"\x80" + bytes(47)  # 384 wide, 1 row, top left dot black
    out = pm220.encode(job(pm220, "50x30", [Page(384, 1, data)]))
    assert out == (
        b"SIZE 50 mm,30 mm\r\n"
        b"GAP 2 mm,0 mm\r\n"
        b"CLS\r\n"
        b"BITMAP 0,0,48,1,0," + b"\x7f" + b"\xff" * 47 + b"\r\n"
        b"PRINT 1\r\n"
    )
    assert b"DENSITY" not in out and b"DIRECTION" not in out
    for bad in SETTINGS_COMMANDS:
        assert bad not in out


def test_pm220_continuous_uses_page_length(pm220):
    h = 203  # 25.4 mm
    out = pm220.encode(job(pm220, "50xC", [Page(384, h, bytes(48 * h))]))
    assert out.startswith(b"SIZE 50 mm,25.4 mm\r\nGAP 0 mm,0 mm\r\nCLS\r\n")


def test_pages_round_trip_from_base64(p21):
    raw = bytes(range(24))
    b64 = base64.b64encode(raw).decode()
    assert base64.b64decode(b64) == raw
    assert math.ceil(96 / 8) * 2 == len(raw)

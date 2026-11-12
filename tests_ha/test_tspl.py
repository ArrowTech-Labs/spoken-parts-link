import pytest

from custom_components.spoken_parts_printers.drivers.tspl import TsplJob, fmt_mm, invert


def test_golden_bytes_tiny_bitmap():
    # Contract raster, 8x2 dots: row 0 = leftmost dot black, row 1 = rightmost dot black.
    contract = bytes([0b1000_0000, 0b0000_0001])
    out = (
        TsplJob()
        .size(14, 40)
        .gap(5, 0)
        .cls()
        .bitmap(0, 0, 1, 2, invert(contract), mode=0)
        .print_(1)
        .to_bytes()
    )
    assert out == (
        b"SIZE 14 mm,40 mm\r\n"
        b"GAP 5 mm,0 mm\r\n"
        b"CLS\r\n"
        b"BITMAP 0,0,1,2,0,\x7f\xfe\r\n"
        b"PRINT 1\r\n"
    )


def test_polarity_contract_black_becomes_tspl_zero():
    assert invert(b"\xff\x00\xf0") == b"\x00\xff\x0f"
    # Padding bits (0 in the contract, white) become 1 (white in TSPL).
    assert invert(bytes([0b1110_0000])) == bytes([0b0001_1111])


def test_mm_formatting():
    assert fmt_mm(14) == "14"
    assert fmt_mm(14, 1) == "14.0"
    assert fmt_mm(12.5) == "12.5"
    j = TsplJob(mm_decimals=1).size(14, 40).gap(5, 0).to_bytes()
    assert j == b"SIZE 14.0 mm,40.0 mm\r\nGAP 5.0 mm,0 mm\r\n"


def test_density_direction_and_guards():
    assert TsplJob().density(15).direction(0, 0).to_bytes() == b"DENSITY 15\r\nDIRECTION 0,0\r\n"
    with pytest.raises(ValueError):
        TsplJob().density(16)
    with pytest.raises(ValueError):
        TsplJob().command("CLS\r\nTIMER 0")
    with pytest.raises(ValueError):
        TsplJob().bitmap(0, 0, 2, 2, b"\x00\x00\x00")
    with pytest.raises(ValueError):
        TsplJob().print_(0)

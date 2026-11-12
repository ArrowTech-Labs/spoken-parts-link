# Printer protocol notes

Every protocol fact the drivers rely on, where it comes from, and whether it is
verified. "Verified" means a published packet capture or a first hand report of
the printer doing it. Nothing here was tested by us on real hardware yet.

The code in this folder is our own. The merlinschumacher/nelko-p21-print project
is AGPL-3.0: we read its README and looked at the bytes in its packet capture
for facts only, and did not copy or adapt any of its code.

## Sources

| Key | Source |
|-----|--------|
| MS | https://github.com/merlinschumacher/nelko-p21-print README (protocol and hardware notes for the P21 and P22) |
| CAP | `nelko_bt_capture.pcapng` in the same repository: the official app connecting and printing the default 14 x 40 mm template. We read the bytes on the wire (command lines and bitmap data). |
| OTLP | https://opentlp.github.io/table-of-hardware/nelko-p21.html (OpenTLP hardware table, P21 TSPL2 family) |
| AR | https://alpharesearch.de/?p=637 (PM220 on Linux, 2026-03-07) |
| TBC | https://github.com/realugbun/tspl-ble-cups README (TSPL over BLE for Nelko PL70e-BT and similar) |
| TSC | TSC TSPL/TSPL2 programming manual (the base command set) |

## Nelko P21 (`nelko.p21`)

| Fact | Used as | Source | Status |
|------|---------|--------|--------|
| Bluetooth Classic serial port (SPP/RFCOMM) on channel 1 is the primary channel; one connection at a time; no PIN | `spp.channel = 1` | MS | Verified |
| Raw RFCOMM socket (`AF_BLUETOOTH`, `BTPROTO_RFCOMM`) works without root and without `/dev/rfcomm0` | `transports/rfcomm.py` | MS | Verified (on a desktop Linux; not yet on HAOS) |
| BLE GATT service `ff00`: `ff02` write without response (commands in), `ff01` notify (answers out) | `ble` UUIDs | MS | Verified |
| GATT server MTU 23, so at most 20 bytes per write; 20 byte writes paced about 12 ms apart print fine without the `ff03` credit channel | `ble.chunk_size = 20`, `ble.pacing_s = 0.012` | MS | Verified |
| BLE only works while no Classic link is up; LE address equals the Classic address, and BlueZ high level APIs (bleak) may not list it | BLE marked best effort; discovery best effort | MS, OTLP (Web Bluetooth trouble) | Verified |
| Commands are ASCII lines ending in CRLF; the printer acts on `\n` | `drivers/tspl.py` | MS, OTLP | Verified |
| Print head is 96 dots (12 mm) at 203 dpi; wider bitmaps are clipped | `print_width_dots = 96` | MS, OTLP | Verified |
| Full 40 mm label prints as 320 dots | length check | MS | Verified |
| Labels 14 x 40, 14 x 50, 14 x 75 mm, die cut | media list | Product page, research notes | Verified (product) |
| The official app's job: `SIZE 14.0 mm,40.0 mm`, `GAP 5.0 mm,0 mm`, `DIRECTION 0,0`, `DENSITY 15`, `CLS`, `BITMAP 0,0,12,284,1,<data>` CRLF, `PRINT 1` | P21 job framing and number format | CAP | Verified |
| `BITMAP x,y,width_bytes,height,mode,` then raw rows, MSB first, rows padded to whole bytes; mode 1 = OR | `TsplJob.bitmap` | MS, CAP | Verified |
| BITMAP polarity: a 0 bit prints black, a 1 bit is white | every raster byte is inverted (contract bit 1 = black) | MS ("Image format"); CAP (the mostly white template's bitmap is almost all `0xFF`) | Verified |
| OTLP's table says "set bit = ink" | not used | OTLP | Conflicts with MS and CAP; we follow MS and CAP |
| `GAP 5.0 mm` for 14 x 50 and 14 x 75 rolls | media `gap_mm` | assumed from the 14 x 40 capture | Unverified |
| `DENSITY` is a per job line: `CONFIG?` does not report it and only `BEEP` and `TIMER` are listed as settings | we send `DENSITY 15` with every job, as the app does | MS, CAP | Verified that the app sends it; that it is not stored is inferred |
| `ESC ! ?` (`1B 21 3F`) answers one raw status byte; `0x00` ready | status before and after each job, and every 60 s | MS; CAP shows the app sends it (followed by CRLF) before and after printing | Verified |
| Status byte values: `0x01` lid open, `0x02` feeding, `0x03` paper error, `0x04` no paper, `0x10` paused, `0x20` busy, `0x24` out of paper, `0x30` low battery, `0x80` overheated, `0x90` no power | status mapping | MS | `0x00` and `0x04` verified on a P21; the rest come from the table only |
| `BATTERY?` answers `BATTERY ` + level (BCD, `0x99` = 99 %) + charging flag + CRLF; it reports 99 % while charging | battery sensor | MS, OTLP | Verified |
| `CONFIG?` answers DPI, versions, shutdown timer, beep | not used | MS | Verified, not needed |
| Commands that change settings: `TIMER`, `BEEP`, `INITIALPRINTER` (factory reset); `SELFTEST` prints a page | never sent | MS | Verified |
| USB CDC port answers `ERROR0` to everything | USB not offered | MS | Verified |
| OTLP lists 512 bytes as the largest write | `spp.chunk_size = 512` | OTLP | Not checked; RFCOMM itself has no such limit, so this is only a conservative chunk size |
| OTLP opens each job with `ESC ! o` ("cancel pause"); MS documents `ESC ! o` as the label (NFC) info query; CAP shows the app sending it while idle | not sent | MS, OTLP, CAP | Not needed to print; left out |

## Nelko PM220 (`nelko.pm220`)

| Fact | Used as | Source | Status |
|------|---------|--------|--------|
| Bluetooth Classic SPP, `/dev/rfcomm0`, 115200 raw, TSPL | `spp` transport | AR | Verified |
| `SIZE 40 mm,30 mm` then `PRINT 1` prints | job framing (whole mm numbers, no DENSITY, no DIRECTION) | AR | Verified |
| The RFCOMM channel | channel 1, changeable in the printer's options | AR does not name it; 1 is the usual SPP channel | Unverified |
| 203 dpi, 23 to 54 mm media, 40 mm/s | capabilities, media range | Product page | Verified (product) |
| Print width 384 dots (48 mm) | `print_width_dots = 384` | inferred from 203 dpi and the 2 inch class | Unverified |
| The printer finds the label gap itself when the lid closes | | AR | Verified |
| Gap 2 mm for die cut rolls (`GAP 2 mm,0 mm`); `GAP 0 mm,0 mm` for continuous | job framing | TSC convention; 2 mm is a guess | Unverified |
| BITMAP polarity 0 = black and mode 0 (overwrite) | job framing | TSC convention, same as the P21 | Unverified on the PM220 |
| `ESC ! ?` status | not used (no status read, so "printed" means every byte was written) | TSC convention | Unverified, so not used |
| BLE | not offered | TBC confirms TSPL over BLE only for the PL70e-BT (Feasycom module); PM220 is not listed | Unverified |
| AR sends `ESC @` before `SIZE` | not sent | AR | Not needed per the TSPL manual |

## What we never send

No settings command of any kind: no `TIMER`, `BEEP`, `INITIALPRINTER`,
`SELFTEST`, `SET ...`, or any `ESC` sequence other than the read only `ESC ! ?`.
The only lines a job contains are `SIZE`, `GAP`, `DIRECTION` (P21), `DENSITY`
(P21), `CLS`, `BITMAP` and `PRINT`. The tests check that encoded jobs contain
none of the settings commands.

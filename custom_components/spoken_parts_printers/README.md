# Spoken Parts Printers

A Home Assistant integration that prints Spoken Parts labels on Bluetooth label
printers. The Spoken Parts Link app sends each job to Home Assistant, and this
integration sends it to the printer.

Supported printers:

| Printer | Connection | Labels |
|---------|------------|--------|
| Nelko P21 | Bluetooth Classic (best), Bluetooth LE (best effort) | 14 x 40, 14 x 50, 14 x 75 mm |
| Nelko PM220 | Bluetooth Classic | 23 to 54 mm wide, labels or continuous roll |

Needs Home Assistant 2026.7 or newer with the Bluetooth integration on.

## Install

With HACS: open HACS, choose Custom repositories, add
`https://github.com/ArrowTech-Labs/spoken-parts-link` with the type
Integration, then install Spoken Parts Printers and restart Home Assistant.

Without HACS: copy the `custom_components/spoken_parts_printers` folder into
the `custom_components` folder of your Home Assistant configuration, then
restart Home Assistant.

## Add a printer

1. Turn the printer on. For Bluetooth Classic, pair it with the Home Assistant
   host first (Settings, Devices and services, Bluetooth; the P21 needs no PIN).
2. Go to Settings, Devices and services, Add integration, and pick Spoken Parts
   Printers. A P21 that is advertising over Bluetooth LE may also show up as
   discovered. Discovery is best effort: the P21 only advertises while nothing
   else is connected to it.
3. Pick the printer model, then enter its Bluetooth address (for example
   `AA:BB:CC:DD:EE:FF`), a name, the connection and the labels loaded.

Each printer gets a Status sensor (ready, printing, offline, error, waiting)
and, for printers that report it, a Battery sensor.

Options (Configure on the printer's entry): name, connection, labels loaded,
Stay connected, and the serial port channel.

* Stay connected keeps the Bluetooth Classic link open so labels print right
  away, and checks the printer every 60 seconds. It suits a printer that is
  plugged in. While it is on, the phone app cannot connect to the printer, and
  a printer on battery may not turn itself off. Turn it off for a printer
  that runs on battery.
* Change Labels loaded when you put a different roll in. A job made for other
  labels is refused rather than printed on the wrong size.

## Services

Both services answer with data and are meant for the Spoken Parts Link app.

* `spoken_parts_printers.list_printers` lists the printers and their state.
* `spoken_parts_printers.print_raster` prints a job. It answers
  `{"ok": true, "bytes": n, "pages": k}` only after every byte reached the
  printer and the printer (when it can report status) showed no error.
  Otherwise it answers `{"ok": false, "code": "...", "message": "..."}`.
  It only raises an error for a malformed call.

## Add a new printer driver

A driver is one Python module in `drivers/` that registers itself.

1. Subclass `drivers.base.Driver` and set:
   * `id` (`vendor.model`, the same id the Spoken Parts backend uses),
     `vendor`, `model`;
   * `capabilities`: a `Capabilities` with `dpi`, `print_width_dots`, `media`
     (a tuple of `Media` with kind `die_cut`, `continuous` or `shipping_4x6`
     and sizes in mm), `default_media`, `color`, `transports` (from `spp`,
     `ble`, `usb`, `ipp`), `formats` (for example `("raster",)` or
     `("zpl",)`) and `battery`;
   * `spp` (`SppParams`: RFCOMM channel, chunk size, pacing), `ble`
     (`BleParams`: service, write and notify UUIDs, chunk size, pacing), or both, for
     each Bluetooth transport it offers.
2. Implement `encode(job)` to return every byte of the job. Raster pages come
   in as 1 bit per dot, rows top to bottom, MSB first, bit 1 = black; convert
   to the printer's own polarity. Optionally implement `status_query()` and
   `parse_status()`, and `battery_query()` and `parse_battery()`. Only send
   print and read only commands, never a settings command.
3. At the bottom of the module call `register(MyDriver())`, and import the
   module in `drivers/__init__.py`.
4. Add tests in `tests_ha/`: golden bytes for a tiny job (see
   `test_nelko.py`), the status mapping, and the registry (see the fake 4x6 ZPL
   driver in `test_registry.py`). Record each protocol fact and its source in
   `PROTOCOL.md`, marked verified or unverified.

The config flow, the services and the sensors pick up a new driver with no
other change. A new transport (USB, IPP) is a new class in `transports/`
plus a branch in `build_transport` in `integration.py`.

## Tests

From the repository root:

```sh
# Printer core (drivers, transports, runtime). Needs only pytest.
python -m pytest tests_ha
# Home Assistant glue. Needs the pinned test harness.
pip install -r tests_ha/requirements-glue.txt
python -m pytest tests_ha/glue
```

If Home Assistant's test harness is installed in the same environment, run the
core tests with `-p no:homeassistant --ignore=tests_ha/glue`.

Protocol notes and sources: [PROTOCOL.md](PROTOCOL.md).

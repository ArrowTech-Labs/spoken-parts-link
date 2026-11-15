# Spoken Parts Power

A Home Assistant integration that keeps a breaker from tripping. It adds up the
power of the smart plugs on each circuit and, when the sum crosses the circuit's
shed line, turns plugs off once, in this order:

1. AMS HT heaters and dryers that are drawing power,
2. printers that are not printing,
3. printing printers, the one with the least filament on its plate first.

It counts what it turned off as zero at once and looks again only after fresh
readings arrive (at most 3 rounds per event), so stale readings cannot make it
cut more. A print is only cut on fresh readings.

The trip uses the circuit's short trip filter (2 s by default; at once over the
hard line). Before a printer is turned off, every plug on the circuit is asked
for its draw right now (a direct Zigbee read through Home Assistant), and the
printer is turned off only if the fresh sum is still over the shed line
(readings older than 5 s do not count). A heater or dryer that is in a
heating session counts at its learned draw even between its pulses, so it goes
before any printer. Heaters and dryers come back
on by themselves when there is room, then printers that were not printing. A
printer that was printing stays off until a person turns it back on from
Spoken Parts (the engine checks there is room) or with the plug's button.

Circuits (breaker amps and the shed, restore and hard lines), the rooms they
serve, and what each plug powers are set up on the Power page in Spoken Parts.
The Spoken Parts Link app passes that setup to this integration. Protection is
on as soon as a circuit with plugs is set up.

## What it switches

Only THIRDREALITY Smart Plug Gen3 plugs (model 3RSP02064Z) paired with ZHA,
only the relay of that same device, and only plugs on a circuit from Spoken
Parts. Plugs of any other make are never switched. The option "Allow switching"
(on by default) turns all switching off on this Home Assistant.

## Install

Copy this folder (`ha/custom_components/spoken_parts_power` in this repository)
to `custom_components/spoken_parts_power` in your Home Assistant configuration
folder, restart Home Assistant, then go to Settings,
Devices and services, Add integration, and pick Spoken Parts Power. (It is kept out of
this repository's `custom_components` folder on purpose: HACS installs one
integration per repository, and that one is Spoken Parts Printers.)

## Plug settings

Each plug should report on a 1 W change and come back as before after a power
cut. The action "Apply plug settings" (`spoken_parts_power.apply_plug_settings`,
admins only) sets the power rise and drop thresholds to 1 W and the power on
behavior to previous on every plug that is on a circuit. It never turns a plug
on or off. The integration reads a plug directly when it has not reported for
30 seconds.

## What it adds

- `sensor.sp_power_<circuit>_sum` (W) and `sensor.sp_power_<circuit>_state`
  per circuit, with the lines, the standby plan and the open event as
  attributes.
- `binary_sensor.sp_power_engine_ok` and `binary_sensor.sp_power_feed_ok`.
- Websocket commands for the Spoken Parts Link app (admin only):
  `spoken_parts_power/hello`, `status`, `feed`, `config`, `command`,
  `entries`, `ack`.

The engine (`engine/`) and the plug rules (`inventory.py`) do not import Home
Assistant and are tested on their own (`tests_ha/power`), including the
design's scenarios and a property test of the seven rules. The Home Assistant
glue is tested against a fake ZHA (`tests_ha/glue/test_power_glue.py`).

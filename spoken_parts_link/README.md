# Spoken Parts Link

Sends prints from Spoken Parts to your Bambu Lab printers and shows their
status in Spoken Parts.

- Finds your printers on the local network by listening only.
- Reads their status with the access codes you type here.
- Uploads the prints Spoken Parts sends to the printer's USB stick. Someone
  then taps the file on the printer screen and presses Print.
- Follows each print to the end and tells Spoken Parts how it goes.
- Prints labels on a Bluetooth label printer through Home Assistant's Spoken
  Parts Printers integration (optional).
- Keeps the access codes on this device, encrypted. They are never sent to
  Spoken Parts.
- Opens no port to the internet.

- From version 0.7, works with the Spoken Parts Power integration in Home
  Assistant, which can turn smart plugs off to keep a breaker from tripping.

The link never pauses, stops or moves a printer, and never changes its
settings. With power protection set up in Spoken Parts, the Spoken Parts Power
integration can cut the power to a printer, dryer or AMS HT plug, which stops
what it was doing.

See the Documentation tab for setup steps.

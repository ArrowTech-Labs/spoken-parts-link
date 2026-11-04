# Changelog

## 0.1.1

- The link page opens on the code field until the link is connected. The
  field is ready to type in, and the printers list is below it.
- A short hint under the code field says where to get the code in Spoken
  Parts.
- The docs explain how to turn on "Show in sidebar" and where to enter the
  code.

## 0.1.0

First test release (experimental).

- Connect to Spoken Parts with a code and a check number.
- Find Bambu Lab printers on the network by listening for their broadcasts.
- Type printer access codes on the link page. They are stored encrypted on this
  device and never sent to Spoken Parts.
- Check each printer's certificate before sending its access code. A printer
  that passed the check is remembered and never offered for manual trust. A
  printer that never passed it can be trusted by hand.
- Remove a single printer on the link page to start over with it.
- Read printer status and send it to Spoken Parts about every 30 seconds.
- If the link is removed in Spoken Parts, the link deletes its access codes and
  key.
- Status only: no printing yet.

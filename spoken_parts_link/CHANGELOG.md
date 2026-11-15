# Changelog

## 0.7.2

- Power protection (Spoken Parts Power 0.1.2): the one-minute wait before a
  printer could be turned off is gone again, as Marc decided. Instead, before
  any printer is turned off, every plug on the circuit is asked for its draw
  right now over Zigbee, and the printer is turned off only if the circuit is
  really over the line on those fresh readings. AMS HT heaters and dryers that
  are drying still count at their usual draw between heating pulses.
- The link app itself is unchanged.

## 0.7.1

- Power protection no longer turns a printer off for a short power spike.
  After two wrong cuts of an H2D on Nov 15, 2026, the Spoken Parts Power
  integration (0.1.1) changes three things:
  - Before a printer is turned off, every plug on the circuit is read again.
    Readings older than 5 seconds no longer count, and the printer stays on if
    the fresh readings are under the shed line.
  - A printer is turned off only after the circuit has been over the shed line
    for 60 seconds without a break (or at once over the hard line). AMS HT
    heaters and dryers still go after the circuit's short trip filter.
  - An AMS HT or dryer that is drying counts at its usual draw even between
    its heating pulses, so it is always turned off before any printer.
- The link app itself is unchanged.

## 0.7.0

- Power protection. Spoken Parts can now protect a breaker by turning smart
  plugs off before the breaker trips. The plugs are switched by the new
  Spoken Parts Power integration in Home Assistant (THIRDREALITY Smart Plug
  Gen3 plugs on ZHA only), on the circuits you set up on the Power page in
  Spoken Parts. When the power on a circuit gets too high it turns off, in
  this order: AMS HT heaters and dryers, then printers that are not printing,
  then the print with the least filament on its plate. Heaters, dryers and
  idle printers come back on by themselves when there is room. A printer
  that was printing stays off until a person turns it back on.
- This means Spoken Parts can now stop a print or a dry by cutting the power
  to its plug. It still never sends a printer a stop, pause or any other
  command, and never changes a printer setting.
- The link itself still switches nothing. It talks to the Spoken Parts Power
  integration over Home Assistant's own websocket (the same "Home Assistant
  API" permission, no new port), and only with that integration's own
  commands: it tells the integration what each printer is doing and what its
  print is worth, passes on the circuit setup and people's requests from
  Spoken Parts, and sends the integration's log of what it did to Spoken
  Parts. If the internet is down, the log waits on this device.

## 0.6.1

- Fixes camera pictures. In 0.6.0 and earlier, every camera picture failed
  with "cannot run ffmpeg (no such file or directory)": the app's image has no
  /tmp folder, and the link started ffmpeg in it. ffmpeg now starts in the
  image's root folder, and the image has an empty /tmp. Printers whose camera
  is on in Spoken Parts send pictures again.
- Nothing else changes. ffmpeg still only reads the camera frame on stdin and
  writes the picture to stdout, with no address, access code or environment.
  Nothing new is sent to the printers.

## 0.6.0

- Reports each AMS unit's humidity, humidity level, temperature and drying
  state (time left, and why drying can't run), and each slot's RFID tray id,
  filament preset id and how much is left, with the printer status it already
  sends. Spoken Parts uses them to track how wet each roll is (SPOK-FAIL-1).
  Older Spoken Parts versions ignore the new fields.
- Also passes on, read only, three things the AMS already reports: the dry the
  printer was asked to run (temperature, hours and material, when the firmware
  says), the unit's raw status word (its drying status bits), and the
  printer's feeder sensor word (`tray_exist_bits`: which slots have filament
  in the feeder tube). Spoken Parts uses them to tell when an AMS dry starts
  and finishes, and whether a dried roll has been fed.
- Nothing new is sent to the printers. The link still only reads their status
  and still only ever sends the status request and the print start.

## 0.5.0

- Prints labels on a Bluetooth label printer through Home Assistant. You set
  the printer up in Home Assistant with the Spoken Parts Printers integration
  (a Nelko P21 for spool labels, for example). When someone prints a label in
  Spoken Parts, the link picks up the job and hands it to that integration,
  which sends it to the printer. The link tells Spoken Parts whether it
  printed or why it did not.
- New permission: "Home Assistant API". The link needs it to hand label jobs
  to the Spoken Parts Printers integration on this device and to ask it which
  label printers are set up. The link only ever calls that integration's two
  actions (list the printers, print a label) and refuses any other. No new
  port is opened.
- Without the integration, the link page says "Install the Spoken Parts
  Printers integration in Home Assistant to print labels". Spoken Parts is
  told too.
- A label job is handed to Home Assistant once. If Home Assistant does not
  answer, the job is reported as failed and never sent again, so a label is
  never printed twice. Spoken Parts drops label jobs nobody picked up within
  3 minutes.
- The link page has a new "Label printers" section with each printer's state
  and the last label.
- Label jobs never go to a 3D printer. The link sends nothing new to your
  Bambu Lab printers for this.
- Nothing happens with label printers while Spoken Parts pauses the link.

## 0.3.1

- Phone alerts now reach a Home Assistant that serves only https on port
  8123. The link tries http first and then https on this device, and keeps
  the one that works. For this one local address the link does not check
  Home Assistant's certificate, because that certificate never names this
  device's own address. Any other address is always checked.
- New optional setting, "Home Assistant address for phone alerts"
  (`ha_url`), for a Home Assistant the link cannot find by itself. Leave it
  empty unless phone alerts do not arrive.
- The link log says once which address reached Home Assistant. It never
  shows the webhook ID.
- Built with Go 1.27.4, which fixes a security problem in Go's web code
  (GO-2026-6859).

## 0.3.0

- Watches each printer for problems, when an owner turns monitoring on in
  Spoken Parts. The rules run on the link, so they work with the internet
  down: failed and paused prints, new printer messages (any message from the
  AI camera is critical), objects skipped in a short time, no progress,
  temperatures off target, lost contact, and the finish. The same alert is
  not repeated for 30 minutes unless it gets worse.
- Camera stills from the printer's own camera, per printer, when the camera is
  turned on in Spoken Parts and LAN Only Liveview is on at the printer (Cloud
  mode stays). One every 2 minutes while printing, one with each problem and
  one at the end; none while idle. The link reads the stream itself and gives
  ffmpeg only one picture: ffmpeg never sees the access code.
- Alerts and stills go to Spoken Parts, where your team sees them on the farm
  page.
- Optional phone alerts through one Home Assistant automation (a local
  webhook). No new Home Assistant permission. The webhook ID is stored
  encrypted.
- The link page shows each printer's camera and open alerts.
- The link never pauses, stops or skips anything. People decide.

## 0.2.2

- Upload progress goes to Spoken Parts every 10 seconds instead of every 2, so
  the link stays well inside its request limit.
- If Spoken Parts did not get the link's "uploaded" or "upload failed"
  message (a lost connection), the link sends it again.

## 0.2.1

Fixes from a review before 0.2.0 was installed anywhere.

- A new upload never deletes the file of another print that is still waiting
  to be tapped, or that is printing. The printer is checked again right before
  each delete.
- A print that was downloaded but not uploaded when the link stopped is
  uploaded when it starts again, instead of waiting forever.
- Downloaded files are removed without first being overwritten, so a large
  file no longer uses its whole size in memory on a Raspberry Pi.
- When a print is closed in Spoken Parts while it is printing, its file stays
  on the printer until the next upload.
- Try again after a busy or unreachable printer works even if the print was
  sent more than 30 minutes ago.
- Leftover downloads from a crash are deleted when the link starts.
- A half uploaded file is deleted from the printer after a failed upload.

## 0.2.0

- Sends prints. When a print is sent to a printer in Spoken Parts, the link
  gets the file, checks it (its checksum, and that it is a complete sliced
  plate for this printer) and uploads it to the printer's USB stick.
- Printers stay in Bambu's Cloud mode. Someone at the printer opens Storage on
  the printer screen, taps the file Spoken Parts names and presses Print. The
  phone shows the steps, and the exact file name, once the upload is done.
- The link sees the print start on the printer and follows it to the end:
  progress, layers, errors and the finish go to Spoken Parts.
- If someone starts a different file, or the printer uses an AMS slot the plate
  should not use, Spoken Parts is told so a person can stop it. The link
  cannot pause or stop a print.
- Only one Spoken Parts file is on a printer's USB stick at a time, so nobody
  taps an old one. Files the link uploads are named `SP-` plus a short id and
  the plate. Before an upload the link deletes the files it uploaded to that
  printer earlier, and after the print it deletes the new one. It keeps a list
  of the files it uploaded, and it never deletes a file that is not on that
  list, a file someone else put on the stick, or the file of a print that is
  running. If an old file can't be deleted, Spoken Parts names it so the right
  one is tapped.
- Owners can run a dry run: the whole delivery without printing (no project
  start, ever), for testing a printer.
- If the printer has no USB stick, the link says so and waits until someone
  inserts one and taps Try again in Spoken Parts.
- The upload uses the printer's own FTPS server (port 990) with the same
  certificate check as before: the access code is only sent to a printer whose
  certificate checks out.
- The link page shows, per printer, whether a USB stick is in, how prints
  start (upload only in Cloud mode), the print it is delivering and the last
  delivery problem.
- The only commands the link can send to a printer are the status request and,
  only for printers in Developer Mode and only if auto start is turned on, a
  start for a file it uploaded. Auto start is off and is not an app option.
- Downloaded files wait in the app's private data folder and are deleted after
  the print, after a failure, or when the link is removed.

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

# Spoken Parts Link

Spoken Parts Link connects the Bambu Lab printers at your site to Spoken Parts.
It runs inside Home Assistant on a small computer at your site, such as a
Raspberry Pi.

## What it does

- It finds Bambu Lab printers on your network. It only listens. It never sends
  anything to look for printers.
- It reads each printer's status: idle or printing, percent done, layers, time
  left, errors, the filament in each AMS slot and the nozzle sizes. It uses the
  printer's access code for this.
- It tells Spoken Parts what it sees, about every 30 seconds (every 5 seconds
  while a print is on its way).
- It uploads the prints Spoken Parts sends to the printer's USB stick, and
  follows each print to the end (see "Sending prints" below).
- From version 0.3 it watches each printer for problems, when you turn
  monitoring on in Spoken Parts: failed or paused prints, printer messages
  (HMS), objects skipped in a short time, no progress, temperatures off
  target, lost contact. With the camera on, it takes a camera still every 2
  minutes while a print runs, and one with each problem (see "Watching for
  problems" below).

## What it does not do

- It does not pause, stop or move a printer, heat it, run its fans or change
  its settings. It cannot.
- It does not start prints by itself. Your printers stay in Bambu's Cloud
  mode, and in Cloud mode only Bambu's own apps or a person at the printer can
  start a print. Someone taps the file on the printer screen.
- It never sends your printer access codes to Spoken Parts. They stay on this
  device, and only go to the printer they belong to. A code you type on
  spokenparts.com is sealed to this link in your browser, so Spoken Parts
  cannot read it either (see "Type a code on spokenparts.com" below).
- It sends only two kinds of message to a printer: a request for its full
  status (called "pushall"), at most once every 5 minutes, and the file upload
  (FTPS, port 990). Camera stills are read from the printer's own camera
  stream (RTSPS, port 322); nothing is sent to the printer for them.
- It never pauses, stops or skips anything when it sees a problem. It tells
  people. Stopping a print stays with a person at the printer screen, in Bambu
  Handy or in Bambu Studio.
- It opens no port to the internet. It only makes outgoing HTTPS connections to
  spokenparts.com.

## Before you start

- Home Assistant OS. Apps need it. A Raspberry Pi 4 or 5 works well.
- Your printers and Home Assistant on the same network.
- A USB stick in each printer that should receive prints (FAT32 or exFAT).
  On the H2D, H2S and X2D the link can only upload to a USB stick.
- An owner account in Spoken Parts for this site.

## Install

1. Open Home Assistant on a computer at your site, for example
   `http://homeassistant.local:8123`.
2. Add our app store with this link:
   [Add the Spoken Parts repository](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FArrowTech-Labs%2Fspoken-parts-link).
   Or go to Settings, then Apps, then Install app. Open the menu, choose
   Repositories and add `https://github.com/ArrowTech-Labs/spoken-parts-link`.
3. Find Spoken Parts Link in the list and install it.
4. Turn on "Start on boot" and "Watchdog". Then start it.

## Open the link

1. In Home Assistant, go to Settings, then Apps (Add-ons on older Home
   Assistant), then Spoken Parts Link.
2. Turn on "Show in sidebar". The app can't turn this on for you.
3. Pick Spoken Parts in the sidebar. Or tap "Open web UI" on the app's page.

Until the link is connected, the code field is at the top of the page and
ready to type in.

## Enter your code

1. In Spoken Parts, open your building and go to the Network setup step. Get a
   code there.
2. On the link page in Home Assistant, paste or type the code in "Enter the
   code from Spoken Parts" and tap Connect. The code works for 10 minutes.
3. The link page shows a 4 digit check number. Spoken Parts shows a number too.
4. If the two numbers are the same, confirm in Spoken Parts. If they are not the
   same, do not confirm. Get a new code instead.
5. The link page then says "Connected to" your site's name.

If nobody confirms within 15 minutes, the link page says "This code timed out.
Get a new code in Spoken Parts."

## Type the printer access codes

Each printer has an 8 character access code. You find it on the printer under
Settings, then Network. On some models it is under Settings, then LAN Only.

1. On the link page, find the printer in the list. It shows the last 4
   characters of the serial number.
2. Type its code in "Access code (8 characters)" and tap Save.
3. The page says "Saved. Checking the printer...". After a few seconds the
   access code shows "Working".

The field is write only. The page never shows a saved code. To change a code,
type the new one and save it.

You can type access codes only after the link is connected to Spoken Parts.
The codes are stored encrypted. Opening them needs a secret on this device and
a key that Spoken Parts gives only to this link. See "What is stored on this
device" below for what that does and does not protect.

### Type a code on spokenparts.com

From version 0.4 you can also type a printer's access code on
spokenparts.com, for example on your phone while you stand at the printer.
The link page keeps working the same way, and you can use either one.

- Your browser seals the code to this link before it leaves your phone or
  computer. Only this link can open it. Spoken Parts passes the sealed code on
  but cannot read it, and deletes it once the link has it, or after 15
  minutes.
- A sealed code works only for the printer it was typed for and only for this
  link.
- The link then saves it exactly like a code typed on this page, and checks it
  with the printer. Spoken Parts shows "Working" or asks you to type it again.
- To seal codes, the link makes its own key for this and tells Spoken Parts
  the public half. The bottom of the link page shows its short ID, "Access
  code key" and 16 letters and numbers. Spoken Parts shows the same ID where
  you type the code. If the two ever differ, do not type the code there: use
  the link page and tell us.
- This needs Spoken Parts to support it. Until then the website sends you to
  the link page.

## Printer certificates

Before the link sends an access code, it checks the printer's security
certificate. The certificate must be signed by Bambu Lab and carry the
printer's serial number. This stops another device on your network from
pretending to be your printer.

Once a printer has passed this check, the link remembers it. If something
later answers for that printer with a certificate that does not pass, the
link does not send the access code and does not offer to trust it. The page
says "This printer's certificate changed and couldn't be checked. If you just
replaced or reset the printer, remove it here and add it again." It also warns
that something on your network answered for the printer. Real firmware updates
keep a certificate signed by Bambu Lab, so they do not cause this.

To start over with a printer, tap "Remove this printer here". This deletes its
access code and what the link remembers about it. When the printer is on again
it shows up as new, and you type its access code again.

A printer that has never passed the check (for example a very new model) shows
"We couldn't check this printer's certificate." and a "Trust this printer"
button. Only tap it for a printer you just set up and can see on your own
network. The link then remembers that exact certificate.

If the page says the certificate couldn't be checked because this device's
clock may be wrong, check the date and time in Home Assistant. The link tries
again by itself.

The link also ignores printer announcements that come from outside your local
network, or that name an address other than the sender's own.

## Sending prints

1. A worker scans the printer's Ready code in Spoken Parts. Spoken Parts picks
   the next print and the link starts uploading it right away.
2. While it uploads, the phone shows the changes the print needs, one at a
   time: the filament for each AMS slot (and which nozzle it feeds on an H2D),
   a nozzle swap, the plate. The link checks the filament and nozzle steps
   against what the printer reports.
3. When the upload is done and every step checks out, the phone says which
   file to tap: on the printer screen open Storage, tap that file (its name
   starts with SP and the printer number), check the filament list matches what
   the phone shows, and press Print.
4. The link sees the print start and follows it to the end. When it ends, the
   link deletes the file it uploaded.

Only one Spoken Parts file is on the USB stick at a time. Its name starts with
`SP-`. Before each upload the link deletes the Spoken Parts files it uploaded
to that printer before. It never deletes other files on the stick. To print
something again, put it in the queue in Spoken Parts, not from the printer's
storage list.

If the printer has no USB stick, Spoken Parts says "Insert a USB stick" for
that printer. Insert one and tap Try again on the phone.

If someone starts a different file, or the printer uses an AMS slot this print
should not use, Spoken Parts tells the worker and the owner. Stop the print on
the printer screen if it is wrong. The link can't stop it.

The link page shows, for each printer, whether a USB stick is in, how prints
start ("No, upload only" in Cloud mode), the print it is delivering, and the
last delivery problem.

Files wait in the app's private data folder while they are delivered. They are
deleted after the print, after a failure, or when the link is removed.

## Watching for problems

Monitoring is off until an owner turns it on in Spoken Parts (the building's
monitor settings). The rules run here on the link, so they keep working when
the internet is down; alerts wait and go to Spoken Parts when it is back.

What raises an alert:

| What | How serious |
|---|---|
| A print failed | Critical |
| A print paused | Warning, critical with an error or printer message |
| A print finished (the plate needs clearing) | Info |
| A new printer message (HMS) | Warning. Critical for fatal ones and for any message from the AI camera (codes starting 0C00) |
| A printer error | Critical |
| The printer's own AI camera saw a problem | Critical |
| Objects skipped during a print | Warning. Critical when 2 or more are skipped within 10 minutes |
| No progress for 20 minutes | Critical |
| Progress or time left jumps | Warning |
| A nozzle or the bed is off target for 4 checks in a row | Critical |
| The link lost contact with the printer for 4 checks | Critical during a print, else warning |
| The camera failed 3 times in a row | Warning |

The same alert is not sent again for 30 minutes unless it gets more serious.
The numbers are settings in Spoken Parts. The link checks every 30 seconds.

### Camera stills

Turn the camera on per printer in Spoken Parts. The printer must have "LAN
Only Liveview" on (on the printer: Settings, LAN Only, LAN Only Liveview).
This does not turn on LAN Only mode: your printer stays in Cloud mode, and
Developer Mode stays off.

- Stills are taken only while a print runs (every 2 minutes by default), and
  one with each problem and at the end of a print. None while a printer is
  idle. Cameras can see people in the room.
- The link opens the camera stream itself, with the same certificate check as
  for the printer status, and logs in with the access code. It takes one
  picture and gives only that picture to ffmpeg (a video tool in this app) to
  turn it into a JPEG. ffmpeg never sees the access code or the camera address.
- The last 60 stills per printer are kept here in `/data/frames`. Each still
  also goes to Spoken Parts, where your team sees it on the farm page. Spoken
  Parts keeps stills for 7 days, and stills from a problem or a failed print
  for 90 days (settings in Spoken Parts).
- Spoken Parts can also check the stills with image AI, when the owner turns
  it on. That runs on the Spoken Parts server, with keys kept there; this app
  never has or needs an AI key. A quick check looks at each still. If it sees
  a failure, a smarter check looks at the same still, and you are told to stop
  the print only when both see it. It costs at most $1 a day.

## Phone alerts through Home Assistant

Optional. The link can also send problems to your phone through one Home
Assistant automation. This works when the internet is down. The app gets no
other access to Home Assistant: it can only trigger this one automation.

1. In Home Assistant, go to Settings, Automations, and create an automation.
2. Add a trigger: Webhook. Pick a long random webhook ID (at least 24
   characters) and keep "Only accessible from the local network" on.
3. Add an action that notifies your phone. In YAML it looks like this, for
   the Home Assistant mobile app on the phone named "FBI Van 183". Replace the
   webhook ID with your own, and check your phone's notify service name under
   Developer Tools, Actions (search "mobile_app"); use that name if it is
   different:

```yaml
alias: Spoken Parts printer alerts
triggers:
  - trigger: webhook
    webhook_id: "REPLACE-WITH-A-LONG-RANDOM-ID"
    allowed_methods: [POST]
    local_only: true
actions:
  - action: notify.mobile_app_fbi_van_183
    data:
      title: "{{ trigger.json.title }}"
      message: "{{ trigger.json.message }}"
      data:
        image: "{{ trigger.json.image_url | default('') }}"
mode: queued
```

4. On the link page, under "Phone alerts through Home Assistant", paste the
   webhook ID (or the whole webhook address) and choose what goes to the
   phone: critical problems only, warnings and critical problems (the
   default), or everything.
5. Tap "Send a test".

The link finds Home Assistant on this device by itself, over http or https.
If your Home Assistant uses https, the link does not check its certificate for
this one local address, because that certificate never names it. If the test
says Home Assistant could not be reached, set "Home Assistant address for
phone alerts" on the app's Configuration tab to the address you use, for
example `https://192.168.1.10:8123` (address and port only). For an address
other than this device's own, the certificate must be valid.

The webhook ID is kept encrypted, like the access codes, and is never shown
again or sent to Spoken Parts. The picture in the notification is a link to
the still in Spoken Parts that works for 24 hours.

## Why the app uses the host network

Bambu Lab printers announce themselves with a broadcast on UDP port 2021. An
app only hears those broadcasts on the host network. The link page itself only
answers Home Assistant's own ingress proxy, so other devices on your network
cannot open it.

## What is stored on this device

The app keeps these files in its private data folder. Only this app can read
them, but they are not protected from someone who has the device's storage or
a backup.

- **Printer access codes: encrypted.** They are encrypted with a key made from
  a secret on this device and a key that Spoken Parts gives only to this link.
  The combined key is kept in memory only.
- **The link key and the local secret: not encrypted.** They are plain files.
  The link key is how Spoken Parts knows this is your link. Encrypting them
  would not help, because the key to open them would have to sit in the same
  folder and in the same backups.
- **The Home Assistant webhook ID: encrypted,** the same way as the access
  codes, in its own file.
- **The key for codes typed on spokenparts.com: encrypted,** the same way as
  the access codes, in its own file. It is deleted with the access codes.
- **Monitor state and camera stills: not encrypted.** What each printer last
  reported, which alerts were sent, and the last 60 stills per printer.

## Backups

Home Assistant backups include this app's data: the link key, the local secret
and the encrypted access codes.

This means someone who has a backup file and its password can act as this link
and open the access codes, until you remove the link in Spoken Parts. So:

1. Keep Home Assistant backup encryption on (it is on by default) and keep the
   backup password safe.
2. If you lose a backup, or think someone else has it, remove the link in
   Spoken Parts, on your site's Network setup page. After that Spoken Parts
   never gives out its key again, and the codes in that backup can no longer be
   opened.
3. Then change the access code on each printer. Connect again and type the new
   codes.

If you restore a backup on a new device, the link keeps working with the same
connection, as long as it was not removed in Spoken Parts. Turn the old device
off first. If both run at the same time, they share one link and get in each
other's way. If that happens, remove the link on one of them ("Remove this
link here") and in Spoken Parts, then connect that one again with a new code.

## Remove the link

1. On the link page, tap "Remove this link here" and confirm. This deletes the
   access codes, the key for codes typed on spokenparts.com and the link key on
   this device.
2. Also remove the link in Spoken Parts, on your site's Network setup page.
3. You can then uninstall the app in Home Assistant under Settings, then Apps.

If the link is removed in Spoken Parts first, the link notices on its next
check in. It deletes the access codes and its key by itself. The page then
says "This link was removed in Spoken Parts."

## Options

- `log_level`: `debug`, `info` (the default), `warning` or `error`. Access
  codes and keys are never written to the log.

## Check the app image

This is optional. Every release image is signed by our GitHub release workflow.
To check it yourself, install cosign and the GitHub CLI, then run:

```
cosign verify ghcr.io/arrowtech-labs/spoken-parts-link:0.2.2 --certificate-identity-regexp '^https://github.com/ArrowTech-Labs/spoken-parts-link/.github/workflows/release.yml@refs/tags/v' --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

```
gh attestation verify oci://ghcr.io/arrowtech-labs/spoken-parts-link:0.2.2 --owner ArrowTech-Labs
```

Both commands should say the check passed. Home Assistant itself does not check
image signatures yet, so this is the way to check.

## Help

For help, contact Spoken Parts support at https://spokenparts.com. To report a
security problem, see SECURITY.md in the repository.

## License

PolyForm Shield 1.0.0. It is free to use, also for business, including print
farms. You may not resell it or use it to offer a competing product. See the
LICENSE file for the full terms.

# Spoken Parts Link

Spoken Parts Link connects the Bambu Lab printers at your site to Spoken Parts.
It runs inside Home Assistant on a small computer at your site, such as a
Raspberry Pi.

## What it does in version 0.1

- It finds Bambu Lab printers on your network. It only listens. It never sends
  anything to look for printers.
- It reads each printer's status: idle or printing, percent done and time left.
  It uses the printer's access code for this.
- It tells Spoken Parts what it sees, about every 30 seconds.

## What it does not do

- It does not start, pause or stop prints. Version 0.1 only reads status.
- It never sends your printer access codes to Spoken Parts. They stay on this
  device.
- It sends only one kind of message to a printer: a request for its full status
  (called "pushall"). It sends it once after it connects, and at most once every
  5 minutes per printer.
- It opens no port to the internet. It only makes outgoing HTTPS connections to
  spokenparts.com.

## Before you start

- Home Assistant OS. Apps need it. A Raspberry Pi 4 or 5 works well.
- Your printers and Home Assistant on the same network.
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
   access codes and the link key on this device.
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
cosign verify ghcr.io/arrowtech-labs/spoken-parts-link:0.1.1 --certificate-identity-regexp '^https://github.com/ArrowTech-Labs/spoken-parts-link/.github/workflows/release.yml@refs/tags/v' --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

```
gh attestation verify oci://ghcr.io/arrowtech-labs/spoken-parts-link:0.1.1 --owner ArrowTech-Labs
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

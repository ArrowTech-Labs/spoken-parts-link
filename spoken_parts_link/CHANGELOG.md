# Changelog

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

# Threat model: Spoken Parts Link

This covers the link program in this repository (version 0.5: status,
uploading the prints Spoken Parts sends, watching printers with camera
stills, and handing label jobs to Home Assistant for Bluetooth label
printers), its
pairing with Spoken Parts, the data it keeps on the device, and how release
images are built. It is written for customers, auditors and contributors.

## 1. What the link protects

1. **Printer access codes.** An 8 character code gives full local control of a
   Bambu Lab printer on the LAN. This is the most valuable thing the link holds.
2. **The link key.** An Ed25519 private key that proves "this is the link for
   site X" to Spoken Parts. Stored as a plain file (see 4.5).
3. **The local secret.** 32 random bytes that, together with a key from Spoken
   Parts, open the access code file. Stored as a plain file (see 4.5).
4. **Printer control.** From version 0.2 the link puts files on a printer's
   storage, and a person starts them on the printer screen. A wrong file, a
   file for another printer, or the wrong filament slots could waste material
   or damage a print. The link still cannot pause, stop, move, heat or
   configure a printer. Printers stay in Bambu's Cloud mode, where only Bambu's
   own apps or a person at the printer can start a print.
5. **Status data.** What each printer is doing. Low sensitivity, but it shows
   business volume.
6. **The release pipeline.** Whatever it ships runs on customer networks.
7. **Print files.** The sliced files Spoken Parts sends. They are customers'
   parts, so they are kept only while they are delivered.
8. **The Supervisor token (0.5).** `SUPERVISOR_TOKEN`, which the Supervisor
   gives the app for its `homeassistant_api` permission. It can call Home
   Assistant's whole REST API (any action, any state), so it matters as much
   as the Home Assistant host itself.

## 2. Who might attack

| Actor | What they can do |
|---|---|
| Internet attacker | Reach spokenparts.com, guess pairing codes, replay requests |
| Phisher | Talk an owner into reading out a pairing code |
| LAN attacker | A device or guest on the site network: sniff, fake printer broadcasts, try to open the link page |
| Other Home Assistant apps or users | Code on the same host; a person with Home Assistant admin access |
| Thief | Takes the device, its storage, or a backup file |
| Supply chain attacker | Takes over a maintainer account, a GitHub Action, a base image or the registry |
| Compromised Spoken Parts backend | Someone in control of spokenparts.com |

## 3. Trust boundaries

- **B1** the internet, between the link and spokenparts.com.
- **B2** the site LAN, between the link and the printers.
- **B3** the Home Assistant host: Supervisor, ingress proxy, other apps, backups.
- **B4** GitHub and the container registry, which deliver the code and image.

## 4. Threats and mitigations

### 4.1 Pairing (B1)

| Threat | Mitigation |
|---|---|
| Guessing a live pairing code | 8 character codes, short lifetime, single use, rate limits per IP and globally on the server. A correct guess only creates a pending link that the owner must approve. |
| Phished code | The owner approves the link in Spoken Parts and sees a 4 digit check number derived from the link's public key. The link shows the same number. A mismatch means do not approve. The link also computes the number itself and stops if the server's number differs. |
| Code in logs | The code typed on the page is registered with the log redactor before anything is logged. |

### 4.2 Requests to Spoken Parts (B1)

| Threat | Mitigation |
|---|---|
| Stolen bearer token | There is none. Every request is signed with the link's Ed25519 key over method, path, timestamp, nonce and body hash. |
| Replay or tampering | Timestamp window and single use nonces on the server; the signature covers the body. Redirects are never followed. HTTPS only (TLS 1.2 or newer). |
| Removing a link | Removing it in Spoken Parts makes the next request fail with `link_revoked`. Only a correctly signed request can get that answer. The link then deletes its access codes, key and local secret. |
| Access codes sent to the cloud | The sync request has only an access code *state* (`missing`, `ok`, `wrong`). An end to end test checks that a stored code never appears in any request, header, log line or plain file. |

### 4.3 The local network (B2)

| Threat | Mitigation |
|---|---|
| Fake printer broadcast | Printers are keyed by serial. Before sending an access code the link requires the printer's TLS certificate to chain to a bundled Bambu Lab CA and to name the serial. A fake printer fails this, so it never receives the code. |
| Fake printer for a known serial | The link remembers, per serial, the certificate that last passed the CA check. For such a serial it never offers manual trust: a certificate that fails is shown as changed, nothing is sent, the session goes back to the last good address and keeps checking. The owner's only way back is to remove the printer and add it again. |
| Unknown or new printer CA | Only for a serial that never passed the check: the link shows "We couldn't check this printer's certificate." and sends nothing. The owner can trust that exact certificate by hand. If it later changes, it is treated as above. A certificate that fails only on its dates (a wrong clock on this device) is retried, never offered for trust. |
| Sniffing | MQTT and FTPS run over TLS 1.2. The access code is only sent inside those sessions. |
| Fake printer for uploads | The FTPS upload (port 990) checks the printer's certificate the same way as MQTT (Bambu CA and the serial, or the certificate the MQTT session already checked) before the access code is sent. The data connection resumes the checked control session. The address from the server's PASV answer is ignored: data goes to the printer's own address only. |
| LAN device opens the link page | The page binds to the Supervisor interface (172.30.32.1) and refuses any source other than the ingress proxy (172.30.32.2). |
| Discovery abuse | The link only listens. Packets over 2 KiB are dropped and fields are validated. The announced address must be the sender's own, on a private LAN range (RFC 1918 or IPv6 ULA). Each source and all sources together are rate limited, and a full source table never resets a limited source. A printer's address changes at most once a minute, never while a checked session is up, and an address that flips back is held for 5 minutes. Unconfigured printers are forgotten after a day, and the list is capped. |
| Printer overload | One session per printer, and one status request at connect, at most every 5 minutes. A wrong access code is not retried until a new one is saved. |

### 4.4 The Home Assistant host (B3)

| Threat | Mitigation |
|---|---|
| Other apps read `/data` | Supervisor keeps each app's data separate. The app asks for no Supervisor API access, no privileged capabilities and no device access. Its one Home Assistant permission, `homeassistant_api` (0.5), is for label printers (section 4.10). A custom AppArmor profile limits files, capabilities and sockets. |
| Process compromise | The program starts as root only to fix ownership of its folders, then switches to uid 65532 for good before it opens any socket or reads any network input. The image has no shell or other programs. |
| Home Assistant admin users | They can open the link page. Access code fields are write only: the page never shows a saved code. |
| Cross site requests | Every form needs a CSRF token. The page has no scripts, a strict content security policy and only same origin framing. |
| Codes in add-on options | Access codes are never add-on options, because options are stored in plain text and shown on the Configuration tab. |
| Logs | One logger for the whole program. Every line passes a redactor that removes access codes, the pairing code, the data key and key material. |

### 4.5 Stolen device or disk

The device holds the link key, the local secret and the encrypted access code
file. The data partition of Home Assistant OS is not encrypted. Only the
access codes are encrypted. The link key and the local secret are plain files
readable only by the app: any key that wrapped them would have to live in the
same folder and the same backups, so wrapping them would protect nothing.

- The access code file cannot be opened from the disk alone. Its key is
  HKDF-SHA256 of the local secret and a 32 byte data key that Spoken Parts gives
  only to this link, on request, after a signed call. The derived key lives
  only in memory.
- A thief who keeps the device online can act as that site's link until it is
  removed in Spoken Parts, and can get the data key in that time.
- **Response:** remove the link in Spoken Parts. Spoken Parts then never gives
  out the data key again. Change the printer access codes.

### 4.6 Backups (owner decision)

The link key, the local secret and the encrypted access codes are **included**
in Home Assistant backups. This keeps restores simple: a restored link keeps
working without pairing again.

The tradeoff: anyone who has a backup file and its password can recover the
access codes. They would need the data key, and Spoken Parts gives that key to
the link key, which is also in the backup. So the encryption alone does not
protect a leaked backup. The real protections are:

1. Home Assistant backup encryption (on by default) and a safe backup password.
2. Removing the link in Spoken Parts after a backup is lost. Revocation stops
   the data key, so the codes in that backup can no longer be opened.
3. Changing printer access codes after a lost backup.

A second effect: restoring a backup on a second device while the first still
runs gives two devices that both act as the same link. Spoken Parts accepts
both, and each one's report marks the other's printers offline. The owner
should turn the old device off before restoring, or remove the link on one
device and pair it again.

### 4.7 Compromised Spoken Parts backend

A compromised backend can see status data and can stop links (by revoking them).
It cannot read access codes: they are never sent. From version 0.2 it can put a
file on a printer's USB stick, with a name the link chooses the shape of
(`SP<printer>-...gcode.3mf`). It cannot start that file: printers stay in Cloud
mode, and a person must tap it on the printer screen. Auto start exists only
for Developer Mode printers, behind an option that is off and is not an app
option. It cannot send any other command: the MQTT client has no general
publish function, the allowed commands are a constant
(`mqtt.AllowedCommands`: pushall and project_file), and a repository test fails
if any other printer command appears in the code. It cannot delete other files
on the printer: the link deletes only files it uploaded itself.

### 4.8 Sending prints (B1, B2)

| Threat | Mitigation |
|---|---|
| Forged send | Sends are only read from Spoken Parts over the signed link API (outgoing HTTPS, no new port). Spoken Parts only lists sends for printers of the link's own site that the link itself reported, and only to the paired link key. |
| Stolen file link | Each file is fetched once with a one-time token that lasts 30 minutes. The token goes in a header, never in a URL, so it is not in proxy or server logs. The link registers it with the log redactor. |
| File tampering | The link checks the file's SHA-256 against the send and the server's header, then that it is a zip with the plate's G-code and that the G-code matches the md5 Studio wrote. A mismatch fails the send; nothing is uploaded. Files over 512 MB are refused. |
| Wrong printer | A send names the printer's serial and model. The link uploads only to that serial, only if the model matches what the printer announces, and only over a session whose certificate names that serial. |
| Wrong file started | The link tells Spoken Parts which file started. A different file is reported as the wrong file, and the worker and owner are told. |
| Wrong filament slots | Spoken Parts plans each filament's AMS slot from the link's own AMS report, and the phone checks each swap against it. After the start, the link compares the printer's mapping in use with the plan and raises an alert on a mismatch. The link cannot stop the print; a person stops it on the printer screen. |
| Double start | In Cloud mode the link starts nothing. With auto start on, a start is recorded on disk before it is sent and is sent at most once per send, and once per MQTT session, so a restart never starts a print twice. |
| Files left behind | Files wait in `/data/spool` (0600) and are deleted after the print, after a failure, and when the link is removed. On the printer the link deletes its own file after the print, and before each upload it deletes the earlier files it uploaded there, so only one Spoken Parts file is on the stick. |
| Deleting someone's files | The link deletes only reserved `SP-<id>-<plate>.gcode.3mf` names that are in its own manifest (`/data/state/storage.json`) with the recorded size, and never the running print's file. Anything else on the stick is never touched. |
| Printer busy or in trouble | The link uploads only to a printer that is not printing, has storage and reports no serious error. |

### 4.9 Monitoring and camera stills (B1, B2, B3)

| Threat | Mitigation |
|---|---|
| Access code leaks through the camera tool | The link does the RTSPS session itself (TLS 1.2, the same Bambu CA and serial check as MQTT, Digest or Basic login in process). ffmpeg gets only one H.264 key frame on stdin and writes the JPEG to stdout. Its argument list is fixed, its environment is empty, and it never sees a URL or the code. Tests check the argv and env. |
| A fake camera on the LAN | The camera connection must pass the certificate check before the code is sent, and is only used while the checked MQTT session to the same address is up. |
| Malicious camera data | The RTSP client caps header, body, packet, NAL and frame sizes and the whole grab has a 15 second limit; ffmpeg has its own 15 second limit and its output is capped at 2 MB. The depacketizer is fuzz tested. ffmpeg runs under the app's AppArmor profile (`ix`), which allows no other program and no shell is in the image. |
| Privacy (people in the room) | Stills only while printing, with problems and at the end; none while idle. Monitoring and the camera are off until an owner turns them on in Spoken Parts. Metadata (EXIF and comments) is stripped. |
| Phone alert webhook | The webhook id is a secret: stored in its own AES-256-GCM file, keyed like the access codes, registered with the log redactor, never shown again or sent to Spoken Parts. The link posts only to Home Assistant on this host (or the address in the owner's `ha_url` option), never through a proxy. On https to 127.0.0.0/8 or ::1 the certificate is not checked, because Home Assistant's certificate never names that address; the client that skips the check dials only loopback addresses. Every other https address is checked against the system roots. Errors and logs name only the scheme, host and port. The webhook needs no Home Assistant permission; the `homeassistant_api` permission added in 0.5 is used only for label printers (section 4.10). |
| Alerts that act on printers | None. The monitor never sends a printer command; the MQTT allow list is unchanged (pushall, and project_file behind the off flag). |
| Signed still links | Stills attached to alerts can come with a 24 hour signed link for the phone notification. The link registers it with the log redactor. |

### 4.10 Label printers through Home Assistant (B1, B3)

New trust edge in 0.5: link, then the Supervisor (`http://supervisor/core/api`
on the hassio network), then a Home Assistant core action of the Spoken Parts
Printers integration, which talks to the label printer over Bluetooth. No new
inbound port: every call is made by the link.

| Threat | Mitigation |
|---|---|
| The Supervisor token is misused | The token can call any Home Assistant action and read every state. The link sends it only in the Authorization header to the Supervisor, never through a proxy, never on a redirect. `internal/haapi` refuses any call but `spoken_parts_printers.list_printers` and `spoken_parts_printers.print_raster` before a request is made, and a repository test fails if any other code reads the token or names the Supervisor's address. The token is registered with the log redactor and no error includes it. ffmpeg runs with an empty environment, so it never gets the token. |
| Process compromise | Code running as the link could use the token for anything Home Assistant allows. The same protections as before apply (uid 65532, no shell, AppArmor). This is the main new risk (section 5). |
| A compromised backend prints something | It could queue label jobs for this link's label printers: it can waste labels. A job names a printer the link reported and goes only to the integration's `print_raster`, which only prints. It cannot reach any other Home Assistant action or any 3D printer. |
| A label is printed twice | A job is claimed once (the backend answers 409 to a second claim) and handed to Home Assistant at most once: the link remembers each job it claimed and never hands it on again, even when Home Assistant does not answer (the job is reported as failed with `timeout`). Only the state report is retried. |
| A label is printed late | Spoken Parts expires queued jobs 3 minutes after they were made and refuses to hand out an expired job. |
| Payload abuse | The label raster (`pages[].data_b64`) is opaque to the link: it is not decoded, stored or logged, only passed to Home Assistant as received. A claimed job answer is capped at 2 MB. Home Assistant's answers are capped at 1 MB, and printer names, states and messages are cut to the backend's sizes before they are reported or shown (the page escapes them; it has no scripts). |
| Integration missing or Home Assistant down | The link still reports, with `integration.present=false` and a short code (`not_installed`, `ha_unreachable`, `no_token`, `http_<n>`), so Spoken Parts can tell the owner. A job that arrives meanwhile fails with `no_integration` or `timeout`. |
| Paused or removed link | Nothing is reported, polled or printed while Spoken Parts pauses the link, and the runner stops and forgets its status when the link is removed. |
| 3D printers | Label jobs never use MQTT, FTPS or a Bambu printer. A repository test fails if the label or Supervisor code imports the printer, MQTT, FTPS, sends or camera packages. The MQTT allow list is unchanged. |

### 4.11 Supply chain (B4)

| Threat | Mitigation |
|---|---|
| Malicious change to the code | Code owners review, protected main branch, no `pull_request_target` workflows, read only default token. |
| Compromised GitHub Action | Every action pinned to a full commit SHA. Dependabot proposes updates. |
| Tampered image | Release images are signed with keyless cosign and carry build provenance and SBOM attestations from the release workflow on a version tag. The release job runs in a protected environment that needs a maintainer's approval. |
| Dependencies | No third party Go modules. The builder image and the Alpine image ffmpeg comes from are pinned by digest, and the ffmpeg package by version (installed with `--no-scripts`; only its binary and libraries are copied into the image). `govulncheck` runs in CI. Builds are reproducible (`-trimpath`, no build ID), and CI checks two builds match byte for byte. |

## 5. Risks that remain

1. **Home Assistant does not check image signatures.** A compromised release
   pipeline could ship a bad image. Checking is up to the owner (see DOCS.md).
2. **A stolen or copied link works until it is removed in Spoken Parts.**
3. **A leaked backup plus its password exposes the access codes** unless the
   link is removed in Spoken Parts first (section 4.6).
4. **Trusting a printer by hand** accepts whatever certificate is shown. It is
   only offered for a serial that never passed the CA check, so an attacker on
   the LAN can only use it against a printer that has not been set up yet. The
   page shows the certificate fingerprint and asks the owner to be sure.
5. **A real Bambu Lab certificate taken from a rooted printer** would pass the
   certificate check for that printer's serial.
6. **Anyone on the LAN with an access code** can control that printer directly.
   This is how Bambu Lab LAN access works. Keep printers on a trusted network
   and change access codes when people or devices leave.
7. **A person starts the wrong thing.** In Cloud mode a person taps the file. The
   link reports a wrong file or wrong slot, but cannot stop the print.
8. **Files on the USB stick.** Anyone at the printer can read the uploaded file
   until the print ends and the link deletes it.
9. **ffmpeg parses camera data.** A bug in ffmpeg's H.264 decoder could be
   reached by a malicious camera stream that passed the certificate check (in
   practice, a compromised printer). It runs unprivileged under AppArmor with
   no network use of its own, but it can read the app's data folder.
10. **Host compromise.** An app with full access, or a compromise of the Home
   Assistant host, can read everything on that host, including this app's data.
11. **The Supervisor token (0.5).** If the link process itself is taken over,
   the attacker has the app's Home Assistant token and can do whatever Home
   Assistant's REST API allows, not just print labels. The allow list in
   `internal/haapi` limits what the link's own code does, not what a
   compromised process can do. Owners who do not use label printers still
   grant this permission when they install 0.5.

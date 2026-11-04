# Threat model: Spoken Parts Link

This covers the link program in this repository (version 0.1, status only), its
pairing with Spoken Parts, the data it keeps on the device, and how release
images are built. It is written for customers, auditors and contributors.

## 1. What the link protects

1. **Printer access codes.** An 8 character code gives full local control of a
   Bambu Lab printer on the LAN. This is the most valuable thing the link holds.
2. **The link key.** An Ed25519 private key that proves "this is the link for
   site X" to Spoken Parts. Stored as a plain file (see 4.5).
3. **The local secret.** 32 random bytes that, together with a key from Spoken
   Parts, open the access code file. Stored as a plain file (see 4.5).
4. **Printer control.** In version 0.1 the link sends nothing to a printer
   except the standard status request, so it cannot be used to control one.
5. **Status data.** What each printer is doing. Low sensitivity, but it shows
   business volume.
6. **The release pipeline.** Whatever it ships runs on customer networks.

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
| Sniffing | MQTT runs over TLS 1.2. The access code is only sent inside that session. |
| LAN device opens the link page | The page binds to the Supervisor interface (172.30.32.1) and refuses any source other than the ingress proxy (172.30.32.2). |
| Discovery abuse | The link only listens. Packets over 2 KiB are dropped and fields are validated. The announced address must be the sender's own, on a private LAN range (RFC 1918 or IPv6 ULA). Each source and all sources together are rate limited, and a full source table never resets a limited source. A printer's address changes at most once a minute, never while a checked session is up, and an address that flips back is held for 5 minutes. Unconfigured printers are forgotten after a day, and the list is capped. |
| Printer overload | One session per printer, and one status request at connect, at most every 5 minutes. A wrong access code is not retried until a new one is saved. |

### 4.4 The Home Assistant host (B3)

| Threat | Mitigation |
|---|---|
| Other apps read `/data` | Supervisor keeps each app's data separate. The app asks for no Supervisor or Home Assistant API access, no privileged capabilities and no device access. A custom AppArmor profile limits files, capabilities and sockets. |
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

In version 0.1 a compromised backend can see status data and can stop links (by
revoking them). It cannot read access codes: they are never sent. It cannot
control printers through the link: the link sends nothing to a printer except
the status request, and the MQTT client has no general publish function.

### 4.8 Supply chain (B4)

| Threat | Mitigation |
|---|---|
| Malicious change to the code | Code owners review, protected main branch, no `pull_request_target` workflows, read only default token. |
| Compromised GitHub Action | Every action pinned to a full commit SHA. Dependabot proposes updates. |
| Tampered image | Release images are signed with keyless cosign and carry build provenance and SBOM attestations from the release workflow on a version tag. The release job runs in a protected environment that needs a maintainer's approval. |
| Dependencies | No third party Go modules. The builder image is pinned by digest. `govulncheck` runs in CI. Builds are reproducible (`-trimpath`, no build ID), and CI checks two builds match byte for byte. |

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
7. **Host compromise.** An app with full access, or a compromise of the Home
   Assistant host, can read everything on that host, including this app's data.

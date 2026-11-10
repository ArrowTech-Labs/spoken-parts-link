# Spoken Parts Link

Spoken Parts Link is a small program that connects the Bambu Lab printers at a
print farm to [Spoken Parts](https://spokenparts.com). It runs as a Home
Assistant app (add-on) on a computer at the site, such as a Raspberry Pi.

It reads what each printer is doing and reports it to Spoken Parts. From
version 0.2 it also uploads the prints Spoken Parts sends to the printer's USB
stick (FTPS), sees them start when someone taps the file on the printer screen,
and reports their progress until they finish. Printers stay in Bambu's Cloud
mode. The link never pauses, stops or moves a printer.

## Install

You need Home Assistant OS on a computer on the same network as your printers.

[![Add the repository to Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FArrowTech-Labs%2Fspoken-parts-link)

Or in Home Assistant go to Settings, then Apps, then Install app. Open the menu,
choose Repositories and add `https://github.com/ArrowTech-Labs/spoken-parts-link`.
Then install Spoken Parts Link and start it.

The full guide is in [spoken_parts_link/DOCS.md](spoken_parts_link/DOCS.md). It
is also the app's Documentation tab in Home Assistant.

## How it works

- **Pairing.** On first start the link makes its own Ed25519 key. You type a
  code from Spoken Parts on the link page. Both sides then show the same 4
  digit check number, and you confirm it in Spoken Parts.
- **Signed requests.** Every request to Spoken Parts is signed with the link's
  key, with a timestamp and a single use nonce. There is no password or token
  that could leak. The link only makes outgoing HTTPS connections.
- **Discovery.** The link listens for the printers' own broadcasts on UDP port
  2021. It never sends discovery packets.
- **Printer status.** For each printer with an access code, the link keeps one
  MQTT session over TLS 1.2. It checks that the printer's certificate is signed
  by Bambu Lab and names the printer's serial before it sends the access code.
  A printer that passed this check once is never offered for manual trust
  again.
  The only MQTT message it sends to a printer is the standard "pushall" status
  request, at most once every 5 minutes. (A `project_file` start exists for
  printers in Developer Mode, behind `SP_LINK_AUTOSTART`, which is off. It is
  not an app option.) The allowed commands are one constant,
  `mqtt.AllowedCommands`, and a repository test fails if any other printer
  command appears in the code.
- **Sending prints.** The link polls Spoken Parts for sends (signed, outgoing
  only). It downloads each file once with a one-time token sent in a header,
  checks its SHA-256 and that it is a complete sliced plate (G-code md5), then
  uploads it over implicit FTPS on port 990: TLS 1.2, the same Bambu CA and
  serial check as MQTT before the access code is sent, and the data connection
  resumes the control connection's TLS session. It sees the print start in the
  printer's report and sends progress events until the print ends, then
  deletes its own file (names `SP<printer>-...gcode.3mf` only).
- **Access codes.** You type them on the link page. They are stored in
  `/data/secrets/codes.enc`, encrypted with AES-256-GCM. The key is derived
  from a secret on the device and a key that Spoken Parts gives only to this
  link. The access codes are never sent to Spoken Parts. If the link is removed
  in Spoken Parts, the link deletes them. The link key and the local secret
  are plain files readable only by the app (encrypting them on the same disk
  would not help). They are in Home Assistant backups, so a backup plus its
  password can act as the link until it is removed in Spoken Parts.
- **The link page** only answers Home Assistant's ingress proxy. It has no
  scripts, a strict content security policy and a CSRF token on every form.

More detail: [THREAT-MODEL.md](THREAT-MODEL.md) and [SECURITY.md](SECURITY.md).

## Check a release image

Release images are built and signed by GitHub Actions with keyless cosign. They
also carry build provenance and an SBOM attestation.

```
cosign verify ghcr.io/arrowtech-labs/spoken-parts-link:0.2.2 --certificate-identity-regexp '^https://github.com/ArrowTech-Labs/spoken-parts-link/.github/workflows/release.yml@refs/tags/v' --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/arrowtech-labs/spoken-parts-link:0.2.2 --owner ArrowTech-Labs
```

## Build from source

Go 1.27.3 or newer. No third party Go modules are used.

```
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=0.2.2" ./cmd/spoken-parts-link
docker buildx build -f spoken_parts_link/Dockerfile --platform linux/amd64,linux/arm64 .
```

For development the program reads these environment variables:

| Variable | Default | Use |
|---|---|---|
| `SP_LINK_SERVER` | `https://spokenparts.com` | Backend URL. Must be https, except for localhost. |
| `SP_LINK_LISTEN` | `172.30.32.1:8099` | Link page address. |
| `SP_LINK_ALLOW` | `172.30.32.2` | Comma separated source IPs allowed to open the page. |
| `SP_LINK_DATA` | `/data` | Data folder. |
| `SP_LINK_SSDP` | `0.0.0.0:2021` | Discovery port. |
| `SP_LINK_AUTOSTART` | off | `true` lets the link start prints on Developer Mode printers. Not an app option. |

## Repository layout

```
repository.yaml              Home Assistant app repository
spoken_parts_link/           the app: config.yaml, Dockerfile, AppArmor profile, docs
cmd/spoken-parts-link/       main program
internal/                    identity, signing, API client, encrypted store,
                             discovery, MQTT client, printers, FTPS client,
                             3MF check, sends, web page, app
internal/printers/ca/        public Bambu Lab CA certificates (see SOURCES.md)
```

## License

[PolyForm Shield 1.0.0](LICENSE). You may use Spoken Parts Link for free,
including commercially, for example to run your print farm. You may not resell
it or use it to offer a product that competes with Spoken Parts. See the
LICENSE file for the full terms.

Required Notice: Copyright ArrowTech Labs (https://spokenparts.com)

# Security policy

## Reporting a problem

Please report security problems privately. Do not open a public issue.

Use GitHub's private vulnerability reporting for this repository:
https://github.com/ArrowTech-Labs/spoken-parts-link/security/advisories/new

Tell us what you found, how to reproduce it and which version you used. We aim
to answer within 3 business days and will keep you updated until it is fixed.
We are glad to credit you in the advisory if you want.

Please do not test against other people's printers, Home Assistant systems or
Spoken Parts accounts. Use your own equipment and test accounts.

## Supported versions

| Version | Supported |
|---|---|
| 0.4.x | Yes (latest release only) |

The app is experimental. Fixes ship as a new release. Update in Home Assistant
under Settings, then Apps.

## What is in scope

- The link program in this repository and its release images on
  `ghcr.io/arrowtech-labs/spoken-parts-link`.
- The release workflow and its signatures and attestations.

Problems in the Spoken Parts service itself can also be reported here; we will
route them.

## Design notes

See [THREAT-MODEL.md](THREAT-MODEL.md) for what the link protects, how, and the
risks that remain.

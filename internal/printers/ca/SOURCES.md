# Bambu Lab printer CA certificates

These are public CA certificates. Bambu Lab printers present a TLS certificate on
port 8883 that chains to one of them, with the printer serial as the certificate
common name. The link checks the chain and the common name before it sends a
printer access code.

No private keys are here. Only public certificates.

## Source

- Project: ha-bambulab (https://github.com/greghesp/ha-bambulab)
- Path: `custom_components/bambu_lab/pybambu/certs/`
- Commit: `242be3e38e9bddc72ff5d42672bfcb1383f75cdf` (main, fetched 2026-11-04)
- License of that project: MIT ("Copyright (c) 2023 ha-bambulab contributors").
  The certificates themselves are public data published by their issuer,
  BBL Technologies Co. Ltd.

The upstream file `bambu.cert` holds five certificates. They are split here into
one file per certificate. The other three upstream files hold one each.

## Certificates

"sha256" is the SHA-256 of the DER encoded certificate (the usual certificate
fingerprint, as printed by `openssl x509 -noout -fingerprint -sha256`).

| File | Subject CN | Issuer CN | Kind | Valid until | sha256 |
|---|---|---|---|---|---|
| bbl-ca.pem | BBL CA | BBL CA | root (self signed) | 2032-04-01 | 030bca81cece18b7eff3cfd2b75d09d3efca893bc069609e37fa04257fe4d840 |
| bbl-ca2-rsa.pem | BBL CA2 RSA | BBL CA2 RSA | root (self signed) | 2050-06-17 | e98f19578b3f124ace6b8a247ffeda52dc99c89fd4e7d20c82829977b7f33502 |
| bbl-ca2-ecc.pem | BBL CA2 ECC | BBL CA2 ECC | root (self signed) | 2050-06-17 | 1f99d846718c8d333ac319cb8e70919ab63236090fbadde909ba4a6f0867a683 |
| bbl-ca2-rsa-cross.pem | BBL CA2 RSA | BBL CA | intermediate (cross signed) | 2035-06-15 | 3d13019b45a4fa8a6cb8dc0fd7becbb19bb2bf0c2fcb9434a9345fa58680da0b |
| bbl-ca2-ecc-cross.pem | BBL CA2 ECC | BBL CA | intermediate (cross signed) | 2035-06-15 | e898483224f9092f575d627433bb21ed9d125a77e3635b0da6fc18fe850a02d7 |
| bbl-device-ca-o1c2-v2.pem | BBL Device CA O1C2-V2 | BBL CA2 RSA | intermediate | 2040-07-15 | ce667b6c001d26c34ed97333fb6099699409fc3d4e12429c37baebb05c46c60a |
| bbl-device-ca-n7-v2.pem | BBL Device CA N7-V2 | BBL CA2 RSA | intermediate | 2040-06-26 | 6a854d546ed6e3ce7a2112657e53f4555374413e6f5cb55f995d1ddd7c06dc69 |
| bbl-device-ca-n6-v2.pem | BBL Device CA N6-V2 | BBL CA2 RSA | intermediate | 2040-06-26 | 68bf3182bc32d5a5454f864928abaa291941f6d5ac6a86a0e5ad6ecfe2d5477b |

Self signed certificates are loaded as trust roots. The others are loaded as
intermediates, so a printer that does not send its intermediate still verifies.
`ca_test.go` checks that every file parses and that the fingerprints above match.

## Updating

When Bambu Lab ships a printer that chains to a new CA, the link shows
"We couldn't check this printer's certificate." for it. The owner can trust that
printer by hand until the new CA is added here. To add one: fetch it from a public
source, check it is a CA certificate, add the file and a row above, and update the
fingerprint list in `ca_test.go`.

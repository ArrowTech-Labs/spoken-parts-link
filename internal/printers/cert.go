package printers

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"
)

//go:embed ca/*.pem
var caFS embed.FS

// Bundle is the set of Bambu Lab printer CA certificates.
type Bundle struct {
	Roots         *x509.CertPool
	Intermediates *x509.CertPool
	Certs         []*x509.Certificate
}

// LoadBundle parses the embedded CA files (see ca/SOURCES.md).
func LoadBundle() (*Bundle, error) {
	names, err := fs.Glob(caFS, "ca/*.pem")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var pems [][]byte
	for _, n := range names {
		b, err := caFS.ReadFile(n)
		if err != nil {
			return nil, err
		}
		pems = append(pems, b)
	}
	return BundleFromPEM(pems...)
}

// BundleFromPEM builds a bundle from PEM data. Self signed certificates
// become roots and the others intermediates. Every certificate must be a CA.
// Tests use it to install a runtime generated test CA.
func BundleFromPEM(pems ...[]byte) (*Bundle, error) {
	b := &Bundle{Roots: x509.NewCertPool(), Intermediates: x509.NewCertPool()}
	for _, data := range pems {
		for {
			var blk *pem.Block
			blk, data = pem.Decode(data)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("unexpected PEM block %q in CA bundle", blk.Type)
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, err
			}
			if !c.IsCA {
				return nil, fmt.Errorf("certificate %q is not a CA", c.Subject.CommonName)
			}
			b.Certs = append(b.Certs, c)
			if bytes.Equal(c.RawSubject, c.RawIssuer) && c.CheckSignatureFrom(c) == nil {
				b.Roots.AddCert(c)
			} else {
				b.Intermediates.AddCert(c)
			}
		}
	}
	if len(b.Certs) == 0 {
		return nil, errors.New("CA bundle is empty")
	}
	return b, nil
}

// Fingerprint is "sha256:" plus the lower case hex SHA-256 of the DER bytes.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CertState is the trust state of a printer's TLS certificate.
type CertState string

// Cert states.
const (
	CertUnknown   CertState = "unknown"   // not checked yet
	CertTrusted   CertState = "trusted"   // chains to a Bambu CA and CN = serial
	CertPinned    CertState = "pinned"    // the owner trusted this exact certificate
	CertUntrusted CertState = "untrusted" // never checked out and never pinned: the owner may trust it
	// CertChanged: this serial passed the CA check before (or was pinned),
	// and now shows a certificate that does not. The owner is never offered
	// to trust it; they must remove the printer and add it again.
	CertChanged CertState = "changed"
	// CertClock: the chain and name are right but the certificate is outside
	// its validity dates, most likely because this device's clock is wrong.
	// Retried later, never offered for trust.
	CertClock CertState = "clock"
)

type verdict struct {
	state  CertState
	fp     string
	reason string
}

// verify decides whether the printer's certificate can be trusted, before
// any access code is sent. pin is the owner's pinned fingerprint or "".
// known is true when this serial passed the CA check at least once before.
func (b *Bundle) verify(certs []*x509.Certificate, serial, pin string, known bool, now time.Time) verdict {
	if len(certs) == 0 {
		return verdict{state: CertUntrusted, reason: "no certificate"}
	}
	leaf := certs[0]
	v := verdict{fp: Fingerprint(leaf)}
	chainAt := func(t time.Time) error {
		if b == nil || b.Roots == nil {
			return errors.New("no CA bundle")
		}
		inter := b.Intermediates.Clone()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots:         b.Roots,
			Intermediates: inter,
			CurrentTime:   t,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		})
		return err
	}
	err := chainAt(now)
	chainOK := err == nil
	clockOnly := false
	var cie x509.CertificateInvalidError
	if !chainOK && errors.As(err, &cie) && cie.Reason == x509.Expired {
		// Expired or not yet valid. If the chain verifies at a time inside
		// the leaf's validity, only the clock is in question.
		clockOnly = chainAt(leaf.NotBefore.Add(time.Minute)) == nil
	}
	cnOK := leaf.Subject.CommonName == serial
	reason := "not signed by a known Bambu Lab CA"
	if (chainOK || clockOnly) && !cnOK {
		reason = "certificate name does not match the serial"
	}
	switch {
	case chainOK && cnOK:
		v.state = CertTrusted
	case clockOnly && cnOK:
		v.state, v.reason = CertClock, "certificate dates do not match this device's clock"
	case pin != "" && pin == v.fp:
		v.state = CertPinned
	case known || pin != "":
		v.state, v.reason = CertChanged, "certificate changed and could not be checked"
	default:
		v.state, v.reason = CertUntrusted, reason
	}
	return v
}

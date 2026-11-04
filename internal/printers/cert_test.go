package printers

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const serial1 = "01P00A000000001"

func TestVerify(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	other := testutil.NewCA(t, "Other CA")
	b, err := BundleFromPEM(ca.PEM)
	if err != nil {
		t.Fatal(err)
	}
	good := ca.Leaf(t, serial1).Leaf
	wrongCN := ca.Leaf(t, "01P00A000000002").Leaf
	foreign := other.Leaf(t, serial1).Leaf
	now := time.Now()
	cases := []struct {
		name  string
		certs []*x509.Certificate
		pin   string
		known bool
		at    time.Time
		want  CertState
	}{
		{"chain and CN", []*x509.Certificate{good}, "", false, now, CertTrusted},
		{"CN mismatch", []*x509.Certificate{wrongCN}, "", false, now, CertUntrusted},
		{"unknown CA", []*x509.Certificate{foreign}, "", false, now, CertUntrusted},
		{"unknown CA pinned", []*x509.Certificate{foreign}, Fingerprint(foreign), false, now, CertPinned},
		{"pinned but changed", []*x509.Certificate{foreign}, Fingerprint(good), false, now, CertChanged},
		{"verified before, now unknown CA", []*x509.Certificate{foreign}, "", true, now, CertChanged},
		{"verified before, now CN mismatch", []*x509.Certificate{wrongCN}, "", true, now, CertChanged},
		{"none", nil, "", false, now, CertUntrusted},
		{"CA chain wins over a stale pin", []*x509.Certificate{good}, Fingerprint(foreign), false, now, CertTrusted},
		{"clock ahead", []*x509.Certificate{good}, "", false, now.Add(48 * time.Hour), CertClock},
		{"clock behind", []*x509.Certificate{good}, "", true, now.Add(-48 * time.Hour), CertClock},
		{"clock wrong and CN mismatch", []*x509.Certificate{wrongCN}, "", false, now.Add(48 * time.Hour), CertUntrusted},
		{"clock wrong and unknown CA", []*x509.Certificate{foreign}, "", false, now.Add(48 * time.Hour), CertUntrusted},
	}
	for _, c := range cases {
		v := b.verify(c.certs, serial1, c.pin, c.known, c.at)
		if v.state != c.want {
			t.Errorf("%s: got %s want %s (%s)", c.name, v.state, c.want, v.reason)
		}
	}
	var nilBundle *Bundle
	if v := nilBundle.verify([]*x509.Certificate{good}, serial1, "", false, now); v.state != CertUntrusted {
		t.Errorf("nil bundle trusted a cert")
	}
}

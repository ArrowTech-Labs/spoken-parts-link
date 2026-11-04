package printers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const goodCode = "AbCd1234"

type fakeCodes struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeCodes) Get(s string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.m[s]
	return c, ok
}
func (f *fakeCodes) Has(s string) bool { _, ok := f.Get(s); return ok }
func (f *fakeCodes) Locked() bool      { return false }
func (f *fakeCodes) set(s, c string) {
	f.mu.Lock()
	f.m[s] = c
	f.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	m      *Manager
	broker *testutil.Broker
	codes  *fakeCodes
	clock  *clock
	ca     *testutil.CA
	log    *bytes.Buffer
	dials  []string
	routes map[string]string // printer address -> broker address; "" = unreachable
	mu     sync.Mutex
}

// route sends dials for ip to broker b (nil: the address is unreachable).
func (r *rig) route(ip string, b *testutil.Broker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b == nil {
		r.routes[ip] = ""
	} else {
		r.routes[ip] = b.Addr
	}
}

func (r *rig) dialCount(ip string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dials {
		if d == ip+":8883" {
			n++
		}
	}
	return n
}

func newRig(t *testing.T, bundleCA *testutil.CA, leaf *testutil.CA) *rig {
	t.Helper()
	r := &rig{codes: &fakeCodes{m: map[string]string{}}, clock: &clock{t: time.Now()}, ca: bundleCA, log: &bytes.Buffer{}, routes: map[string]string{}}
	report := []byte(`{"print":{"gcode_state":"RUNNING","mc_percent":42,"mc_remaining_time":70,"fun":"3EC18FFF9CFF"}}`)
	r.broker = testutil.NewBroker(t, leaf.Leaf(t, serial1), serial1, goodCode, report)
	b, err := BundleFromPEM(bundleCA.PEM)
	if err != nil {
		t.Fatal(err)
	}
	red := redact.New()
	red.Add(goodCode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var lw syncWriter
	lw.b = r.log
	r.m, err = New(ctx, Config{
		Bundle:   b,
		Codes:    r.codes,
		MetaPath: filepath.Join(t.TempDir(), "printers.json"),
		Log:      logx.New(&lw, red, logx.Debug),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			r.mu.Lock()
			r.dials = append(r.dials, addr)
			host, _, _ := net.SplitHostPort(addr)
			target, routed := r.routes[host]
			r.mu.Unlock()
			if !routed {
				target = r.broker.Addr
			}
			if target == "" {
				return nil, errors.New("no route to host")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, target)
		},
		RetryMin: 10 * time.Millisecond,
		RetryMax: 40 * time.Millisecond,
		Now:      r.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.m.Close)
	return r
}

type syncWriter struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func announce(ip string) ssdp.Announcement { return announceSerial(serial1, ip) }

func announceSerial(serial, ip string) ssdp.Announcement {
	return ssdp.Announcement{Serial: serial, IP: netip.MustParseAddr(ip), ModelCode: "O1D", Name: "H2D", Firmware: "01.04.20.00", Connect: "lan", At: time.Now()}
}

func (r *rig) snap(t *testing.T) Snapshot {
	t.Helper()
	s := r.m.Snapshot()
	if len(s) != 1 {
		t.Fatalf("want 1 printer, got %d", len(s))
	}
	return s[0]
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionHappyPath(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "report", func() bool { return r.snap(t).GcodeState == "RUNNING" })
	s := r.snap(t)
	if s.Access != AccessOK || s.Cert != CertTrusted || !s.Online || *s.Percent != 42 || *s.RemainingMin != 70 || s.DevMode == nil || !*s.DevMode {
		t.Fatalf("%+v", s)
	}
	if s.LanOnly == nil || !*s.LanOnly || s.Model != "H2D" || !strings.HasPrefix(s.CertFingerprint, "sha256:") {
		t.Fatalf("%+v", s)
	}
	pubs := r.broker.Publishes()
	if len(pubs) != 1 || pubs[0].Payload != mqtt.PushAllPayload || pubs[0].Topic != "device/"+serial1+"/request" {
		t.Fatalf("publishes %+v", pubs)
	}
	r.mu.Lock()
	if r.dials[0] != "192.168.1.40:8883" {
		t.Fatalf("dialed %v", r.dials)
	}
	r.mu.Unlock()
}

func TestPushAllAtMostOncePerFiveMinutes(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "first pushall", func() bool { return len(r.broker.Publishes()) == 1 })
	for i := 0; i < 3; i++ {
		before := len(r.broker.Connects())
		r.broker.DropAll()
		waitFor(t, "reconnect", func() bool { return len(r.broker.Connects()) > before && r.snap(t).Connected })
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(r.broker.Publishes()); n != 1 {
		t.Fatalf("pushall sent %d times within 5 minutes", n)
	}
	r.clock.add(5 * time.Minute)
	before := len(r.broker.Connects())
	r.broker.DropAll()
	waitFor(t, "reconnect", func() bool { return len(r.broker.Connects()) > before })
	waitFor(t, "second pushall", func() bool { return len(r.broker.Publishes()) == 2 })
}

func TestUntrustedCertNeverGetsTheCode(t *testing.T) {
	bundle := testutil.NewCA(t, "Test Printer CA")
	rogue := testutil.NewCA(t, "Rogue CA")
	r := newRig(t, bundle, rogue)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "untrusted", func() bool { return r.snap(t).Cert == CertUntrusted })
	time.Sleep(100 * time.Millisecond)
	if len(r.broker.Connects()) != 0 {
		t.Fatal("access code sent to an untrusted printer")
	}
	s := r.snap(t)
	if s.Candidate == "" || s.Candidate != s.CertFingerprint {
		t.Fatalf("no candidate: %+v", s)
	}
	if err := r.m.Trust(serial1, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("trusted a fingerprint that was not shown")
	}
	if err := r.m.Trust(serial1, s.Candidate); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pinned connect", func() bool { s := r.snap(t); return s.Cert == CertPinned && s.Access == AccessOK })

	// The printer now shows a different certificate: untrusted again.
	other := testutil.NewCA(t, "Other Rogue CA")
	r.broker.SetCert(other.Leaf(t, serial1))
	n := len(r.broker.Connects())
	r.broker.DropAll()
	waitFor(t, "changed", func() bool { return r.snap(t).Cert == CertChanged })
	time.Sleep(100 * time.Millisecond)
	if len(r.broker.Connects()) != n {
		t.Fatal("access code sent after the certificate changed")
	}
	if s := r.snap(t); s.Candidate != "" || !s.Known || !s.Spoofed {
		t.Fatalf("a changed pinned printer must not be offered for trust: %+v", s)
	}
	if err := r.m.Trust(serial1, r.snap(t).CertFingerprint); err == nil {
		t.Fatal("trusted a changed certificate")
	}
}

func TestWrongCodeStopsRetrying(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, "Wrong999")
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "wrong", func() bool { return r.snap(t).Access == AccessWrong })
	time.Sleep(150 * time.Millisecond)
	if n := len(r.broker.Connects()); n != 1 {
		t.Fatalf("retried a wrong code %d times", n)
	}
	r.m.Observe(announce("192.168.1.40"))
	time.Sleep(50 * time.Millisecond)
	if n := len(r.broker.Connects()); n != 1 {
		t.Fatal("rediscovery retried a wrong code")
	}
	r.codes.set(serial1, goodCode)
	r.m.CodeSaved(serial1)
	waitFor(t, "ok after new code", func() bool { return r.snap(t).Access == AccessOK })
}

func TestNoCodeNoConnection(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	time.Sleep(100 * time.Millisecond)
	if r.broker.Handshakes() != 0 || r.snap(t).Access != AccessMissing {
		t.Fatal("connected without a code")
	}
}

func TestDisabledNoConnection(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.Observe(announce("192.168.1.40"))
	time.Sleep(100 * time.Millisecond)
	if r.broker.Handshakes() != 0 {
		t.Fatal("connected while disabled")
	}
}

func TestIPChangeReconnects(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "connected", func() bool { return r.snap(t).Connected })
	// The printer really moves: its old address goes dark.
	r.route("192.168.1.40", nil)
	r.broker.DropAll()
	waitFor(t, "disconnected", func() bool { return !r.snap(t).Connected })
	r.m.Observe(announce("192.168.1.41"))
	waitFor(t, "connected at the new address", func() bool {
		s := r.snap(t)
		return s.Connected && s.IP == "192.168.1.41" && r.dialCount("192.168.1.41") > 0
	})
}

func TestIPChangeIgnoredWhileConnected(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "connected", func() bool { return r.snap(t).Connected })
	for i := 0; i < 40; i++ {
		r.m.Observe(announce("192.168.1." + strconv.Itoa(50+i%2)))
	}
	time.Sleep(100 * time.Millisecond)
	if r.dialCount("192.168.1.50")+r.dialCount("192.168.1.51") != 0 || len(r.broker.Connects()) != 1 || r.snap(t).IP != "192.168.1.40" {
		t.Fatalf("followed spoofed addresses: dials %v connects %d", r.dials, len(r.broker.Connects()))
	}
}

func TestIPChangeRateLimitAndFlapHold(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca) // no code: no sessions, only address bookkeeping
	ip := func() string { return r.snap(t).IP }
	r.m.Observe(announce("192.168.1.40"))
	r.m.Observe(announce("192.168.1.41"))
	if ip() != "192.168.1.41" {
		t.Fatal("first change refused")
	}
	r.m.Observe(announce("192.168.1.42"))
	if ip() != "192.168.1.41" {
		t.Fatal("second change within 60 s accepted")
	}
	r.clock.add(61 * time.Second)
	r.m.Observe(announce("192.168.1.40")) // flips back: hold the first address
	if ip() != "192.168.1.40" {
		t.Fatalf("flap: %s", ip())
	}
	for _, d := range []time.Duration{61 * time.Second, 2 * time.Minute} {
		r.clock.add(d)
		r.m.Observe(announce("192.168.1.41"))
		if ip() != "192.168.1.40" {
			t.Fatalf("hold broken after %v: %s", d, ip())
		}
	}
	r.clock.add(3 * time.Minute)
	r.m.Observe(announce("192.168.1.41"))
	if ip() != "192.168.1.41" {
		t.Fatal("change refused after the hold ended")
	}
}

// TestSpoofOfVerifiedPrinterNeverGetsCode reproduces the review's attack:
// a printer that passed the CA check, then one spoofed announcement with its
// serial and the attacker's address. The link must not offer Trust, and the
// attacker's broker must never get the access code.
func TestSpoofOfVerifiedPrinterNeverGetsCode(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	rogue := testutil.NewCA(t, "Rogue CA")
	attacker := testutil.NewBroker(t, rogue.Leaf(t, serial1), serial1, goodCode)
	r.route("192.168.1.40", r.broker)
	r.route("192.168.1.66", attacker)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "checked", func() bool { s := r.snap(t); return s.Cert == CertTrusted && s.Connected })

	// While the checked session is up, a spoof is ignored outright.
	r.m.Observe(announce("192.168.1.66"))
	time.Sleep(50 * time.Millisecond)
	if r.dialCount("192.168.1.66") != 0 {
		t.Fatal("dialed the attacker while connected")
	}

	// The real printer goes quiet (power off), then the spoof arrives again.
	r.route("192.168.1.40", nil)
	r.broker.DropAll()
	waitFor(t, "disconnected", func() bool { return !r.snap(t).Connected })
	r.clock.add(61 * time.Second)
	r.m.Observe(announce("192.168.1.66"))
	waitFor(t, "attacker checked", func() bool { return r.dialCount("192.168.1.66") > 0 })
	waitFor(t, "changed", func() bool { return r.snap(t).Cert == CertChanged })
	s := r.snap(t)
	if s.Candidate != "" || !s.Known || !s.Spoofed {
		t.Fatalf("trust offered for a known printer: %+v", s)
	}
	if err := r.m.Trust(serial1, s.CertFingerprint); err == nil {
		t.Fatal("Trust accepted for a known printer")
	}
	// The link goes back to the last good address and keeps trying there.
	waitFor(t, "back to the good address", func() bool { return r.snap(t).IP == "192.168.1.40" })

	// The real printer comes back; the session recovers.
	r.route("192.168.1.40", r.broker)
	waitFor(t, "recovered", func() bool { s := r.snap(t); return s.Connected && s.Cert == CertTrusted })
	if n := len(attacker.Connects()); n != 0 {
		t.Fatalf("attacker received %d CONNECT packets", n)
	}
	r.m.Close()
	if strings.Contains(r.log.String(), goodCode) {
		t.Fatal("code in log")
	}
}

func TestVerifiedSurvivesRestart(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "checked", func() bool { return r.snap(t).Cert == CertTrusted })
	waitFor(t, "known", func() bool { return r.snap(t).Known })
	var meta map[string]store.PrinterMeta
	if err := store.LoadJSON(r.m.cfg.MetaPath, &meta); err != nil || !strings.HasPrefix(meta[serial1].Verified, "sha256:") {
		t.Fatalf("verified fingerprint not saved: %v %+v", err, meta)
	}
}

func TestForgetMakesPrinterNewAgain(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "known", func() bool { return r.snap(t).Known })
	if err := r.m.Forget(serial1); err != nil {
		t.Fatal(err)
	}
	if len(r.m.Snapshot()) != 0 {
		t.Fatal("printer still listed")
	}
	r.codes.mu.Lock()
	delete(r.codes.m, serial1)
	r.codes.mu.Unlock()
	r.m.Observe(announce("192.168.1.40"))
	if s := r.snap(t); s.Known || s.Access != AccessMissing {
		t.Fatalf("not new after forget: %+v", s)
	}
	if err := r.m.Forget("01P00A000000009"); err == nil {
		t.Fatal("forgot an unknown printer")
	}
}

func TestEvictionAndCap(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.m.cfg.MaxPrinters = 3
	serial := func(i int) string { return "01P00A00000010" + strconv.Itoa(i) }
	for i := 0; i < 3; i++ {
		r.m.Observe(announceSerial(serial(i), "192.168.1.4"+strconv.Itoa(i)))
	}
	r.codes.set(serial(1), goodCode) // a configured printer is never evicted
	r.m.Observe(announceSerial(serial(3), "192.168.1.43"))
	if r.m.Has(serial(3)) {
		t.Fatal("table grew past its cap")
	}
	r.clock.add(11 * time.Minute)
	r.m.Observe(announceSerial(serial(2), "192.168.1.42")) // still announcing
	r.m.Observe(announceSerial(serial(3), "192.168.1.43"))
	if !r.m.Has(serial(3)) || r.m.Has(serial(0)) || !r.m.Has(serial(1)) || !r.m.Has(serial(2)) {
		t.Fatal("full table must evict the longest quiet unconfigured printer")
	}
	r.clock.add(25 * time.Hour)
	snaps := r.m.Snapshot()
	if len(snaps) != 1 || snaps[0].Serial != serial(1) {
		t.Fatalf("stale unconfigured printers not evicted: %d left", len(snaps))
	}
}

func TestCodeNeverLogged(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, ca, ca)
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "ok", func() bool { return r.snap(t).Access == AccessOK })
	r.m.Close()
	if strings.Contains(r.log.String(), goodCode) {
		t.Fatal("code in log")
	}
}

func TestPackageHasNoRawPrinterWrites(t *testing.T) {
	for _, f := range []string{"manager.go", "cert.go", "report.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(src, []byte(".Write(")) {
			t.Errorf("%s writes to a connection directly; only mqtt.RequestPushAll may send", f)
		}
	}
	_ = io.Discard
}

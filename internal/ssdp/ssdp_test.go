package ssdp

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

func TestParseNotify(t *testing.T) {
	a, err := Parse(testutil.SSDPPacket("01P00A000000001", "192.168.1.40", "O1D", "H2D"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Serial != "01P00A000000001" || a.IP.String() != "192.168.1.40" || a.ModelCode != "O1D" || a.Name != "H2D" || a.Firmware != "01.04.20.00" || a.Connect != "lan" {
		t.Fatalf("%+v", a)
	}
}

func TestParseIsCaseInsensitive(t *testing.T) {
	p := "notify * HTTP/1.1\nlocation: 192.0.2.7\nnt: URN:BAMBULAB-COM:DEVICE:3DPRINTER:1\nusn: 01P00A000000002\nDEVMODEL.BAMBU.COM: C12\n\n"
	a, err := Parse([]byte(p))
	if err != nil || a.Serial != "01P00A000000002" || a.ModelCode != "C12" {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestParseRejects(t *testing.T) {
	base := string(testutil.SSDPPacket("01P00A000000001", "192.168.1.40", "O1D", "H2D"))
	cases := map[string]string{
		"too big":      base + strings.Repeat("X", MaxPacket),
		"not notify":   strings.Replace(base, "NOTIFY *", "M-SEARCH *", 1),
		"other device": strings.Replace(base, "urn:bambulab-com:device:3dprinter:1", "upnp:rootdevice", 1),
		"short usn":    strings.Replace(base, "USN: 01P00A000000001", "USN: 01P", 1),
		"usn chars":    strings.Replace(base, "USN: 01P00A000000001", "USN: 01P00A/../0001", 1),
		"long usn":     strings.Replace(base, "USN: 01P00A000000001", "USN: "+strings.Repeat("A", 41), 1),
		"bad ip":       strings.Replace(base, "Location: 192.168.1.40", "Location: printer.local", 1),
		"multicast":    strings.Replace(base, "Location: 192.168.1.40", "Location: 239.255.255.250", 1),
		"broadcast":    strings.Replace(base, "Location: 192.168.1.40", "Location: 255.255.255.255", 1),
		"unspecified":  strings.Replace(base, "Location: 192.168.1.40", "Location: 0.0.0.0", 1),
		"no location":  strings.Replace(base, "Location: 192.168.1.40\r\n", "", 1),
	}
	for name, p := range cases {
		if _, err := Parse([]byte(p)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseCleansFields(t *testing.T) {
	p := string(testutil.SSDPPacket("01P00A000000001", "http://192.168.1.40:80/desc", "O1D<script>", "My\x07 Printer "+strings.Repeat("n", 80)))
	a, err := Parse([]byte(p))
	if err != nil {
		t.Fatal(err)
	}
	if a.IP.String() != "192.168.1.40" || a.ModelCode != "" || strings.ContainsRune(a.Name, 7) || len([]rune(a.Name)) > 60 {
		t.Fatalf("%+v", a)
	}
}

func TestModelNames(t *testing.T) {
	want := map[string]string{"O1D": "H2D", "O1E": "H2D Pro", "O2D": "H2D Pro", "O1S": "H2S", "N6": "X2D", "BL-P001": "X1C", "C12": "P1S", "C11": "P1P", "N2S": "A1", "N1": "A1 mini", "ZZ": ""}
	for code, name := range want {
		if got := ModelName(code); got != name {
			t.Errorf("%s: %q want %q", code, got, name)
		}
	}
}

type collector struct {
	mu  sync.Mutex
	got []Announcement
}

func (c *collector) add(a Announcement) { c.mu.Lock(); c.got = append(c.got, a); c.mu.Unlock() }
func (c *collector) n() int             { c.mu.Lock(); defer c.mu.Unlock(); return len(c.got) }

func newTestListener() (*Listener, *collector, *time.Time) {
	c := &collector{}
	l := NewListener(nil, logx.New(io.Discard, redact.New(), logx.Info), c.add)
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return l, c, &now
}

func pkt(ip string) []byte { return testutil.SSDPPacket("01P00A000000001", ip, "O1D", "H2D") }

func TestLocationMustBeSenderOnTheLAN(t *testing.T) {
	l, c, _ := newTestListener()
	accept := []string{"192.168.1.40", "10.1.2.3", "172.16.0.9", "fd12:3456:789a::10"}
	for i, ip := range accept {
		l.Handle(netip.MustParseAddr(ip), pkt(ip))
		if c.n() != i+1 {
			t.Errorf("%s refused", ip)
		}
	}
	before := c.n()
	refuse := map[string][2]string{
		"location is not the sender": {"192.168.1.41", "192.168.1.40"},
		"public":                     {"203.0.113.5", "203.0.113.5"},
		"CGNAT":                      {"100.64.1.2", "100.64.1.2"},
		"link local":                 {"169.254.10.1", "169.254.10.1"},
		"IPv6 link local":            {"fe80::1", "fe80::1"},
		"IPv6 global":                {"2001:db8::1", "2001:db8::1"},
		"Home Assistant internal":    {"172.30.32.2", "172.30.32.2"},
		"loopback":                   {"127.0.0.1", "127.0.0.1"},
	}
	for name, v := range refuse {
		l.Handle(netip.MustParseAddr(v[0]), pkt(v[1]))
		if c.n() != before {
			t.Errorf("%s accepted", name)
			before = c.n()
		}
	}
	if ValidLANAddr(netip.MustParseAddr("::ffff:192.168.1.40")) != true {
		t.Error("IPv4 mapped address refused")
	}
}

func TestRateLimitPerSource(t *testing.T) {
	l, c, now := newTestListener()
	a, b := netip.MustParseAddr("192.168.1.40"), netip.MustParseAddr("192.168.1.41")
	for i := 0; i < 50; i++ {
		l.Handle(a, pkt("192.168.1.40"))
	}
	if c.n() != 5 {
		t.Fatalf("burst: %d", c.n())
	}
	l.Handle(b, pkt("192.168.1.41"))
	if c.n() != 6 {
		t.Fatal("other source limited")
	}
	*now = now.Add(3 * time.Second)
	for i := 0; i < 10; i++ {
		l.Handle(a, pkt("192.168.1.40"))
	}
	if c.n() != 9 {
		t.Fatalf("refill: %d", c.n())
	}
}

// The review found that 512 junk sources reset every bucket. Now a limited
// source stays limited, and a full table refuses new sources instead.
func TestLimiterTableFullDoesNotReset(t *testing.T) {
	lim := newLimiter(1, 5, 1e9, 1e9, 512)
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	victim := netip.MustParseAddr("192.168.1.40")
	for i := 0; i < 5; i++ {
		lim.allow(victim, now)
	}
	if lim.allow(victim, now) {
		t.Fatal("burst not used up")
	}
	for i := 0; i < 600; i++ {
		lim.allow(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), now)
	}
	if lim.allow(victim, now) {
		t.Fatal("limited source got a fresh burst after a source flood")
	}
	if lim.allow(netip.MustParseAddr("10.9.9.9"), now) {
		t.Fatal("new source admitted to a full table of active sources")
	}
	// After everyone has refilled, old entries are dropped losslessly.
	later := now.Add(10 * time.Second)
	if !lim.allow(netip.MustParseAddr("10.9.9.9"), later) {
		t.Fatal("refilled entries were not reclaimed")
	}
	if len(lim.buckets) > 512 {
		t.Fatalf("table grew to %d", len(lim.buckets))
	}
}

func TestGlobalBudget(t *testing.T) {
	l, c, now := newTestListener()
	// 300 spoofed sources in one instant: only the global burst gets through.
	for i := 0; i < 300; i++ {
		ip := netip.AddrFrom4([4]byte{192, 168, byte(2 + i/250), byte(1 + i%250)})
		l.Handle(ip, pkt(ip.String()))
	}
	if c.n() != globalBurst {
		t.Fatalf("got %d through, want the global burst %d", c.n(), globalBurst)
	}
	*now = now.Add(time.Second)
	for i := 0; i < 300; i++ {
		ip := netip.AddrFrom4([4]byte{192, 168, 9, byte(1 + i%250)})
		l.Handle(ip, pkt(ip.String()))
	}
	if got := c.n() - globalBurst; got > globalRate+1 {
		t.Fatalf("%d packets in the next second, budget is %d", got, globalRate)
	}
}

func TestListenerOverUDPNeverSends(t *testing.T) {
	conn, err := Listen("127.0.0.1:0", false, logx.New(io.Discard, redact.New(), logx.Info))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Announcement, 1)
	l := NewListener(conn, logx.New(io.Discard, redact.New(), logx.Info), func(a Announcement) { got <- a })
	l.validAddr = func(a netip.Addr) bool { return a.IsLoopback() } // test hook: the sender is 127.0.0.1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	c, err := net.Dial("udp4", conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(pkt("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.Serial != "01P00A000000001" || a.Source.String() != "127.0.0.1" {
			t.Fatalf("%+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no announcement")
	}
	// The listener must not answer.
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err == nil {
		t.Fatalf("listener sent %q", buf[:n])
	}
}

func TestPackageNeverWrites(t *testing.T) {
	// Passive only: no source file in this package may call a write method.
	for _, f := range []string{"ssdp.go", "join_unix.go", "join_other.go"} {
		src := testutil.ReadFile(t, f)
		for _, bad := range []string{".Write(", ".WriteTo(", ".WriteToUDP(", ".WriteMsgUDP(", "M-SEARCH"} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s contains %s", f, bad)
			}
		}
	}
}

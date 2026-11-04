// Package ssdp listens for the NOTIFY broadcasts Bambu Lab printers send on
// UDP port 2021. It is passive: it never sends a packet.
package ssdp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
)

// Port is where Bambu printers send their NOTIFY packets.
const Port = 2021

// Group is the SSDP multicast group.
const Group = "239.255.255.250"

// MaxPacket is the largest packet accepted. Bigger packets are dropped.
const MaxPacket = 2048

var (
	serialRe   = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)
	modelRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,20}$`)
	firmwareRe = regexp.MustCompile(`^[0-9A-Za-z._-]{1,40}$`)
)

// ValidSerial reports whether s looks like a printer serial (USN).
func ValidSerial(s string) bool { return serialRe.MatchString(s) }

// Announcement is one parsed printer NOTIFY.
type Announcement struct {
	Serial    string
	IP        netip.Addr
	ModelCode string
	Name      string
	Firmware  string
	// Connect is "lan" or "cloud" when the printer says, else "".
	Connect string
	Source  netip.Addr
	At      time.Time
}

// Errors from Parse.
var (
	ErrTooLarge   = errors.New("packet too large")
	ErrNotPrinter = errors.New("not a Bambu printer announcement")
	ErrBadSerial  = errors.New("bad USN")
	ErrBadIP      = errors.New("bad Location")
)

// Parse reads one packet. Header names are case insensitive.
func Parse(data []byte) (Announcement, error) {
	var a Announcement
	if len(data) > MaxPacket {
		return a, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return a, ErrNotPrinter
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return a, ErrNotPrinter
	}
	first := strings.ToUpper(strings.TrimSpace(lines[0]))
	if !strings.HasPrefix(first, "NOTIFY ") && !strings.HasPrefix(first, "HTTP/1.1 200") {
		return a, ErrNotPrinter
	}
	h := map[string]string{}
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if _, dup := h[k]; dup || k == "" {
			continue
		}
		h[k] = strings.TrimSpace(v)
	}
	nt := strings.ToLower(h["nt"] + " " + h["st"])
	if !strings.Contains(nt, "urn:bambulab-com:device:3dprinter") {
		return a, ErrNotPrinter
	}
	a.Serial = h["usn"]
	if !ValidSerial(a.Serial) {
		return Announcement{}, ErrBadSerial
	}
	ip, ok := parseLocation(h["location"])
	if !ok {
		return Announcement{}, ErrBadIP
	}
	a.IP = ip
	if m := h["devmodel.bambu.com"]; modelRe.MatchString(m) {
		a.ModelCode = m
	}
	a.Name = cleanName(h["devname.bambu.com"])
	if fw := h["devversion.bambu.com"]; firmwareRe.MatchString(fw) {
		a.Firmware = fw
	}
	switch strings.ToLower(h["devconnect.bambu.com"]) {
	case "lan":
		a.Connect = "lan"
	case "cloud":
		a.Connect = "cloud"
	}
	return a, nil
}

// parseLocation accepts a bare IP or a URL like http://ip[:port]/path.
func parseLocation(v string) (netip.Addr, bool) {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	var ip netip.Addr
	if ap, err := netip.ParseAddrPort(v); err == nil {
		ip = ap.Addr()
	} else if a, err := netip.ParseAddr(strings.Trim(v, "[]")); err == nil {
		ip = a
	} else {
		return netip.Addr{}, false
	}
	ip = ip.Unmap()
	// Loopback and other non LAN addresses are refused later, in Handle,
	// by ValidLANAddr.
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	if ip.Is4() && ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Addr{}, false
	}
	return ip, true
}

func cleanName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			continue
		}
		if n == 60 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// models maps SSDP model codes to names (CONTRACT 2.1 plus P1P and A1 mini).
var models = map[string]string{
	"O1D":     "H2D",
	"O1E":     "H2D Pro",
	"O2D":     "H2D Pro",
	"O1S":     "H2S",
	"N6":      "X2D",
	"BL-P001": "X1C",
	"C12":     "P1S",
	"C11":     "P1P",
	"N2S":     "A1",
	"N1":      "A1 mini",
}

// ModelName returns the product name for an SSDP model code, or "" if the
// code is not known.
func ModelName(code string) string { return models[code] }

// ValidLANAddr reports whether ip can be a printer on the local network:
// an RFC 1918 IPv4 address (but not Home Assistant's own 172.30.32.0/23)
// or an IPv6 unique local address. Public, loopback, link local, CGNAT and
// multicast addresses are refused.
func ValidLANAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" {
		return false
	}
	if ip.Is4() {
		return ip.IsPrivate() && !haInternal.Contains(ip)
	}
	return ip.Is6() && ula.Contains(ip)
}

var (
	haInternal = netip.MustParsePrefix("172.30.32.0/23")
	ula        = netip.MustParsePrefix("fc00::/7")
)

// bucket is a token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) take(rate, burst float64, now time.Time) bool {
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limiter is a global token bucket plus one per source. When the source
// table is full it drops buckets that have refilled completely (forgetting
// those loses nothing); if none has, packets from new sources are dropped.
// It never resets a source that is still limited.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // per source, tokens per second
	burst   float64
	global  bucket
	grate   float64
	gburst  float64
	buckets map[netip.Addr]*bucket
	max     int
}

func newLimiter(rate, burst, grate, gburst float64, maxSources int) *limiter {
	return &limiter{
		rate: rate, burst: burst, grate: grate, gburst: gburst,
		global:  bucket{tokens: gburst},
		buckets: map[netip.Addr]*bucket{}, max: maxSources,
	}
}

func (l *limiter) allow(src netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global.last.IsZero() {
		l.global.last = now
	}
	if !l.global.take(l.grate, l.gburst, now) {
		return false
	}
	b, ok := l.buckets[src]
	if !ok {
		if len(l.buckets) >= l.max {
			full := time.Duration(l.burst / l.rate * float64(time.Second))
			for a, old := range l.buckets {
				if now.Sub(old.last) >= full {
					delete(l.buckets, a)
				}
			}
			if len(l.buckets) >= l.max {
				return false
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[src] = b
	}
	return b.take(l.rate, l.burst, now)
}

// Listener reads packets from a UDP socket and hands valid announcements to
// a callback.
type Listener struct {
	conn      *net.UDPConn
	log       *logx.Logger
	handle    func(Announcement)
	limiter   *limiter
	now       func() time.Time
	validAddr func(netip.Addr) bool
}

// Limits: per source 1 packet a second with a burst of 5, all sources
// together 50 a second with a burst of 100, at most 512 sources tracked.
const (
	perSourceRate  = 1
	perSourceBurst = 5
	globalRate     = 50
	globalBurst    = 100
	maxSources     = 512
)

// NewListener wraps conn. handle is called for every accepted announcement,
// from the listener's goroutine.
func NewListener(conn *net.UDPConn, log *logx.Logger, handle func(Announcement)) *Listener {
	return &Listener{
		conn: conn, log: log, handle: handle,
		limiter:   newLimiter(perSourceRate, perSourceBurst, globalRate, globalBurst, maxSources),
		now:       time.Now,
		validAddr: ValidLANAddr,
	}
}

// Run reads until ctx is done or the socket fails.
func (l *Listener) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = l.conn.Close()
	}()
	buf := make([]byte, MaxPacket+1)
	for {
		n, src, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		l.Handle(src.Addr().Unmap(), buf[:n])
	}
}

// Handle processes one packet from src (exported for tests). The printer
// must announce its own address: Location must equal the sender, and the
// sender must be a private LAN address.
func (l *Listener) Handle(src netip.Addr, data []byte) {
	src = src.Unmap()
	if len(data) > MaxPacket || !l.validAddr(src) {
		return
	}
	if !l.limiter.allow(src, l.now()) {
		return
	}
	a, err := Parse(bytes.Clone(data))
	if err != nil {
		return
	}
	if a.IP != src {
		return
	}
	a.Source = src
	a.At = l.now()
	l.handle(a)
}

// Tail returns the last 4 characters of a serial.
func Tail(serial string) string {
	if len(serial) <= 4 {
		return serial
	}
	return serial[len(serial)-4:]
}

// Listen opens the discovery socket on addr ("0.0.0.0:2021" in the add-on).
// When multicast is true it also joins 239.255.255.250 on every interface
// that supports multicast. Joining sends an IGMP membership report, which is
// the kernel's normal group signalling, not a discovery packet.
func Listen(addr string, multicast bool, log *logx.Logger) (*net.UDPConn, error) {
	ua, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	if !multicast {
		return net.ListenUDP("udp4", ua)
	}
	group := &net.UDPAddr{IP: net.ParseIP(Group), Port: ua.Port}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		log.Warn("could not join the discovery group, listening for broadcasts only", "err", err)
		return net.ListenUDP("udp4", ua)
	}
	joinAll(conn, group.IP, log)
	return conn, nil
}

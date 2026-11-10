// Package printers keeps the list of printers seen on the network and runs
// one MQTT status session per printer that has an access code. It also
// uploads files to a printer's storage over FTPS (upload.go).
//
// Before an access code is sent (MQTT or FTPS), the printer's TLS certificate
// must chain to a bundled Bambu Lab CA with the serial as its common name, or
// match a certificate the owner chose to trust. The only MQTT messages ever
// sent are the constant pushall status request and, only with auto start on
// and only to a printer in Developer Mode, one checked project_file start.
package printers

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

// AccessState is what the link knows about a printer's access code.
type AccessState string

// Access states. Only missing, ok and wrong are sent to Spoken Parts.
const (
	AccessMissing   AccessState = "missing"
	AccessUnchecked AccessState = "unchecked"
	AccessOK        AccessState = "ok"
	AccessWrong     AccessState = "wrong"
)

// Username is the fixed MQTT user name on Bambu Lab printers.
const Username = "bblp"

// CodeSource gives read access to stored access codes.
type CodeSource interface {
	Get(serial string) (string, bool)
	Has(serial string) bool
	Locked() bool
}

// Config for a Manager. Zero values get defaults.
type Config struct {
	Bundle       *Bundle
	Codes        CodeSource
	MetaPath     string
	Log          *logx.Logger
	Port         int                                                               // 8883
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error) // test hook
	KeepAlive    time.Duration                                                     // 30 s
	Watchdog     time.Duration                                                     // 60 s
	PushAllEvery time.Duration                                                     // 5 min
	RetryMin     time.Duration                                                     // 5 s
	RetryMax     time.Duration                                                     // 5 min
	OnlineWindow time.Duration                                                     // 90 s
	MaxPrinters  int                                                               // 200
	FTPSPort     int                                                               // 990
	// AutoStart allows a project_file start (Developer Mode printers only).
	// Off by default (SP_LINK_AUTOSTART); our printers stay in Cloud mode.
	AutoStart bool
	Now       func() time.Time
}

// Discovery limits. A LAN host can send any announcement, so a printer's
// address may change at most once a minute, an address that flips back is
// held for 5 minutes, and printers nobody configured are forgotten.
const (
	ipChangeEvery = 60 * time.Second
	flapHold      = 5 * time.Minute
	ipLogEvery    = 10 * time.Minute
	staleAfter    = 24 * time.Hour
	idleEvict     = 10 * time.Minute
	spoofShowFor  = 24 * time.Hour
)

type printer struct {
	serial    string
	ip        netip.Addr
	modelCode string
	name      string
	firmware  string
	connect   string
	lastSeen  time.Time

	prevIP      netip.Addr // the address before the last change
	ipChangedAt time.Time
	holdUntil   time.Time // flapping: ignore address changes until then
	ipLogAt     time.Time
	lastGoodIP  netip.Addr // where the certificate last checked out
	spoofAt     time.Time  // last time a different certificate showed up

	connected   bool
	access      AccessState // unchecked, ok or wrong; only meaningful with a code
	cert        CertState
	certFP      string
	candidate   string
	certReason  string
	lastError   string
	gcodeState  string
	percent     *int
	remaining   *int
	devMode     *bool
	reportFW    string
	lastReport  time.Time
	lastPushAll time.Time
	live        live
	client      *mqtt.Client // the open MQTT session, while connected

	gen    uint64
	cancel context.CancelFunc
	done   chan struct{}
}

// Snapshot is a copy of one printer's state for the page and for sync.
type Snapshot struct {
	Serial          string
	IP              string
	ModelCode       string
	Model           string
	Name            string
	Firmware        string
	Online          bool
	Connected       bool
	Access          AccessState
	Cert            CertState
	CertFingerprint string
	Candidate       string
	CertReason      string
	LastError       string
	GcodeState      string
	Percent         *int
	RemainingMin    *int
	DevMode         *bool
	LanOnly         *bool
	LastSeen        time.Time
	LastReport      time.Time
	// Known is true when this serial passed the CA check before or the owner
	// trusted it. Such a printer is never offered for trust again.
	Known bool
	// Spoofed is true when, in the last day, something answered for this
	// printer with a certificate that did not check out.
	Spoofed bool

	// SPOK-LINK-V02: the job, storage, AMS and nozzles from the reports.
	Layer       *int
	TotalLayers *int
	Subtask     string
	GcodeFile   string
	PrintError  int
	HMS         []string
	HMSFatal    bool
	Storage     *bool
	Trays       []Tray
	Nozzles     []Nozzle
	Mapping     []int
	TrayNow     *int
	// Command is the printer's last answer to a command, CommandSeq counts them.
	Command    *CommandResult
	CommandSeq int
}

// CanStart says how a print starts on this printer: "auto" (Developer Mode
// and auto start on), "upload_only" (someone taps the file on the printer
// screen) or "unknown".
func (s Snapshot) CanStart(autoStart bool) string {
	switch {
	case s.DevMode == nil:
		return "unknown"
	case *s.DevMode && autoStart:
		return "auto"
	default:
		return "upload_only"
	}
}

// Busy reports whether the printer is running a print.
func (s Snapshot) Busy() bool {
	switch s.GcodeState {
	case "RUNNING", "PREPARE", "PAUSE", "SLICING":
		return true
	}
	return false
}

// SerialTail is the last 4 characters of the serial.
func (s Snapshot) SerialTail() string { return ssdp.Tail(s.Serial) }

// Manager owns all printer state and sessions.
type Manager struct {
	cfg  Config
	base context.Context

	mu       sync.Mutex
	printers map[string]*printer
	meta     map[string]store.PrinterMeta
	enabled  bool
	closed   bool
}

// New loads remembered pins and access results and returns a Manager.
// Sessions run under ctx.
func New(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Port == 0 {
		cfg.Port = 8883
	}
	if cfg.FTPSPort == 0 {
		cfg.FTPSPort = 990
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		cfg.Dial = d.DialContext
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.KeepAlive, 30*time.Second)
	def(&cfg.Watchdog, 60*time.Second)
	def(&cfg.PushAllEvery, 5*time.Minute)
	def(&cfg.RetryMin, 5*time.Second)
	def(&cfg.RetryMax, 5*time.Minute)
	def(&cfg.OnlineWindow, 90*time.Second)
	if cfg.MaxPrinters <= 0 {
		cfg.MaxPrinters = 200
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := &Manager{cfg: cfg, base: ctx, printers: map[string]*printer{}, meta: map[string]store.PrinterMeta{}}
	if cfg.MetaPath != "" {
		if err := store.LoadJSON(cfg.MetaPath, &m.meta); err != nil {
			cfg.Log.Warn("could not read saved printer settings, starting fresh", "err", err)
			m.meta = map[string]store.PrinterMeta{}
		}
	}
	return m, nil
}

// Observe records a discovery announcement. The serial is the identity;
// the address is only a hint, and changes to it are rate limited.
func (m *Manager) Observe(a ssdp.Announcement) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !a.IP.IsValid() {
		return
	}
	now := m.cfg.Now()
	p, ok := m.printers[a.Serial]
	if !ok {
		if len(m.printers) >= m.cfg.MaxPrinters {
			m.evictLocked(now, true)
		}
		if len(m.printers) >= m.cfg.MaxPrinters {
			m.cfg.Log.Debug("printer list is full, ignoring a new serial", "serial_tail", ssdp.Tail(a.Serial))
			return
		}
		p = &printer{serial: a.Serial, cert: CertUnknown, access: AccessUnchecked, ip: a.IP}
		switch m.meta[a.Serial].Access {
		case string(AccessOK):
			p.access = AccessOK
		case string(AccessWrong):
			p.access = AccessWrong
		}
		m.printers[a.Serial] = p
		m.cfg.Log.Info("found printer", "serial_tail", ssdp.Tail(a.Serial), "model", a.ModelCode, "ip", a.IP)
	}
	if a.IP != p.ip && !m.changeIPLocked(p, a.IP, now) {
		return // an address we do not (yet) believe: ignore the whole announcement
	}
	p.lastSeen = now
	if a.ModelCode != "" {
		p.modelCode = a.ModelCode
	}
	if a.Name != "" {
		p.name = a.Name
	}
	if a.Firmware != "" {
		p.firmware = a.Firmware
	}
	if a.Connect != "" {
		p.connect = a.Connect
	}
	if p.cancel == nil {
		m.restartLocked(p)
	}
}

// changeIPLocked decides whether to follow an announced address change.
// It returns true if p.ip is now ip.
func (m *Manager) changeIPLocked(p *printer, ip netip.Addr, now time.Time) bool {
	tail := ssdp.Tail(p.serial)
	switch {
	case p.connected && (p.cert == CertTrusted || p.cert == CertPinned):
		// A checked session is up at the current address: a printer that
		// really moved drops this session first.
		m.cfg.Log.Debug("ignored another address for a connected printer", "serial_tail", tail, "ip", ip)
		return false
	case now.Before(p.holdUntil):
		return false
	case !p.ipChangedAt.IsZero() && now.Sub(p.ipChangedAt) < ipChangeEvery:
		return false
	}
	if ip == p.prevIP && !p.ipChangedAt.IsZero() && now.Sub(p.ipChangedAt) < flapHold {
		// Back and forth: go back to the first address and hold it.
		p.holdUntil = now.Add(flapHold)
		m.cfg.Log.Warn("printer address keeps changing, holding the first one for 5 minutes", "serial_tail", tail, "ip", ip)
	} else if now.Sub(p.ipLogAt) >= ipLogEvery {
		p.ipLogAt = now
		m.cfg.Log.Info("printer address changed, reconnecting", "serial_tail", tail, "ip", ip)
	} else {
		m.cfg.Log.Debug("printer address changed, reconnecting", "serial_tail", tail, "ip", ip)
	}
	p.prevIP, p.ip, p.ipChangedAt = p.ip, ip, now
	// A new address may be a different device: check its certificate again.
	p.resetCert()
	m.restartLocked(p)
	return true
}

// forgettable reports whether nothing about p is worth keeping: no stored
// code, no pin, no verified certificate and no access result.
func (m *Manager) forgettableLocked(p *printer) bool {
	if m.cfg.Codes != nil && m.cfg.Codes.Has(p.serial) {
		return false
	}
	if p.cancel != nil && p.connected {
		return false
	}
	return m.meta[p.serial] == (store.PrinterMeta{})
}

// evictLocked forgets printers not seen for 24 hours that hold nothing.
// With room true it also frees one slot, if it can, by forgetting the
// longest unseen such printer that has been quiet for 10 minutes.
func (m *Manager) evictLocked(now time.Time, room bool) {
	var oldest *printer
	for serial, p := range m.printers {
		if !m.forgettableLocked(p) {
			continue
		}
		if now.Sub(p.lastSeen) >= staleAfter {
			m.stopLocked(p)
			delete(m.printers, serial)
			continue
		}
		if now.Sub(p.lastSeen) >= idleEvict && (oldest == nil || p.lastSeen.Before(oldest.lastSeen)) {
			oldest = p
		}
	}
	if room && len(m.printers) >= m.cfg.MaxPrinters && oldest != nil {
		m.stopLocked(oldest)
		delete(m.printers, oldest.serial)
	}
}

// Forget removes a printer and everything remembered about it (pin,
// verified certificate, access result). The caller deletes its stored
// access code. When it announces itself again it is treated as new.
func (m *Manager) Forget(serial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.printers[serial]
	_, hadMeta := m.meta[serial]
	if !ok && !hadMeta {
		return ErrUnknownPrinter
	}
	if ok {
		m.stopLocked(p)
		p.gen++ // any running session is now stale
		delete(m.printers, serial)
	}
	m.setMetaLocked(serial, store.PrinterMeta{})
	m.cfg.Log.Info("printer removed on the link page", "serial_tail", ssdp.Tail(serial))
	return nil
}

// SetEnabled starts (true) or stops (false) all sessions. The app enables
// the manager once the link is active and the access codes are open.
func (m *Manager) SetEnabled(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.enabled == on {
		return
	}
	m.enabled = on
	for _, p := range m.printers {
		if on {
			m.restartLocked(p)
		} else {
			m.stopLocked(p)
		}
	}
}

// CodeSaved is called after a new access code was stored for serial.
func (m *Manager) CodeSaved(serial string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.printers[serial]
	meta := m.meta[serial]
	meta.Access = ""
	m.setMetaLocked(serial, meta)
	if !ok {
		return
	}
	p.access = AccessUnchecked
	p.lastError = ""
	p.resetCert()
	m.restartLocked(p)
}

// resetCert forgets the last certificate verdict so it is checked again on
// the next connection. Pins are not touched.
func (p *printer) resetCert() {
	p.cert = CertUnknown
	p.candidate = ""
	p.certReason = ""
}

// ResetAccess forgets every access result (after the codes were wiped).
// Pins are kept: they describe printers, not the link.
func (m *Manager) ResetAccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for serial, meta := range m.meta {
		meta.Access = ""
		m.meta[serial] = meta
	}
	m.saveMetaLocked()
	for _, p := range m.printers {
		m.stopLocked(p)
		p.access = AccessUnchecked
	}
}

// Errors from Trust.
var (
	ErrUnknownPrinter = errors.New("unknown printer")
	ErrNothingToTrust = errors.New("this printer has no certificate waiting to be trusted")
)

// Trust pins the certificate the owner saw on the page. fp must equal the
// certificate the printer presented last, so the owner trusts exactly what
// was shown.
func (m *Manager) Trust(serial, fp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.printers[serial]
	if !ok {
		return ErrUnknownPrinter
	}
	if p.candidate == "" || p.candidate != fp || p.cert != CertUntrusted || m.knownLocked(serial) {
		return ErrNothingToTrust
	}
	meta := m.meta[serial]
	meta.Pin = fp
	m.setMetaLocked(serial, meta)
	p.candidate = ""
	p.cert = CertUnknown
	p.certReason = ""
	m.cfg.Log.Info("owner trusted printer certificate", "serial_tail", ssdp.Tail(serial), "fingerprint", fp)
	m.restartLocked(p)
	return nil
}

// knownLocked reports whether serial ever passed the CA check or was pinned.
func (m *Manager) knownLocked(serial string) bool {
	meta := m.meta[serial]
	return meta.Verified != "" || meta.Pin != ""
}

// Has reports whether serial has been seen.
func (m *Manager) Has(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.printers[serial]
	return ok
}

// Snapshot returns all printers sorted by serial.
func (m *Manager) Snapshot() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	m.evictLocked(now, false)
	locked := m.cfg.Codes == nil || m.cfg.Codes.Locked()
	out := make([]Snapshot, 0, len(m.printers))
	for _, p := range m.printers {
		s := Snapshot{
			Serial:          p.serial,
			ModelCode:       p.modelCode,
			Model:           ssdp.ModelName(p.modelCode),
			Name:            p.name,
			Firmware:        p.firmware,
			Connected:       p.connected,
			Online:          p.connected || (!p.lastSeen.IsZero() && now.Sub(p.lastSeen) < m.cfg.OnlineWindow),
			Cert:            p.cert,
			CertFingerprint: p.certFP,
			Candidate:       p.candidate,
			CertReason:      p.certReason,
			LastError:       p.lastError,
			GcodeState:      p.gcodeState,
			Percent:         cloneInt(p.percent),
			RemainingMin:    cloneInt(p.remaining),
			DevMode:         cloneBool(p.devMode),
			LastSeen:        p.lastSeen,
			LastReport:      p.lastReport,
			Known:           m.knownLocked(p.serial),
			Spoofed:         !p.spoofAt.IsZero() && now.Sub(p.spoofAt) < spoofShowFor,
		}
		if p.ip.IsValid() {
			s.IP = p.ip.String()
		}
		if s.Model == "" {
			s.Model = p.modelCode
		}
		if p.reportFW != "" {
			s.Firmware = p.reportFW
		}
		l := &p.live
		s.Layer, s.TotalLayers = cloneInt(l.layer), cloneInt(l.total)
		s.Subtask, s.GcodeFile, s.PrintError = l.subtask, l.gcodeFile, l.printError
		s.HMS, s.HMSFatal = append([]string(nil), l.hms...), l.hmsFatal
		s.Storage, s.TrayNow = cloneBool(l.storage), cloneInt(l.trayNow)
		s.Mapping = append([]int(nil), l.mapping...)
		s.Trays, s.Nozzles = l.snapshot()
		if l.command != nil {
			c := *l.command
			s.Command, s.CommandSeq = &c, l.commandSeq
		}
		switch p.connect {
		case "lan":
			s.LanOnly = boolPtr(true)
		case "cloud":
			s.LanOnly = boolPtr(false)
		}
		switch {
		case locked:
			switch m.meta[p.serial].Access {
			case string(AccessOK):
				s.Access = AccessOK
			case string(AccessWrong):
				s.Access = AccessWrong
			default:
				s.Access = AccessMissing
			}
		case !m.cfg.Codes.Has(p.serial):
			s.Access = AccessMissing
		default:
			s.Access = p.access
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// Close stops every session and waits for them to end.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var waits []chan struct{}
	for _, p := range m.printers {
		m.stopLocked(p)
		if p.done != nil {
			waits = append(waits, p.done)
		}
	}
	m.mu.Unlock()
	for _, w := range waits {
		<-w
	}
}

// ---- sessions ----

func (m *Manager) shouldRunLocked(p *printer) bool {
	if !m.enabled || m.closed || !p.ip.IsValid() {
		return false
	}
	if p.cert == CertUntrusted {
		return false // waiting for the owner to trust it
	}
	if p.access == AccessWrong {
		return false // waiting for a new code
	}
	return m.cfg.Codes != nil && m.cfg.Codes.Has(p.serial)
}

func (m *Manager) stopLocked(p *printer) {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.connected = false
	p.client = nil
}

// restartLocked stops any session and starts a new one if it should run.
// The new session waits for the old one to end, so a printer never has two.
func (m *Manager) restartLocked(p *printer) {
	m.stopLocked(p)
	if !m.shouldRunLocked(p) {
		return
	}
	p.gen++
	gen := p.gen
	ctx, cancel := context.WithCancel(m.base)
	prev := p.done
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		m.session(ctx, p.serial, gen)
	}()
}

type outcome int

const (
	outcomeRetry   outcome = iota // failed before or during connect
	outcomeDropped                // was connected, then lost
	outcomeWait                   // needs the owner (untrusted or wrong code)
)

func (m *Manager) session(ctx context.Context, serial string, gen uint64) {
	attempt := 0
	for ctx.Err() == nil {
		var wait time.Duration
		switch m.connectOnce(ctx, serial, gen) {
		case outcomeWait:
			return
		case outcomeDropped:
			attempt = 0
			wait = api.Backoff(1, m.cfg.RetryMin, m.cfg.RetryMax)
		default:
			attempt++
			wait = api.Backoff(attempt, m.cfg.RetryMin, m.cfg.RetryMax)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// with runs f on the printer if this session is still the current one.
func (m *Manager) with(serial string, gen uint64, f func(p *printer)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.printers[serial]
	if !ok || p.gen != gen || p.cancel == nil {
		return false
	}
	f(p)
	return true
}

var errUntrusted = errors.New("printer certificate not trusted")

// tlsCiphers are the TLS 1.2 suites offered to printers. Bambu printers
// speak TLS 1.2 only. The RSA key exchange suites are kept as a last resort
// for older printer firmware.
var tlsCiphers = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
}

func (m *Manager) connectOnce(ctx context.Context, serial string, gen uint64) outcome {
	var ip netip.Addr
	var pin string
	var known bool
	if !m.with(serial, gen, func(p *printer) {
		ip, pin, known = p.ip, m.meta[serial].Pin, m.meta[serial].Verified != ""
	}) {
		return outcomeWait
	}
	tail := ssdp.Tail(serial)
	setErr := func(msg string) {
		m.with(serial, gen, func(p *printer) { p.lastError = msg; p.connected = false })
	}

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err := m.cfg.Dial(dctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(m.cfg.Port)))
	cancel()
	if err != nil {
		setErr("cant_reach")
		m.cfg.Log.Debug("printer not reachable", "serial_tail", tail, "err", err)
		return outcomeRetry
	}

	var v verdict
	cfg := &tls.Config{
		ServerName:         serial,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites:       tlsCiphers,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			v = m.cfg.Bundle.verify(cs.PeerCertificates, serial, pin, known, m.cfg.Now())
			if v.state == CertTrusted || v.state == CertPinned {
				return nil
			}
			return errUntrusted
		},
	}
	tc := tls.Client(raw, cfg)
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	err = tc.HandshakeContext(hctx)
	hcancel()
	if v.state != "" {
		m.with(serial, gen, func(p *printer) {
			p.cert, p.certFP, p.certReason = v.state, v.fp, v.reason
			p.candidate = ""
			switch v.state {
			case CertUntrusted:
				// Never checked out and never pinned: the owner may trust it.
				p.candidate = v.fp
			case CertChanged:
				p.spoofAt = m.cfg.Now()
				if p.lastGoodIP.IsValid() && p.lastGoodIP != p.ip {
					// Go back to where the real printer answered, and hold it.
					now := m.cfg.Now()
					p.prevIP, p.ip, p.ipChangedAt, p.holdUntil = p.ip, p.lastGoodIP, now, now.Add(flapHold)
				}
			case CertTrusted, CertPinned:
				p.lastGoodIP = p.ip
				if v.state == CertTrusted {
					meta := m.meta[serial]
					if meta.Verified != v.fp {
						meta.Verified, meta.VerifiedAt = v.fp, m.cfg.Now().UTC()
						m.setMetaLocked(serial, meta)
					}
				}
			}
		})
	}
	switch v.state {
	case CertUntrusted:
		_ = raw.Close()
		setErr("")
		m.cfg.Log.Warn("printer certificate not trusted, access code not sent", "serial_tail", tail, "reason", v.reason, "fingerprint", v.fp)
		return outcomeWait // until the owner decides
	case CertChanged:
		// Never offered for trust. Keep checking with backoff: the session
		// goes back to the last good address, or the real printer comes
		// back on this one. Nothing is sent unless the check passes.
		_ = raw.Close()
		setErr("")
		m.cfg.Log.Warn("a different certificate answered for a known printer, access code not sent", "serial_tail", tail, "ip", ip, "fingerprint", v.fp)
		return outcomeRetry
	case CertClock:
		_ = raw.Close()
		setErr("")
		m.cfg.Log.Warn("printer certificate dates do not match this device's clock, trying again later", "serial_tail", tail)
		return outcomeRetry
	}
	if err != nil {
		_ = raw.Close()
		setErr("tls")
		m.cfg.Log.Debug("printer TLS handshake failed", "serial_tail", tail, "err", err)
		return outcomeRetry
	}

	code, ok := m.cfg.Codes.Get(serial)
	if !ok {
		_ = tc.Close()
		return outcomeWait
	}
	client, err := mqtt.Connect(tc, serial, mqtt.Options{
		Username:  Username,
		Password:  code,
		KeepAlive: m.cfg.KeepAlive,
		Watchdog:  m.cfg.Watchdog,
	})
	code = ""
	var ce *mqtt.ConnackError
	if errors.As(err, &ce) && ce.BadCredentials() {
		_ = tc.Close()
		m.with(serial, gen, func(p *printer) {
			p.access = AccessWrong
			p.lastError = ""
			meta := m.meta[serial]
			meta.Access = string(AccessWrong)
			m.setMetaLocked(serial, meta)
		})
		m.cfg.Log.Warn("printer rejected the access code", "serial_tail", tail)
		return outcomeWait
	}
	if err != nil {
		_ = tc.Close()
		setErr("mqtt")
		m.cfg.Log.Debug("printer MQTT connect failed", "serial_tail", tail, "err", err)
		return outcomeRetry
	}

	doPush := false
	m.with(serial, gen, func(p *printer) {
		p.connected = true
		p.lastError = ""
		if p.access != AccessOK {
			p.access = AccessOK
			meta := m.meta[serial]
			meta.Access = string(AccessOK)
			m.setMetaLocked(serial, meta)
		}
		now := m.cfg.Now()
		if p.lastPushAll.IsZero() || now.Sub(p.lastPushAll) >= m.cfg.PushAllEvery {
			p.lastPushAll = now
			doPush = true
		}
	})
	m.with(serial, gen, func(p *printer) { p.client = client })
	m.cfg.Log.Info("connected to printer", "serial_tail", tail)
	if doPush {
		if err := client.RequestPushAll(); err != nil {
			m.cfg.Log.Debug("pushall request failed", "serial_tail", tail, "err", err)
		}
	}
	err = client.Run(ctx, func(payload []byte) { m.applyReport(serial, gen, payload) })
	client.Close()
	m.with(serial, gen, func(p *printer) { p.connected, p.client = false, nil })
	if ctx.Err() == nil {
		m.cfg.Log.Info("printer connection closed", "serial_tail", tail, "err", err)
	}
	return outcomeDropped
}

func (m *Manager) applyReport(serial string, gen uint64, payload []byte) {
	u, ok := parseReport(payload)
	if !ok {
		return
	}
	lu, _ := parseLive(payload)
	m.with(serial, gen, func(p *printer) {
		p.live.apply(lu)
		p.lastReport = m.cfg.Now()
		if u.gcodeState != nil {
			p.gcodeState = *u.gcodeState
		}
		if u.percent != nil {
			p.percent = u.percent
		}
		if u.remaining != nil {
			p.remaining = u.remaining
		}
		if u.devKnown {
			p.devMode = u.devMode
		}
		if u.firmware != "" {
			p.reportFW = u.firmware
		}
	})
}

func (m *Manager) setMetaLocked(serial string, meta store.PrinterMeta) {
	if meta == (store.PrinterMeta{}) {
		delete(m.meta, serial)
	} else {
		m.meta[serial] = meta
	}
	m.saveMetaLocked()
}

func (m *Manager) saveMetaLocked() {
	if m.cfg.MetaPath == "" {
		return
	}
	if err := store.SaveJSON(m.cfg.MetaPath, m.meta); err != nil {
		m.cfg.Log.Warn("could not save printer settings", "err", err)
	}
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func boolPtr(b bool) *bool { return &b }

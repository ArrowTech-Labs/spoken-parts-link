package printers

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ftps"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
)

// Upload problems, mapped to the codes Spoken Parts shows in plain words.
var (
	ErrNotReady    = errors.New("printer is not connected with a checked certificate and a working access code")
	ErrNoStorage   = errors.New("printer has no USB stick or SD card")
	ErrRefused     = errors.New("printer refused the file (553)")
	ErrAccessCode  = errors.New("printer refused the access code")
	ErrCertificate = errors.New("printer certificate did not check out for the upload")
	ErrCantReach   = errors.New("printer not reachable for the upload")
	ErrAutoStart   = errors.New("auto start is off, or the printer is not in Developer Mode")
)

// FileNameRe is the only kind of name the link puts on (and deletes from) a
// printer's storage: SP<printer>-<ref>-<plate>-<id>.gcode.3mf.
var FileNameRe = regexp.MustCompile(`^SP[0-9]{1,5}-[A-Za-z0-9-]{1,30}\.gcode\.3mf$`)

type target struct {
	ip     netip.Addr
	pin    string
	known  bool
	certFP string
	code   string
}

// target returns what an upload needs, only for a printer whose MQTT session
// already checked its certificate and access code.
func (m *Manager) target(serial string) (target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.printers[serial]
	if !ok || !p.ip.IsValid() || p.access != AccessOK || (p.cert != CertTrusted && p.cert != CertPinned) || m.cfg.Codes == nil {
		return target{}, ErrNotReady
	}
	code, ok := m.cfg.Codes.Get(serial)
	if !ok {
		return target{}, ErrNotReady
	}
	meta := m.meta[serial]
	return target{ip: p.ip, pin: meta.Pin, known: meta.Verified != "", certFP: p.certFP, code: code}, nil
}

// ftpSession opens a logged in FTPS session. The certificate must pass the same
// check as for MQTT, or be the very certificate the MQTT session checked.
func (m *Manager) ftpSession(ctx context.Context, serial string) (*ftps.Client, error) {
	t, err := m.target(serial)
	if err != nil {
		return nil, err
	}
	var bad bool
	cfg := &tls.Config{
		ServerName:         serial,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12, // printers' FTPS speaks TLS 1.2
		CipherSuites:       tlsCiphers,
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			v := m.cfg.Bundle.verify(cs.PeerCertificates, serial, t.pin, t.known, m.cfg.Now())
			if v.state == CertTrusted || v.state == CertPinned || (v.fp != "" && v.fp == t.certFP) {
				return nil
			}
			bad = true
			return errUntrusted
		},
	}
	addr := net.JoinHostPort(t.ip.String(), strconv.Itoa(m.cfg.FTPSPort))
	c, err := ftps.Dial(ctx, addr, cfg, m.cfg.Dial, 30*time.Second)
	if err != nil {
		if bad {
			return nil, ErrCertificate
		}
		return nil, ErrCantReach
	}
	err = c.Login(Username, t.code)
	t.code = ""
	if err != nil {
		c.Quit()
		if ftps.Code(err) == 530 {
			return nil, ErrAccessCode
		}
		return nil, ErrCantReach
	}
	return c, nil
}

// Upload stores size bytes from r on the printer as name (FileNameRe).
func (m *Manager) Upload(ctx context.Context, serial, name string, r io.Reader, size int64, progress func(int64)) error {
	if !FileNameRe.MatchString(name) {
		return errors.New("bad file name")
	}
	if snap, ok := m.Get(serial); ok && snap.Storage != nil && !*snap.Storage {
		return ErrNoStorage
	}
	c, err := m.ftpSession(ctx, serial)
	if err != nil {
		return err
	}
	defer c.Quit()
	err = c.Store(ctx, "/"+name, r, size, progress)
	if ftps.Code(err) == 553 {
		return ErrRefused
	}
	if err != nil {
		m.cfg.Log.Info("upload to printer failed", "serial_tail", ssdp.Tail(serial), "err", err)
		return err
	}
	m.cfg.Log.Info("uploaded a file to the printer", "serial_tail", ssdp.Tail(serial), "file", name, "bytes", size)
	return nil
}

// Delete removes a file the link uploaded (FileNameRe only).
func (m *Manager) Delete(ctx context.Context, serial, name string) error {
	if !FileNameRe.MatchString(name) {
		return errors.New("bad file name")
	}
	c, err := m.ftpSession(ctx, serial)
	if err != nil {
		return err
	}
	defer c.Quit()
	return c.Delete("/" + name)
}

// Get returns one printer's snapshot.
func (m *Manager) Get(serial string) (Snapshot, bool) {
	for _, s := range m.Snapshot() {
		if strings.EqualFold(s.Serial, serial) {
			return s, true
		}
	}
	return Snapshot{}, false
}

// AutoStart reports whether auto start is on for this link.
func (m *Manager) AutoStart() bool { return m.cfg.AutoStart }

// StartProjectFile starts a file already on the printer's storage. Only with
// auto start on, only for a printer that reports Developer Mode, and only
// over the checked MQTT session. Never for a Cloud mode printer.
func (m *Manager) StartProjectFile(serial string, pf mqtt.ProjectFile) error {
	m.mu.Lock()
	p, ok := m.printers[serial]
	var client *mqtt.Client
	dev := false
	if ok {
		client = p.client
		dev = p.devMode != nil && *p.devMode
		p.live.commandSeq++ // a new answer is needed
		p.live.command = nil
	}
	m.mu.Unlock()
	if !m.cfg.AutoStart || !dev {
		return ErrAutoStart
	}
	if client == nil {
		return ErrNotReady
	}
	err := client.StartProjectFile(pf, int(time.Now().Unix()%1_000_000))
	m.cfg.Log.Info("sent a project_file start", "serial_tail", ssdp.Tail(serial), "err", err)
	return err
}

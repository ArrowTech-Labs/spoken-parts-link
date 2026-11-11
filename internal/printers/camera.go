package printers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
)

// Feeds lists the printers with a live or wanted MQTT session, with a copy
// of everything they reported, for the monitoring rules.
func (m *Manager) Feeds() []monitor.PrinterFeed {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []monitor.PrinterFeed
	for _, p := range m.printers {
		if !m.shouldRunLocked(p) && !p.connected {
			continue
		}
		f := monitor.PrinterFeed{Serial: p.serial, Name: p.name, Connected: p.connected, LastReport: p.lastReport}
		if f.Name == "" {
			f.Name = ssdp.ModelName(p.modelCode)
		}
		if p.merged != nil {
			f.Report = make(map[string]json.RawMessage, len(p.merged))
			for k, v := range p.merged {
				f.Report[k] = v
			}
		}
		out = append(out, f)
	}
	return out
}

// Camera errors.
var (
	ErrNoCamera   = errors.New("printer is not connected, so its camera is not used")
	ErrCameraCert = errors.New("camera certificate did not check out, access code not sent")
)

// WithCamera opens a TLS connection to the printer's camera (RTSPS, port
// 322) with the same certificate check as MQTT, and only then calls f with
// the connection, the camera's address (ip:port, as in the RTSP URL) and
// the access code. The code goes
// nowhere else; f must not keep it. The connection is closed when f returns.
//
// The camera is only used while the MQTT session is up with a checked or
// trusted certificate, so it is the printer we already talk to.
func (m *Manager) WithCamera(ctx context.Context, serial string, f func(conn net.Conn, host, code string) error) error {
	var ip netip.Addr
	var pin string
	var known bool
	m.mu.Lock()
	p, ok := m.printers[serial]
	if ok {
		ip, pin, known = p.ip, m.meta[serial].Pin, m.meta[serial].Verified != ""
		ok = p.connected && (p.cert == CertTrusted || p.cert == CertPinned) && ip.IsValid()
	}
	m.mu.Unlock()
	if !ok {
		return ErrNoCamera
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err := m.cfg.Dial(dctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(m.cfg.CameraPort)))
	cancel()
	if err != nil {
		return errors.New("camera not reachable")
	}
	var v verdict
	tc := tls.Client(raw, &tls.Config{
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
	})
	defer tc.Close()
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	err = tc.HandshakeContext(hctx)
	hcancel()
	if v.state != CertTrusted && v.state != CertPinned {
		m.cfg.Log.Warn("printer camera certificate did not check out, access code not sent", "serial_tail", ssdp.Tail(serial), "fingerprint", v.fp)
		return ErrCameraCert
	}
	if err != nil {
		return errors.New("camera TLS handshake failed")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = tc.SetDeadline(dl)
	}
	code, ok := m.cfg.Codes.Get(serial)
	if !ok {
		return ErrNoCamera
	}
	err = f(tc, net.JoinHostPort(ip.String(), strconv.Itoa(m.cfg.CameraPort)), code)
	code = ""
	return err
}

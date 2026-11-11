// Package hanotify sends phone notifications through one Home Assistant
// automation (plan section 8, option B): the owner makes an automation with a
// webhook trigger (local only, a long random id) that calls
// notify.mobile_app_<phone>. The link posts to that webhook on the same host.
// It needs no Home Assistant permission and can trigger nothing else.
//
// The webhook id is a secret (anyone who knows it can make the phone buzz),
// so it is kept like the access codes: in an encrypted file whose key needs
// the data key from Spoken Parts, and it is never logged or shown again.
//
// 0.3.1: Home Assistant may serve only HTTPS on 8123 (http: ssl_certificate).
// With no ha_url option the link tries http://127.0.0.1:8123 first and, when
// that fails before Home Assistant answers, https://127.0.0.1:8123 without a
// certificate check (Home Assistant's certificate never names 127.0.0.1). The
// check is skipped only for loopback addresses, and the client that skips it
// refuses to connect anywhere else. The address that worked is kept for the
// life of the process.
package hanotify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

// DefaultHost is Home Assistant on this host (the app uses host networking).
const DefaultHost = "127.0.0.1:8123"

// Keys in the encrypted file.
const (
	keyWebhook = "webhook_id"
	keyMin     = "min_severity"
)

// WebhookRe is what a webhook id may look like: long and random.
var WebhookRe = regexp.MustCompile(`^[A-Za-z0-9_-]{24,128}$`)

// Errors.
var (
	ErrBadID  = errors.New("webhook id must be 24 to 128 letters, digits, - or _")
	ErrNotSet = errors.New("no Home Assistant webhook is set up")
	// ErrBadURL never quotes the option: a pasted webhook address holds the id.
	ErrBadURL      = errors.New("ha_url must be http:// or https:// with a host and an optional port, and nothing after it")
	errNotLoopback = errors.New("refused: certificate checks are skipped only for this host")
)

// Notifier is the Home Assistant webhook sink.
type Notifier struct {
	fixed    string // from the ha_url option; "" means automatic
	autoHost string // host:port tried over http, then https
	store    *store.Codes
	red      *redact.Redactor
	log      *logx.Logger
	verify   *http.Client // normal certificate checks
	loopback *http.Client // https to 127.0.0.0/8 or ::1 only, certificate not checked

	mu      sync.Mutex
	learned string // base that reached Home Assistant (automatic mode)
}

// New returns a notifier keeping its settings in st (locked until Unlock).
// base is the ha_url option: empty for automatic, else http(s)://host[:port].
// A bad base is logged (without its value) and automatic is used.
func New(base string, st *store.Codes, red *redact.Redactor, lg *logx.Logger) *Notifier {
	n := &Notifier{
		autoHost: DefaultHost,
		store:    st,
		red:      red,
		log:      lg,
		verify:   newClient(&tls.Config{MinVersion: tls.VersionTLS12}, nil),
		loopback: newClient(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, loopbackOnly), // loopback only, enforced at dial
	}
	fixed, err := ParseBase(base)
	switch {
	case err != nil:
		lg.Warn("ha_url option ignored, using Home Assistant on this host", "err", err)
	case fixed != "":
		n.fixed = fixed
		lg.Info("phone alerts use the ha_url option", "url", fixed, "cert_check", certCheck(fixed))
	}
	return n
}

func newClient(tc *tls.Config, control func(network, address string, c syscall.RawConn) error) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil, // never through a proxy: the id is in the path
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, Control: control}).DialContext,
			TLSClientConfig:     tc,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// loopbackOnly is the dial guard of the client that skips certificate
// checks: whatever the URL said, it connects only to a loopback address.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !ap.Addr().Unmap().IsLoopback() {
		return errNotLoopback
	}
	return nil
}

// ParseBase checks the ha_url option and returns scheme://host[:port], or ""
// for automatic. The error never contains the value.
func ParseBase(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", ErrBadURL
	}
	return u.Scheme + "://" + u.Host, nil
}

// isLoopback reports whether host (no port) is an address in 127.0.0.0/8 or
// ::1. Names, even "localhost", are not: only literal addresses count.
func isLoopback(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// skipsVerify reports whether base is https to a loopback address, the only
// case where the certificate is not checked.
func skipsVerify(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && isLoopback(u.Hostname())
}

func certCheck(base string) string {
	switch {
	case strings.HasPrefix(base, "http://"):
		return "none (http)"
	case skipsVerify(base):
		return "off (loopback)"
	}
	return "on"
}

func (n *Notifier) client(base string) *http.Client {
	if skipsVerify(base) {
		return n.loopback
	}
	return n.verify
}

// bases are the addresses to try, in order.
func (n *Notifier) bases() []string {
	if n.fixed != "" {
		return []string{n.fixed}
	}
	b := []string{"http://" + n.autoHost, "https://" + n.autoHost}
	n.mu.Lock()
	if n.learned == b[1] {
		b[0], b[1] = b[1], b[0]
	}
	n.mu.Unlock()
	return b
}

func (n *Notifier) remember(base string) {
	if n.fixed != "" {
		return
	}
	n.mu.Lock()
	changed := n.learned != base
	n.learned = base
	n.mu.Unlock()
	if changed {
		n.log.Info("Home Assistant reached", "url", base, "cert_check", certCheck(base))
	}
}

// Unlock opens the settings file with key (from the data key).
func (n *Notifier) Unlock(key []byte) error {
	err := n.store.Unlock(key)
	if id, ok := n.store.Get(keyWebhook); ok {
		n.red.Add(id)
	}
	return err
}

// Ready implements monitor.Notifier.
func (n *Notifier) Ready() (bool, monitor.Severity) {
	if !n.store.Has(keyWebhook) {
		return false, monitor.Critical
	}
	return true, n.MinSeverity()
}

// Configured reports whether a webhook id is stored.
func (n *Notifier) Configured() bool { return n.store.Has(keyWebhook) }

// MinSeverity is the lowest severity sent to the phone (warning by default).
func (n *Notifier) MinSeverity() monitor.Severity {
	if v, ok := n.store.Get(keyMin); ok && monitor.Severity(v).Rank() >= 0 {
		return monitor.Severity(v)
	}
	return monitor.Warning
}

// Set stores the webhook id and the lowest severity to send.
func (n *Notifier) Set(id string, minSev monitor.Severity) error {
	if !WebhookRe.MatchString(id) {
		return ErrBadID
	}
	if minSev.Rank() < 0 {
		minSev = monitor.Warning
	}
	n.red.Add(id)
	if err := n.store.Set(keyWebhook, id); err != nil {
		return err
	}
	return n.store.Set(keyMin, string(minSev))
}

// SetMin changes only the lowest severity.
func (n *Notifier) SetMin(minSev monitor.Severity) error {
	if !n.Configured() {
		return ErrNotSet
	}
	if minSev.Rank() < 0 {
		minSev = monitor.Warning
	}
	return n.store.Set(keyMin, string(minSev))
}

// Clear deletes the webhook id and the severity choice.
func (n *Notifier) Clear() error {
	if err := n.store.Delete(keyWebhook); err != nil {
		return err
	}
	return n.store.Delete(keyMin)
}

// Lock forgets the key (the link was removed or the codes wiped).
func (n *Notifier) Lock() { n.store.Lock() }

// Wipe deletes the file.
func (n *Notifier) Wipe() error { return n.store.Wipe() }

// payload is what the automation gets as trigger.json.
type payload struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Key      string `json:"key"`
	Printer  string `json:"printer"`
	ImageURL string `json:"image_url,omitempty"`
}

// Notify implements monitor.Notifier.
func (n *Notifier) Notify(ctx context.Context, x monitor.Notice) error {
	id, ok := n.store.Get(keyWebhook)
	if !ok {
		return ErrNotSet
	}
	body, err := json.Marshal(payload(x))
	if err != nil {
		return err
	}
	bases := n.bases()
	var failed []string
	for i, base := range bases {
		last := i == len(bases)-1
		code, err := n.post(ctx, base, id, body)
		if err != nil {
			// Refused, reset, TLS on a plain port and so on: Home Assistant
			// did not take the request, so the next address may be tried.
			failed = append(failed, base+": "+err.Error())
			if ctx.Err() != nil {
				break
			}
			continue
		}
		// Some servers answer plain HTTP on their HTTPS port with 400.
		if code == http.StatusBadRequest && strings.HasPrefix(base, "http://") && !last {
			failed = append(failed, base+": answered 400")
			continue
		}
		n.remember(base)
		if code < 200 || code > 299 {
			return fmt.Errorf("Home Assistant answered %d", code)
		}
		return nil
	}
	return fmt.Errorf("could not reach Home Assistant (%s)", strings.Join(failed, "; "))
}

// post sends one request. Errors never name the URL (it holds the id).
func (n *Notifier) post(ctx context.Context, base, id string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/webhook/"+id, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("could not build the request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client(base).Do(req)
	if err != nil {
		// url.Error names the URL; keep only the cause, redacted again.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		msg := n.red.Redact(err.Error())
		if id != "" {
			msg = strings.ReplaceAll(msg, id, "[redacted]")
		}
		return 0, errors.New(msg)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

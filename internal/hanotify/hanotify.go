// Package hanotify sends phone notifications through one Home Assistant
// automation (plan section 8, option B): the owner makes an automation with a
// webhook trigger (local only, a long random id) that calls
// notify.mobile_app_<phone>. The link posts to that webhook on the same host.
// It needs no Home Assistant permission and can trigger nothing else.
//
// The webhook id is a secret (anyone who knows it can make the phone buzz),
// so it is kept like the access codes: in an encrypted file whose key needs
// the data key from Spoken Parts, and it is never logged or shown again.
package hanotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

// DefaultBase is Home Assistant on this host (the app uses host networking).
const DefaultBase = "http://127.0.0.1:8123"

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
)

// Notifier is the Home Assistant webhook sink.
type Notifier struct {
	base  string
	store *store.Codes
	red   *redact.Redactor
	http  *http.Client
}

// New returns a notifier keeping its settings in st (locked until Unlock).
func New(base string, st *store.Codes, red *redact.Redactor) *Notifier {
	if base == "" {
		base = DefaultBase
	}
	return &Notifier{
		base:  base,
		store: st,
		red:   red,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:       nil, // never through a proxy: the id is in the path
				DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+"/api/webhook/"+id, bytes.NewReader(body))
	id = ""
	if err != nil {
		return errors.New("could not build the Home Assistant request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		// The error would name the URL, and the URL holds the id.
		return errors.New("could not reach Home Assistant")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Home Assistant answered %d", resp.StatusCode)
	}
	return nil
}

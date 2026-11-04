// Package api is the client for the Spoken Parts link API (/api/link/v1).
// Every request is signed with the link's Ed25519 key (CONTRACT 1.1).
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/signer"
)

// DefaultServer is the production backend.
const DefaultServer = "https://spokenparts.com"

// Prefix is the API path prefix.
const Prefix = "/api/link/v1/"

// Limits.
const (
	maxResponse  = 1 << 20
	maxRetryWait = 10 * time.Minute
)

// Error codes from the contract that the link reacts to.
const (
	CodeLinkRevoked = "link_revoked"
	CodeLinkPending = "link_pending"
	CodeLinksOff    = "links_off"
	CodeThrottled   = "throttled"
	CodeBadCode     = "bad_code"
	CodeKeyInUse    = "key_in_use"
	CodeInvalid     = "invalid"
	CodeUnknownLink = "unknown_link"
)

// Error is a non 2xx answer.
type Error struct {
	Status     int
	Code       string
	Detail     string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("spoken parts answered %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("spoken parts answered %d", e.Status)
}

// HasCode reports whether err is an *Error with the given code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// RetryAfter returns the server's Retry-After for err, or 0.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// ParseServer validates a base URL. It must be https, except that plain http
// is allowed for localhost, 127.0.0.1 and ::1 (tests and development).
func ParseServer(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil {
		return nil, fmt.Errorf("server URL: %w", err)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("server URL must be scheme://host[:port] with no user, query or fragment")
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && local:
	default:
		return nil, errors.New("server URL must use https (http only for localhost)")
	}
	return u, nil
}

// Client talks to the backend.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
	now       func() time.Time
}

// New returns a client for base (already validated with ParseServer).
func New(base *url.URL, version string) *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          4,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		base: base,
		http: &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
			// The signature covers the path, and a redirect could leave
			// https. Never follow one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		userAgent: "spoken-parts-link/" + version,
		now:       time.Now,
	}
}

// SetHTTPClient replaces the HTTP client (tests).
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// Site and Company appear in status and sync answers.
type Site struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Company is the owner of the site.
type Company struct {
	Name string `json:"name"`
}

// PairRequest is the body of POST pair.
type PairRequest struct {
	Code      string `json:"code"`
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
}

// PairResponse is the 201 answer of POST pair.
type PairResponse struct {
	LinkID      string `json:"link_id"`
	Status      string `json:"status"`
	CheckNumber string `json:"check_number"`
	ExpiresAt   string `json:"expires_at"`
}

// PairStatus is the answer of GET pair/status.
type PairStatus struct {
	Status      string   `json:"status"`
	CheckNumber string   `json:"check_number"`
	Site        *Site    `json:"site"`
	Company     *Company `json:"company"`
}

// SyncPrinter is one printer in a sync request (CONTRACT 1.4).
type SyncPrinter struct {
	Serial          string `json:"serial"`
	ModelCode       string `json:"model_code"`
	Name            string `json:"name"`
	IP              string `json:"ip"`
	Firmware        string `json:"firmware"`
	Online          bool   `json:"online"`
	AccessCode      string `json:"access_code"` // "missing" | "ok" | "wrong" (a state, never the code)
	DevMode         *bool  `json:"dev_mode"`
	LanOnly         *bool  `json:"lan_only"`
	GcodeState      string `json:"gcode_state"`
	Percent         *int   `json:"percent"`
	RemainingMin    *int   `json:"remaining_min"`
	CertFingerprint string `json:"cert_fingerprint"`
}

// SyncRequest is the body of POST sync.
type SyncRequest struct {
	Version  string        `json:"version"`
	UptimeS  int64         `json:"uptime_s"`
	Printers []SyncPrinter `json:"printers"`
}

// SyncResponse is the answer of POST sync.
type SyncResponse struct {
	NextSyncS int               `json:"next_sync_s"`
	Paused    bool              `json:"paused"`
	Status    string            `json:"status"`
	Site      *Site             `json:"site"`
	Company   *Company          `json:"company"`
	Sends     []json.RawMessage `json:"sends"`
	Cancel    []json.RawMessage `json:"cancel"`
	Commands  []json.RawMessage `json:"commands"`
}

// Pair calls POST pair. It is signed with key but carries no X-SP-Link.
func (c *Client) Pair(ctx context.Context, key ed25519.PrivateKey, req PairRequest) (*PairResponse, error) {
	var out PairResponse
	if err := c.do(ctx, key, "", http.MethodPost, "pair", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PairStatus calls GET pair/status.
func (c *Client) PairStatus(ctx context.Context, key ed25519.PrivateKey, linkID string) (*PairStatus, error) {
	var out PairStatus
	if err := c.do(ctx, key, linkID, http.MethodGet, "pair/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Sync calls POST sync.
func (c *Client) Sync(ctx context.Context, key ed25519.PrivateKey, linkID string, req SyncRequest) (*SyncResponse, error) {
	if req.Printers == nil {
		req.Printers = []SyncPrinter{}
	}
	var out SyncResponse
	if err := c.do(ctx, key, linkID, http.MethodPost, "sync", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DataKey calls POST data-key and returns the 32 raw bytes. The caller must
// keep them in memory only and clear them when done.
func (c *Client) DataKey(ctx context.Context, key ed25519.PrivateKey, linkID string) ([]byte, error) {
	var out struct {
		DataKey string `json:"data_key"`
	}
	if err := c.do(ctx, key, linkID, http.MethodPost, "data-key", struct{}{}, &out); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(out.DataKey)
	out.DataKey = ""
	if err != nil || len(raw) != 32 {
		clear(raw)
		return nil, errors.New("data key answer was not 32 bytes of base64")
	}
	return raw, nil
}

func (c *Client) do(ctx context.Context, key ed25519.PrivateKey, linkID, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	u := c.base.JoinPath(Prefix + path)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return err
	}
	ts := c.now().Unix()
	nonce := signer.NewNonce()
	target := req.URL.RequestURI()
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if linkID != "" {
		req.Header.Set(signer.HeaderLink, linkID)
	}
	req.Header.Set(signer.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(signer.HeaderNonce, nonce)
	req.Header.Set(signer.HeaderSignature, signer.Sign(key, method, target, ts, nonce, body))

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, scrubURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("%s %s: reading answer: %w", method, path, err)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("%s %s: answer too large", method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
		var eb struct {
			Detail string `json:"detail"`
			Code   string `json:"code"`
		}
		if json.Unmarshal(data, &eb) == nil {
			e.Code, e.Detail = eb.Code, eb.Detail
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: answer was not valid JSON", method, path)
		}
	}
	return nil
}

// scrubURLError keeps the operation and cause but drops nothing secret: our
// URLs never carry secrets. It exists so callers do not log bodies.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d < 0 {
		return 0
	}
	return min(d, maxRetryWait)
}

// Sync interval bounds (CONTRACT 1.4).
const (
	SyncDefault = 30 * time.Second
	SyncMin     = 5 * time.Second
	SyncMax     = 300 * time.Second
)

// NextSync turns next_sync_s into a duration clamped to 5..300 s. Zero or
// missing means the default of 30 s.
func NextSync(seconds int) time.Duration {
	if seconds <= 0 {
		return SyncDefault
	}
	return min(max(time.Duration(seconds)*time.Second, SyncMin), SyncMax)
}

// Backoff returns a wait for the given failed attempt (1 based): exponential
// from base, capped at limit, with full jitter in [d/2, d].
func Backoff(attempt int, base, limit time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1)
}

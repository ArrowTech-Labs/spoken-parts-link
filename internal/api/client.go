// Package api is the client for the Spoken Parts link API (/api/link/v1).
// Every request is signed with the link's Ed25519 key (CONTRACT 1.1).
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
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
	download  *http.Client // no overall timeout: files can be large
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
		download: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		userAgent: "spoken-parts-link/" + version,
		now:       time.Now,
	}
}

// SetHTTPClient replaces the HTTP client (tests).
func (c *Client) SetHTTPClient(h *http.Client) { c.http, c.download = h, h }

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

	// 0.2: what the swap steps are checked against, and the job.
	Storage     *bool        `json:"storage"`
	AMS         []SyncTray   `json:"ams,omitempty"`
	Nozzles     []SyncNozzle `json:"nozzles,omitempty"`
	SubtaskName string       `json:"subtask_name,omitempty"`
	GcodeFile   string       `json:"gcode_file,omitempty"`
	Layer       *int         `json:"layer,omitempty"`
	TotalLayers *int         `json:"total_layers,omitempty"`
	TrayNow     *int         `json:"tray_now,omitempty"`
	CanStart    string       `json:"can_start,omitempty"`

	// 0.3: camera and alert status for the farm page.
	Monitor *SyncMonitor `json:"monitor,omitempty"`
}

// SyncMonitor is one printer's monitoring status in a sync request.
type SyncMonitor struct {
	Camera      string `json:"camera"` // none, off, waiting, ok, failing, stream_off
	LastFrameAt string `json:"last_frame_at,omitempty"`
	OpenAlerts  int    `json:"open_alerts"`
}

// SyncTray is one AMS slot in a sync request.
type SyncTray struct {
	AmsID  int    `json:"ams_id"`
	SlotID int    `json:"slot_id"`
	Type   string `json:"type"`
	Color  string `json:"color"`
	Nozzle string `json:"nozzle"`
}

// SyncNozzle is one nozzle in a sync request.
type SyncNozzle struct {
	Side     string `json:"side"`
	Diameter string `json:"diameter"`
	Type     string `json:"type"`
}

// SyncRequest is the body of POST sync.
type SyncRequest struct {
	Version        string        `json:"version"`
	UptimeS        int64         `json:"uptime_s"`
	Printers       []SyncPrinter `json:"printers"`
	MonitorVersion int           `json:"monitor_version,omitempty"`
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
	// 0.2: sends waiting for this link, and how each printer may start.
	SendsWaiting int               `json:"sends_waiting"`
	CanStart     map[string]string `json:"can_start"`
	// 0.3: the site's monitoring settings (nil from older backends).
	Monitor json.RawMessage `json:"monitor"`
}

// MapRow is one filament of a send's plan: the AMS slot it prints from.
type MapRow struct {
	Filament int    `json:"filament"`
	Material string `json:"material"`
	Color    string `json:"color"`
	AmsID    int    `json:"ams_id"`
	SlotID   int    `json:"slot_id"`
	Nozzle   string `json:"nozzle"`
	Tray     int    `json:"tray"`
}

// SendPrinter is the printer a send is for.
type SendPrinter struct {
	Number    int    `json:"number"`
	Name      string `json:"name"`
	Serial    string `json:"serial"`
	Model     string `json:"model"`
	ModelCode string `json:"model_code"`
}

// SendOptions are the print options for an auto start.
type SendOptions struct {
	Timelapse     bool `json:"timelapse"`
	BedLeveling   bool `json:"bed_levelling"`
	FlowCali      bool `json:"flow_cali"`
	VibrationCali bool `json:"vibration_cali"`
	LayerInspect  bool `json:"layer_inspect"`
	UseAMS        bool `json:"use_ams"`
}

// Send is one print Spoken Parts sent to a printer of this link.
type Send struct {
	ID          string      `json:"id"`
	State       string      `json:"state"`
	LinkState   string      `json:"link_state"`
	AgeS        int         `json:"age_s"`
	Printer     SendPrinter `json:"printer"`
	PlateIndex  int         `json:"plate_index"`
	FileName    string      `json:"file_name"`
	SubtaskName string      `json:"subtask_name"`
	SHA256      string      `json:"sha256"`
	Mapping     []MapRow    `json:"mapping"`
	AutoStart   bool        `json:"autostart"`
	// DryRun: an owner's test. Never started, whatever else allows it.
	DryRun    bool        `json:"dry_run"`
	Options   SendOptions `json:"options"`
	FileURL   string      `json:"file_url"`
	FileToken string      `json:"file_token"`
}

// StateRequest is the body of POST sends/<id>/state.
type StateRequest struct {
	State       string     `json:"state"`
	Percent     *int       `json:"percent,omitempty"`
	FileName    string     `json:"file_name,omitempty"`
	Code        string     `json:"code,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	SubtaskName string     `json:"subtask_name,omitempty"`
	Alert       *SendAlert `json:"alert,omitempty"`
	// Others: earlier Spoken Parts files still on the printer's storage.
	Others []string `json:"others,omitempty"`
}

// SendAlert is a problem after the start that a person must act on.
type SendAlert struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Event is one progress event of a send, in order. Key makes it idempotent.
type Event struct {
	Key          string   `json:"key"`
	SendID       string   `json:"send_id"`
	ObservedAt   string   `json:"observed_at"`
	GcodeState   string   `json:"gcode_state"`
	Percent      *int     `json:"percent,omitempty"`
	Layer        *int     `json:"layer,omitempty"`
	TotalLayers  *int     `json:"total_layers,omitempty"`
	RemainingMin *int     `json:"remaining_min,omitempty"`
	PrintError   int      `json:"print_error,omitempty"`
	HMS          []string `json:"hms,omitempty"`
	SubtaskName  string   `json:"subtask_name,omitempty"`
}

// MaxFile is the largest file the link downloads (512 MB).
const MaxFile = 512 << 20

// ListSends calls GET sends?state=queued.
func (c *Client) ListSends(ctx context.Context, key ed25519.PrivateKey, linkID string) ([]Send, error) {
	var out struct {
		Sends []Send `json:"sends"`
	}
	if err := c.do(ctx, key, linkID, http.MethodGet, "sends?state=queued", nil, &out); err != nil {
		return nil, err
	}
	return out.Sends, nil
}

// SendState calls POST sends/<id>/state.
func (c *Client) SendState(ctx context.Context, key ed25519.PrivateKey, linkID, id string, req StateRequest) error {
	if !sendIDRe.MatchString(id) {
		return errors.New("bad send id")
	}
	return c.do(ctx, key, linkID, http.MethodPost, "sends/"+id+"/state", req, nil)
}

// Events calls POST printers/<serial>/events (at most 50 per call).
func (c *Client) Events(ctx context.Context, key ed25519.PrivateKey, linkID, serial string, events []Event) error {
	if !serialRe.MatchString(serial) || len(events) == 0 || len(events) > 50 {
		return errors.New("bad events call")
	}
	return c.do(ctx, key, linkID, http.MethodPost, "printers/"+serial+"/events", map[string]any{"events": events}, nil)
}

// MaxFrame is the largest camera frame Spoken Parts takes (400 KB).
const MaxFrame = 400 << 10

// FrameMeta describes one camera frame. It goes in the query string, so the
// signature covers it.
type FrameMeta struct {
	ID          string // uuid made by the link (the same id twice is stored once)
	TakenAt     time.Time
	Reason      string // interval, alert, finish, failed
	Printing    bool
	TaskID      string
	Layer       *int
	TotalLayers *int
	AlertKey    string
}

// FrameAnswer is the answer of POST printers/<serial>/frames.
type FrameAnswer struct {
	FrameID   string `json:"frame_id"`
	Duplicate bool   `json:"duplicate"`
	// ImageURL is a signed link to the frame (24 hours), for alert frames
	// only. It goes to the Home Assistant phone notification, never to logs.
	ImageURL string `json:"image_url"`
}

// CodeCameraOff means Spoken Parts does not want frames from this printer.
const CodeCameraOff = "camera_off"

var (
	reasonRe = regexp.MustCompile(`^(interval|alert|finish|failed)$`)
	taskIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9_:.\-]{1,120}$`)
)

// Frame calls POST printers/<serial>/frames with one JPEG.
func (c *Client) Frame(ctx context.Context, key ed25519.PrivateKey, linkID, serial string, m FrameMeta, jpeg []byte) (*FrameAnswer, error) {
	if !serialRe.MatchString(serial) || !sendIDRe.MatchString(m.ID) || !reasonRe.MatchString(m.Reason) {
		return nil, errors.New("bad frame call")
	}
	if len(jpeg) < 4 || len(jpeg) > MaxFrame || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return nil, errors.New("frame is not a JPEG of at most 400 KB")
	}
	q := url.Values{}
	q.Set("id", m.ID)
	q.Set("ts", strconv.FormatInt(m.TakenAt.Unix(), 10))
	q.Set("reason", m.Reason)
	q.Set("printing", map[bool]string{false: "0", true: "1"}[m.Printing])
	if taskIDRe.MatchString(m.TaskID) {
		q.Set("task_id", m.TaskID)
	}
	if m.Layer != nil {
		q.Set("layer", strconv.Itoa(*m.Layer))
	}
	if m.TotalLayers != nil {
		q.Set("total_layers", strconv.Itoa(*m.TotalLayers))
	}
	if m.AlertKey != "" && keyRe.MatchString(m.AlertKey) {
		q.Set("alert_key", m.AlertKey)
	}
	var out FrameAnswer
	if err := c.doRaw(ctx, key, linkID, "printers/"+serial+"/frames?"+q.Encode(), "image/jpeg", jpeg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Job is the print an alert is about.
type Job struct {
	Task         string `json:"task"`
	TaskID       string `json:"task_id"`
	GcodeState   string `json:"gcode_state"`
	Layer        *int   `json:"layer"`
	TotalLayers  *int   `json:"total_layers"`
	Percent      *int   `json:"percent"`
	RemainingMin *int   `json:"remaining_min"`
}

// Alert is one alert the link raised.
type Alert struct {
	ID       string         `json:"id"`
	TS       string         `json:"ts"`
	Severity string         `json:"severity"`
	Key      string         `json:"key"`
	Message  string         `json:"message"`
	Data     map[string]any `json:"data"`
	FrameID  *string        `json:"frame_id"`
	Job      Job            `json:"job"`
}

// AlertBatch is the body of POST printers/<serial>/alerts.
type AlertBatch struct {
	Alerts  []Alert  `json:"alerts"`
	Cleared []string `json:"cleared"`
	Reset   bool     `json:"reset"`
}

// Alerts calls POST printers/<serial>/alerts (at most 50 alerts per call).
func (c *Client) Alerts(ctx context.Context, key ed25519.PrivateKey, linkID, serial string, b AlertBatch) error {
	if !serialRe.MatchString(serial) || len(b.Alerts) > 50 || (len(b.Alerts) == 0 && len(b.Cleared) == 0 && !b.Reset) {
		return errors.New("bad alerts call")
	}
	if b.Alerts == nil {
		b.Alerts = []Alert{}
	}
	if b.Cleared == nil {
		b.Cleared = []string{}
	}
	return c.do(ctx, key, linkID, http.MethodPost, "printers/"+serial+"/alerts", b, nil)
}

var (
	sendIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	serialRe = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)
)

// ErrTooLarge means the file is over MaxFile.
var ErrTooLarge = errors.New("file is larger than 512 MB")

// Download fetches a send's file once with its one-time token (in the
// X-SP-File-Token header, never in the URL) and writes it to w. It returns
// the bytes written, the sha256 of what was written, and the server's
// X-SP-SHA256 header.
func (c *Client) Download(ctx context.Context, key ed25519.PrivateKey, linkID string, s Send, w io.Writer) (int64, string, string, error) {
	if !sendIDRe.MatchString(s.ID) || s.FileURL != Prefix+"sends/"+s.ID+"/file" || s.FileToken == "" {
		return 0, "", "", errors.New("bad file link")
	}
	u := c.base.JoinPath(s.FileURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", "", err
	}
	c.sign(req, key, linkID, nil)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-SP-File-Token", s.FileToken)
	resp, err := c.download.Do(req)
	if err != nil {
		return 0, "", "", fmt.Errorf("GET file: %w", scrubURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		e := &Error{Status: resp.StatusCode}
		var eb struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(data, &eb) == nil {
			e.Code = eb.Code
		}
		return 0, "", "", e
	}
	if resp.ContentLength > MaxFile {
		return 0, "", "", ErrTooLarge
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, MaxFile+1))
	if err != nil {
		return n, "", "", fmt.Errorf("GET file: %w", err)
	}
	if n > MaxFile {
		return n, "", "", ErrTooLarge
	}
	return n, hex.EncodeToString(h.Sum(nil)), strings.ToLower(resp.Header.Get("X-SP-SHA256")), nil
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
	return c.send(ctx, key, linkID, method, path, "application/json", body, out)
}

// doRaw posts body as is with the given content type.
func (c *Client) doRaw(ctx context.Context, key ed25519.PrivateKey, linkID, path, contentType string, body []byte, out any) error {
	return c.send(ctx, key, linkID, http.MethodPost, path, contentType, body, out)
}

func (c *Client) send(ctx context.Context, key ed25519.PrivateKey, linkID, method, path, contentType string, body []byte, out any) error {
	p, query, _ := strings.Cut(path, "?")
	u := c.base.JoinPath(Prefix + p)
	u.RawQuery = query
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	c.sign(req, key, linkID, body)

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

// sign adds the user agent, the link id and the signature headers.
func (c *Client) sign(req *http.Request, key ed25519.PrivateKey, linkID string, body []byte) {
	ts := c.now().Unix()
	nonce := signer.NewNonce()
	req.Header.Set("User-Agent", c.userAgent)
	if linkID != "" {
		req.Header.Set(signer.HeaderLink, linkID)
	}
	req.Header.Set(signer.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(signer.HeaderNonce, nonce)
	req.Header.Set(signer.HeaderSignature, signer.Sign(key, req.Method, req.URL.RequestURI(), ts, nonce, body))
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

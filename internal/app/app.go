// Package app runs the link: pairing, the data key, sync, and the printer
// manager, and answers the ingress page.
package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/hanotify"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/identity"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/sends"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

// Fixed values sent at pairing.
const (
	LinkName = "Spoken Parts Link"
	Platform = "ha_addon"
)

// Config for New. Zero durations get production defaults.
type Config struct {
	DataDir string
	Version string
	Server  *url.URL
	Log     *logx.Logger
	Bundle  *printers.Bundle

	PollEvery      time.Duration // pair/status poll, 5 s
	PendingTimeout time.Duration // 15 min
	SyncMin        time.Duration // 5 s
	SyncMax        time.Duration // 300 s
	RetryMin       time.Duration // 5 s
	RetryMax       time.Duration // 5 min
	UnknownConfirm time.Duration // 1 min, see unknownConfirmFor
	UnknownRetry   time.Duration // 20 s between unknown_link checks
	Now            func() time.Time

	// AutoStart allows project_file on Developer Mode printers (off).
	AutoStart bool

	// Test hooks.
	HTTPClient   *http.Client
	PrinterDial  func(ctx context.Context, network, addr string) (net.Conn, error)
	PrinterPort  int
	FTPSPort     int
	PrinterRetry time.Duration
	Sends        sends.Config // only the timings are used

	// 0.3 monitoring. Grabber turns a camera connection into one JPEG
	// (internal/camera with ffmpeg in production).
	Grabber     FrameGrabber
	CameraPort  int
	HAURL       string        // Home Assistant, http://127.0.0.1:8123
	MonitorTick time.Duration // 30 s
}

// FrameGrabber gets one JPEG over an open, checked camera connection.
type FrameGrabber interface {
	Grab(ctx context.Context, serial string, conn net.Conn, host, code string) ([]byte, error)
}

// App is the running link.
type App struct {
	cfg      Config
	log      *logx.Logger
	red      *redact.Redactor
	api      *api.Client
	codes    *store.Codes
	printers *printers.Manager
	sends    *sends.Runner
	monitor  *monitor.Runner
	ha       *hanotify.Notifier
	started  time.Time
	wake     chan struct{}

	idDir     string
	statePath string

	mu          sync.Mutex
	id          *identity.Identity
	st          store.State
	lastSync    time.Time
	syncProblem bool
	paused      bool
	codesReady  bool
	failures    int

	// unknownSince is when Spoken Parts first answered unknown_link in the
	// current run of such answers; unknownCount counts them.
	unknownSince time.Time
	unknownCount int
	// pairBlockedUntil honours Retry-After from a throttled pair call.
	pairBlockedUntil time.Time
}

// unknown_link must be seen this many times in a row, over at least this
// long, before the link gives up on its pairing. One odd answer (for example
// during a backend restore) must not wipe a working link.
const (
	unknownConfirmCount = 3
	unknownConfirmFor   = time.Minute
	unknownRetry        = 20 * time.Second
)

// New loads (or creates) the identity and state under cfg.DataDir.
func New(ctx context.Context, cfg Config) (*App, error) {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.PollEvery, 5*time.Second)
	def(&cfg.PendingTimeout, 15*time.Minute)
	def(&cfg.SyncMin, api.SyncMin)
	def(&cfg.SyncMax, api.SyncMax)
	def(&cfg.RetryMin, 5*time.Second)
	def(&cfg.RetryMax, 5*time.Minute)
	def(&cfg.UnknownConfirm, unknownConfirmFor)
	def(&cfg.UnknownRetry, unknownRetry)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &App{
		cfg:       cfg,
		log:       cfg.Log,
		red:       cfg.Log.Redactor(),
		api:       api.New(cfg.Server, cfg.Version),
		codes:     store.NewCodes(filepath.Join(cfg.DataDir, "secrets", "codes.enc")),
		started:   cfg.Now(),
		wake:      make(chan struct{}, 1),
		idDir:     filepath.Join(cfg.DataDir, "identity"),
		statePath: filepath.Join(cfg.DataDir, "state", "state.json"),
	}
	if cfg.HTTPClient != nil {
		a.api.SetHTTPClient(cfg.HTTPClient)
	}
	for _, d := range []string{"identity", "secrets", "state", "spool", "frames"} {
		if err := fsutil.EnsureDir(filepath.Join(cfg.DataDir, d)); err != nil {
			return nil, err
		}
	}
	if err := a.loadIdentity(); err != nil {
		return nil, err
	}
	if err := store.LoadJSON(a.statePath, &a.st); err != nil {
		a.log.Warn("could not read the saved link state, starting unpaired", "err", err)
		a.st = store.State{}
	}
	m, err := printers.New(ctx, printers.Config{
		Bundle:     cfg.Bundle,
		Codes:      a.codes,
		MetaPath:   filepath.Join(cfg.DataDir, "state", "printers.json"),
		Log:        cfg.Log,
		Port:       cfg.PrinterPort,
		Dial:       cfg.PrinterDial,
		RetryMin:   cfg.PrinterRetry,
		FTPSPort:   cfg.FTPSPort,
		CameraPort: cfg.CameraPort,
		AutoStart:  cfg.AutoStart,
		Now:        cfg.Now,
	})
	if err != nil {
		return nil, err
	}
	a.printers = m
	a.ha = hanotify.New(cfg.HAURL, store.NewCodes(filepath.Join(cfg.DataDir, "secrets", "ha.enc")), a.red)
	a.monitor = monitor.New(monitor.Config{
		StatePath: filepath.Join(cfg.DataDir, "state", "monitor.json"),
		FramesDir: filepath.Join(cfg.DataDir, "frames"),
		Source:    monitorSource{a},
		Uplink:    linkAPI{a},
		Notify:    a.ha,
		Log:       cfg.Log,
		Now:       cfg.Now,
		Tick:      cfg.MonitorTick,
	})
	sc := cfg.Sends
	sc.SpoolDir = filepath.Join(cfg.DataDir, "spool")
	sc.StatePath = filepath.Join(cfg.DataDir, "state", "sends.json")
	sc.API = linkAPI{a}
	sc.Printers = m
	sc.Log = cfg.Log
	if a.sends, err = sends.New(sc); err != nil {
		return nil, err
	}
	if a.st.Status == store.StatusPending && cfg.Now().Sub(a.st.PendingSince) > cfg.PendingTimeout {
		a.mu.Lock()
		a.resetLocked(web.MsgTimedOut, true)
		a.mu.Unlock()
	}
	a.log.Info("link started", "version", cfg.Version, "status", statusLabel(a.st.Status), "key", identity.Fingerprint(a.id.Public))
	return a, nil
}

func statusLabel(s string) string {
	if s == "" {
		return "not_paired"
	}
	return s
}

func (a *App) loadIdentity() error {
	id, created, err := identity.LoadOrCreate(a.idDir)
	if err != nil {
		return err
	}
	a.red.AddBytes(id.Private.Seed())
	a.red.AddBytes(id.Private)
	a.red.AddBytes(id.LocalSecret)
	if created {
		a.log.Info("created a new link key", "key", identity.Fingerprint(id.Public))
	}
	a.id = id
	return nil
}

// Observe passes a discovery announcement to the printer manager.
func (a *App) Observe(ann ssdp.Announcement) { a.printers.Observe(ann) }

// Close stops printer sessions.
func (a *App) Close() { a.printers.Close() }

func (a *App) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// sleep waits for d, a wake up, or ctx. It returns false if ctx is done.
func (a *App) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-a.wake:
		return true
	case <-t.C:
		return true
	}
}

type snapshot struct {
	key    ed25519.PrivateKey
	id     *identity.Identity
	linkID string
	status string
}

func (a *App) snap() snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return snapshot{key: a.id.Private, id: a.id, linkID: a.st.LinkID, status: a.st.Status}
}

// current reports whether s still describes the link (no reset since).
func (a *App) currentLocked(s snapshot) bool {
	return a.id == s.id && a.st.LinkID == s.linkID && a.st.Status == s.status
}

// Run drives the link until ctx is done.
func (a *App) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() { a.sends.Run(ctx); close(done) }()
	defer func() { <-done }()
	mdone := make(chan struct{})
	go func() { a.monitor.Run(ctx); close(mdone) }()
	defer func() { <-mdone }()
	for ctx.Err() == nil {
		s := a.snap()
		switch s.status {
		case store.StatusPending:
			a.stepPending(ctx, s)
		case store.StatusActive:
			a.stepActive(ctx, s)
		default:
			a.sleep(ctx, time.Hour)
		}
	}
}

// unknownLinkLocked records an unknown_link answer. It returns true (and
// resets the link, wiping codes and key as for link_revoked: the data key
// can never come back) once the answer is confirmed; otherwise it returns
// false and the caller retries after UnknownRetry.
func (a *App) unknownLinkLocked() bool {
	now := a.cfg.Now()
	if a.unknownCount == 0 {
		a.unknownSince = now
	}
	a.unknownCount++
	if a.unknownCount < unknownConfirmCount || now.Sub(a.unknownSince) < a.cfg.UnknownConfirm {
		a.syncProblem = true
		a.log.Warn("Spoken Parts does not know this link, checking again", "seen", a.unknownCount)
		return false
	}
	a.log.Warn("Spoken Parts does not know this link any more, deleting access codes and the link key")
	a.resetLocked(web.MsgUnknownLink, true)
	return true
}

func (a *App) stepPending(ctx context.Context, s snapshot) {
	a.mu.Lock()
	since := a.st.PendingSince
	a.mu.Unlock()
	if a.cfg.Now().Sub(since) > a.cfg.PendingTimeout {
		a.mu.Lock()
		if a.currentLocked(s) {
			a.log.Info("pairing timed out")
			a.resetLocked(web.MsgTimedOut, true)
		}
		a.mu.Unlock()
		return
	}
	ps, err := a.api.PairStatus(ctx, s.key, s.linkID)
	wait := a.cfg.PollEvery
	a.mu.Lock()
	switch {
	case !a.currentLocked(s):
	case api.HasCode(err, api.CodeLinkRevoked):
		a.log.Info("pairing was not approved or expired")
		a.resetLocked(web.MsgRejected, true)
	case api.HasCode(err, api.CodeUnknownLink):
		if !a.unknownLinkLocked() {
			wait = max(wait, a.cfg.UnknownRetry)
		}
	case err != nil:
		a.syncProblem = true
		wait = max(wait, api.RetryAfter(err))
		a.log.Debug("pair status check failed", "err", err)
	case ps.CheckNumber != "" && ps.CheckNumber != identity.CheckNumber(s.id.Public):
		a.log.Warn("check number from Spoken Parts does not match this key, stopping")
		a.resetLocked(web.MsgMismatch, true)
	case ps.Status == store.StatusActive:
		a.unknownCount = 0
		a.syncProblem = false
		a.st.Status = store.StatusActive
		a.st.PairedAt = a.cfg.Now().UTC()
		a.st.Notice = ""
		if ps.Site != nil {
			a.st.SiteID, a.st.SiteName = ps.Site.ID, ps.Site.Name
		}
		if ps.Company != nil {
			a.st.CompanyName = ps.Company.Name
		}
		a.saveStateLocked()
		a.log.Info("link approved", "site", a.st.SiteName)
		wait = 0
	default:
		a.syncProblem = false
	}
	a.mu.Unlock()
	if wait > 0 {
		a.sleep(ctx, wait)
	}
}

func (a *App) stepActive(ctx context.Context, s snapshot) {
	a.mu.Lock()
	ready := a.codesReady
	a.mu.Unlock()
	if !ready {
		if !a.loadDataKey(ctx, s) {
			return
		}
	}
	resp, err := a.api.Sync(ctx, s.key, s.linkID, a.buildSync())
	var wait time.Duration
	a.mu.Lock()
	switch {
	case !a.currentLocked(s):
	case api.HasCode(err, api.CodeLinkRevoked):
		a.log.Warn("this link was removed in Spoken Parts, deleting access codes and the link key")
		a.resetLocked(web.MsgRemoved, true)
	case api.HasCode(err, api.CodeLinkPending):
		a.st.Status = store.StatusPending
		a.st.PendingSince = a.cfg.Now().UTC()
		a.saveStateLocked()
	case api.HasCode(err, api.CodeUnknownLink):
		if !a.unknownLinkLocked() {
			wait = a.cfg.UnknownRetry
		}
	case err != nil:
		a.syncProblem = true
		a.failures++
		wait = max(api.Backoff(a.failures, a.cfg.RetryMin, a.cfg.RetryMax), api.RetryAfter(err))
		a.log.Warn("sync failed", "err", err, "retry_in", wait.Round(time.Second))
	default:
		a.unknownCount = 0
		a.failures = 0
		a.syncProblem = false
		a.lastSync = a.cfg.Now()
		a.paused = resp.Paused
		changed := false
		if resp.Site != nil && (resp.Site.Name != a.st.SiteName || resp.Site.ID != a.st.SiteID) {
			a.st.SiteID, a.st.SiteName = resp.Site.ID, resp.Site.Name
			changed = true
		}
		if resp.Company != nil && resp.Company.Name != a.st.CompanyName {
			a.st.CompanyName = resp.Company.Name
			changed = true
		}
		if changed {
			a.saveStateLocked()
		}
		if resp.SendsWaiting > 0 && !resp.Paused {
			a.sends.Kick()
		}
		if ms, ok := monitor.ParseSettings(resp.Monitor); ok {
			a.monitor.SetSettings(ms)
		}
		wait = a.nextSync(resp.NextSyncS)
	}
	a.mu.Unlock()
	if wait > 0 {
		a.sleep(ctx, wait)
	}
}

func (a *App) nextSync(n int) time.Duration {
	d := api.NextSync(n)
	return min(max(d, a.cfg.SyncMin), a.cfg.SyncMax)
}

// loadDataKey fetches the data key, derives the file key and opens the
// access code file. Neither key is ever written to disk.
func (a *App) loadDataKey(ctx context.Context, s snapshot) bool {
	dk, err := a.api.DataKey(ctx, s.key, s.linkID)
	if err == nil {
		a.red.AddBytes(dk)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer clear(dk)
	if !a.currentLocked(s) {
		return false
	}
	switch {
	case api.HasCode(err, api.CodeLinkRevoked):
		a.log.Warn("this link was removed in Spoken Parts, deleting access codes and the link key")
		a.resetLocked(web.MsgRemoved, true)
		return false
	case api.HasCode(err, api.CodeLinkPending):
		a.st.Status = store.StatusPending
		a.st.PendingSince = a.cfg.Now().UTC()
		a.saveStateLocked()
		return false
	case api.HasCode(err, api.CodeUnknownLink):
		if !a.unknownLinkLocked() {
			a.mu.Unlock()
			a.sleep(ctx, a.cfg.UnknownRetry)
			a.mu.Lock()
		}
		return false
	case err != nil:
		a.syncProblem = true
		a.failures++
		wait := max(api.Backoff(a.failures, a.cfg.RetryMin, a.cfg.RetryMax), api.RetryAfter(err))
		a.log.Warn("could not get the data key", "err", err, "retry_in", wait.Round(time.Second))
		a.mu.Unlock()
		a.sleep(ctx, wait)
		a.mu.Lock()
		return false
	}
	key, err := store.DeriveKey(s.id.LocalSecret, dk)
	if err != nil {
		a.log.Error("could not derive the access code key", "err", err)
		return false
	}
	a.red.AddBytes(key)
	err = a.codes.Unlock(key)
	clear(key)
	if hk, herr := store.DeriveKeyFor(s.id.LocalSecret, dk, store.HAInfo); herr == nil {
		a.red.AddBytes(hk)
		if herr = a.ha.Unlock(hk); errors.Is(herr, store.ErrCannotOpen) {
			a.log.Warn("the saved Home Assistant webhook could not be opened, it needs to be typed again")
		}
		clear(hk)
	}
	switch {
	case errors.Is(err, store.ErrCannotOpen):
		a.log.Warn("saved access codes could not be opened, they need to be typed again")
	case err != nil:
		a.log.Error("could not read saved access codes", "err", err)
		return false
	}
	for _, c := range a.codes.Values() {
		a.red.Add(c)
	}
	a.failures = 0
	a.unknownCount = 0
	a.codesReady = true
	a.printers.SetEnabled(true)
	a.sends.SetEnabled(true)
	a.log.Info("access codes opened", "count", len(a.codes.Values()))
	return true
}

func (a *App) buildSync() api.SyncRequest {
	snaps := a.printers.Snapshot()
	if len(snaps) > 100 {
		// Keep the printers that matter: ones with a code or a checked
		// certificate first, then the most recently seen.
		sort.SliceStable(snaps, func(i, j int) bool {
			ri, rj := syncRank(snaps[i]), syncRank(snaps[j])
			if ri != rj {
				return ri > rj
			}
			return snaps[i].LastSeen.After(snaps[j].LastSeen)
		})
		snaps = snaps[:100]
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].Serial < snaps[j].Serial })
	}
	req := api.SyncRequest{
		Version:        a.cfg.Version,
		UptimeS:        int64(a.cfg.Now().Sub(a.started).Seconds()),
		Printers:       make([]api.SyncPrinter, 0, len(snaps)),
		MonitorVersion: 1,
	}
	for _, p := range snaps {
		access := string(p.Access)
		if p.Access == printers.AccessUnchecked {
			access = string(printers.AccessMissing)
		}
		name := p.Name
		if name == "" {
			name = p.Model
		}
		req.Printers = append(req.Printers, api.SyncPrinter{
			Serial:          p.Serial,
			ModelCode:       p.ModelCode,
			Name:            name,
			IP:              p.IP,
			Firmware:        p.Firmware,
			Online:          p.Online,
			AccessCode:      access,
			DevMode:         p.DevMode,
			LanOnly:         p.LanOnly,
			GcodeState:      p.GcodeState,
			Percent:         p.Percent,
			RemainingMin:    p.RemainingMin,
			CertFingerprint: p.CertFingerprint,
			Storage:         p.Storage,
			AMS:             syncTrays(p.Trays),
			Nozzles:         syncNozzles(p.Nozzles),
			SubtaskName:     p.Subtask,
			GcodeFile:       p.GcodeFile,
			Layer:           p.Layer,
			TotalLayers:     p.TotalLayers,
			TrayNow:         p.TrayNow,
			CanStart:        p.CanStart(a.cfg.AutoStart),
			Monitor:         a.syncMonitor(p.Serial),
		})
	}
	return req
}

func (a *App) syncMonitor(serial string) *api.SyncMonitor {
	st := a.monitor.Status(serial)
	m := &api.SyncMonitor{Camera: st.Camera, OpenAlerts: st.OpenAlerts}
	if !st.LastFrameAt.IsZero() {
		m.LastFrameAt = st.LastFrameAt.UTC().Format(time.RFC3339)
	}
	return m
}

func syncTrays(ts []printers.Tray) []api.SyncTray {
	var out []api.SyncTray
	for _, t := range ts {
		if len(out) >= 64 {
			break
		}
		out = append(out, api.SyncTray{AmsID: t.AmsID, SlotID: t.SlotID, Type: t.Type, Color: t.Color, Nozzle: t.Nozzle})
	}
	return out
}

func syncNozzles(ns []printers.Nozzle) []api.SyncNozzle {
	var out []api.SyncNozzle
	for _, n := range ns {
		if len(out) >= 2 {
			break
		}
		out = append(out, api.SyncNozzle{Side: n.Side, Diameter: n.Diameter, Type: n.Type})
	}
	return out
}

// linkAPI signs the sends calls with the link's current key.
type linkAPI struct{ a *App }

var errNotActive = errors.New("link is not active")

func (l linkAPI) active() (snapshot, error) {
	s := l.a.snap()
	if s.status != store.StatusActive {
		return s, errNotActive
	}
	return s, nil
}

func (l linkAPI) ListSends(ctx context.Context) ([]api.Send, error) {
	s, err := l.active()
	if err != nil {
		return nil, err
	}
	// File tokens are one-time and never logged. They are not added to the
	// redactor's small ring, where they would push out the pairing codes.
	return l.a.api.ListSends(ctx, s.key, s.linkID)
}

func (l linkAPI) Download(ctx context.Context, send api.Send, w io.Writer) (int64, string, string, error) {
	s, err := l.active()
	if err != nil {
		return 0, "", "", err
	}
	return l.a.api.Download(ctx, s.key, s.linkID, send, w)
}

func (l linkAPI) SendState(ctx context.Context, id string, req api.StateRequest) error {
	s, err := l.active()
	if err != nil {
		return err
	}
	return l.a.api.SendState(ctx, s.key, s.linkID, id, req)
}

func (l linkAPI) Events(ctx context.Context, serial string, events []api.Event) error {
	s, err := l.active()
	if err != nil {
		return err
	}
	return l.a.api.Events(ctx, s.key, s.linkID, serial, events)
}

// Frame implements monitor.Uplink.
func (l linkAPI) Frame(ctx context.Context, serial string, m api.FrameMeta, jpeg []byte) (*api.FrameAnswer, error) {
	s, err := l.active()
	if err != nil {
		return nil, err
	}
	ans, err := l.a.api.Frame(ctx, s.key, s.linkID, serial, m, jpeg)
	if err == nil && ans.ImageURL != "" {
		// A signed link: anyone holding it sees the frame for a day.
		l.a.red.AddTransient(ans.ImageURL)
	}
	return ans, err
}

// Alerts implements monitor.Uplink.
func (l linkAPI) Alerts(ctx context.Context, serial string, b api.AlertBatch) error {
	s, err := l.active()
	if err != nil {
		return err
	}
	return l.a.api.Alerts(ctx, s.key, s.linkID, serial, b)
}

// monitorSource gives the monitor the printers' reports and camera frames.
type monitorSource struct{ a *App }

func (m monitorSource) Feeds() []monitor.PrinterFeed { return m.a.printers.Feeds() }

var errNoGrabber = errors.New("camera frames are not available in this build")

func (m monitorSource) Grab(ctx context.Context, serial string) ([]byte, error) {
	g := m.a.cfg.Grabber
	if g == nil {
		return nil, errNoGrabber
	}
	var out []byte
	err := m.a.printers.WithCamera(ctx, serial, func(conn net.Conn, host, code string) error {
		var err error
		out, err = g.Grab(ctx, serial, conn, host, code)
		return err
	})
	return out, err
}

func syncRank(p printers.Snapshot) int {
	r := 0
	if p.Access != printers.AccessMissing {
		r += 2
	}
	if p.Known {
		r++
	}
	return r
}

// resetLocked returns the link to "not paired". With wipe it deletes the
// access code file, the key and the local secret, then makes a new key.
func (a *App) resetLocked(notice web.Msg, wipe bool) {
	a.printers.SetEnabled(false)
	// Sends and their files (the spool) go with the link, and so do the
	// monitor state and the camera frames.
	a.sends.Reset()
	a.monitor.Reset()
	a.ha.Lock()
	if wipe {
		a.printers.ResetAccess()
		if err := a.codes.Wipe(); err != nil {
			a.log.Error("could not delete the access code file", "err", err)
		}
		if err := a.ha.Wipe(); err != nil {
			a.log.Error("could not delete the Home Assistant webhook file", "err", err)
		}
		if err := identity.Wipe(a.idDir); err != nil {
			a.log.Error("could not delete the link key", "err", err)
		}
		a.id.Zero()
		if err := a.loadIdentity(); err != nil {
			a.log.Error("could not create a new link key", "err", err)
		}
	}
	a.st = store.State{Notice: string(notice)}
	a.saveStateLocked()
	a.codesReady = false
	a.paused = false
	a.syncProblem = false
	a.lastSync = time.Time{}
	a.failures = 0
	a.unknownCount = 0
	a.kick()
}

func (a *App) saveStateLocked() {
	if err := store.SaveJSON(a.statePath, a.st); err != nil {
		a.log.Error("could not save link state", "err", err)
	}
}

// ---- web.Controller ----

// View implements web.Controller.
func (a *App) View() web.View {
	a.mu.Lock()
	v := web.View{
		CheckNumber:  a.st.CheckNumber,
		SiteName:     a.st.SiteName,
		CompanyName:  a.st.CompanyName,
		Notice:       web.Msg(a.st.Notice),
		LastSync:     a.lastSync,
		SyncProblem:  a.syncProblem,
		Paused:       a.paused,
		CodesReady:   a.codesReady,
		CodesDamaged: a.codesReady && a.codes.Damaged(),
		Version:      a.cfg.Version,
	}
	switch a.st.Status {
	case store.StatusPending:
		v.Phase = web.PhasePending
	case store.StatusActive:
		v.Phase = web.PhaseActive
	default:
		v.Phase = web.PhaseUnpaired
	}
	a.mu.Unlock()
	v.Printers = a.printers.Snapshot()
	v.AutoStart = a.cfg.AutoStart
	v.MonitorOn = a.monitor.Enabled()
	v.Monitor = map[string]monitor.Status{}
	for _, p := range v.Printers {
		v.Monitor[p.Serial] = a.monitor.Status(p.Serial)
	}
	v.HAReady = v.CodesReady
	v.HASet = a.ha.Configured()
	v.HAMin = string(a.ha.MinSeverity())
	v.Sends = map[string]sends.Status{}
	for _, p := range v.Printers {
		if st, ok := a.sends.Status(p.Serial); ok {
			v.Sends[p.Serial] = st
		}
	}
	return v
}

var linkIDRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// Pair implements web.Controller. code is already normalized.
func (a *App) Pair(ctx context.Context, code string) error {
	a.mu.Lock()
	blocked := a.cfg.Now().Before(a.pairBlockedUntil)
	a.mu.Unlock()
	if blocked {
		return web.MsgThrottled // honour the server's Retry-After
	}
	for attempt := 0; attempt < 2; attempt++ {
		s := a.snap()
		if s.status != store.StatusNone {
			return web.MsgAlreadyPaired
		}
		resp, err := a.api.Pair(ctx, s.key, api.PairRequest{
			Code:      code,
			PublicKey: s.id.PublicKeyB64(),
			Name:      LinkName,
			Platform:  Platform,
			Version:   a.cfg.Version,
		})
		a.mu.Lock()
		if !a.currentLocked(s) {
			a.mu.Unlock()
			return web.MsgError
		}
		switch {
		case api.HasCode(err, api.CodeKeyInUse) && attempt == 0:
			// This key already belongs to a link (for example after the
			// state file was lost). Make a fresh key and try once more.
			a.log.Info("link key already in use, making a new one")
			a.resetLocked("", true)
			a.mu.Unlock()
			continue
		case err != nil:
			if ra := api.RetryAfter(err); ra > 0 {
				a.pairBlockedUntil = a.cfg.Now().Add(ra)
			} else if api.HasCode(err, api.CodeThrottled) {
				a.pairBlockedUntil = a.cfg.Now().Add(time.Minute)
			}
			a.mu.Unlock()
			a.log.Info("pairing failed", "err", err)
			return pairProblem(err)
		}
		local := identity.CheckNumber(s.id.Public)
		if resp.CheckNumber != local || !linkIDRe.MatchString(resp.LinkID) {
			a.log.Warn("pairing answer did not match this link key, stopping")
			a.resetLocked(web.MsgMismatch, true)
			a.mu.Unlock()
			return web.MsgMismatch
		}
		a.st = store.State{
			LinkID:       resp.LinkID,
			Status:       store.StatusPending,
			CheckNumber:  local,
			PendingSince: a.cfg.Now().UTC(),
		}
		a.saveStateLocked()
		a.log.Info("pairing started, waiting for approval in Spoken Parts", "check_number", local, "key", identity.Fingerprint(s.id.Public))
		a.mu.Unlock()
		a.kick()
		return nil
	}
	return web.MsgError
}

func pairProblem(err error) web.Msg {
	var e *api.Error
	if !errors.As(err, &e) {
		return web.MsgCantReach
	}
	switch e.Code {
	case api.CodeBadCode:
		return web.MsgBadCode
	case api.CodeInvalid:
		return web.MsgCodeFormat
	case api.CodeThrottled:
		return web.MsgThrottled
	case api.CodeLinksOff:
		return web.MsgLinksOff
	}
	if e.Status >= 500 {
		return web.MsgCantReach
	}
	return web.MsgError
}

// CancelPairing implements web.Controller. The pending link in Spoken Parts
// expires by itself; a fresh key makes sure it can never become active.
func (a *App) CancelPairing() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Status != store.StatusPending {
		return nil
	}
	a.log.Info("pairing stopped on the link page")
	a.resetLocked("", true)
	return nil
}

// SaveCode implements web.Controller.
func (a *App) SaveCode(serial, code string) error {
	if !accessCodeRe.MatchString(code) {
		return web.MsgAccessFormat
	}
	a.red.Add(code)
	a.mu.Lock()
	ready := a.st.Status == store.StatusActive && a.codesReady
	a.mu.Unlock()
	if !ready {
		return web.MsgNotReady
	}
	if !a.printers.Has(serial) {
		return web.MsgUnknownPrinter
	}
	if err := a.codes.Set(serial, code); err != nil {
		a.log.Error("could not save an access code", "serial_tail", ssdp.Tail(serial), "err", err)
		return web.MsgSaveFailed
	}
	a.log.Info("access code saved", "serial_tail", ssdp.Tail(serial))
	a.printers.CodeSaved(serial)
	return nil
}

var accessCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)

// RemovePrinter implements web.Controller: forget a printer here (its code,
// trusted or checked certificate and access result). It is the way back for
// a printer whose certificate changed: it is treated as new when it
// announces itself again.
func (a *App) RemovePrinter(serial string) error {
	a.mu.Lock()
	ready := a.st.Status == store.StatusActive && a.codesReady
	a.mu.Unlock()
	if !ready {
		return web.MsgNotReady
	}
	if a.codes.Has(serial) {
		if err := a.codes.Delete(serial); err != nil {
			a.log.Error("could not delete an access code", "serial_tail", ssdp.Tail(serial), "err", err)
			return web.MsgSaveFailed
		}
	}
	if err := a.printers.Forget(serial); err != nil {
		return web.MsgUnknownPrinter
	}
	a.monitor.Forget(serial)
	return nil
}

// Trust implements web.Controller.
func (a *App) Trust(serial, fp string) error {
	switch err := a.printers.Trust(serial, fp); {
	case errors.Is(err, printers.ErrUnknownPrinter):
		return web.MsgUnknownPrinter
	case err != nil:
		return web.MsgNothingToTrust
	}
	return nil
}

// Unpair implements web.Controller: remove the link locally.
func (a *App) Unpair() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log.Info("link removed on the link page, deleting access codes and the link key")
	a.resetLocked("", true)
	return nil
}

// SaveHA implements web.Controller: store the Home Assistant webhook id.
func (a *App) SaveHA(id string, minSev string) error {
	a.mu.Lock()
	ready := a.st.Status == store.StatusActive && a.codesReady
	a.mu.Unlock()
	if !ready {
		return web.MsgNotReady
	}
	a.red.Add(id)
	if err := a.ha.Set(id, monitor.Severity(minSev)); err != nil {
		if errors.Is(err, hanotify.ErrBadID) {
			return web.MsgHAFormat
		}
		a.log.Error("could not save the Home Assistant webhook", "err", err)
		return web.MsgSaveFailed
	}
	a.log.Info("Home Assistant phone notifications set up", "min_severity", minSev)
	return nil
}

// SetHAMin implements web.Controller.
func (a *App) SetHAMin(minSev string) error {
	if err := a.ha.SetMin(monitor.Severity(minSev)); err != nil {
		return web.MsgSaveFailed
	}
	return nil
}

// RemoveHA implements web.Controller.
func (a *App) RemoveHA() error {
	if err := a.ha.Clear(); err != nil {
		return web.MsgSaveFailed
	}
	a.log.Info("Home Assistant phone notifications removed")
	return nil
}

// TestHA implements web.Controller: one test notification.
func (a *App) TestHA(ctx context.Context) error {
	if !a.ha.Configured() {
		return web.MsgNotReady
	}
	err := a.ha.Notify(ctx, monitor.Notice{
		Title: "Spoken Parts", Message: "Test from the Spoken Parts Link. Phone alerts work.", Severity: "info", Key: "test",
	})
	if err != nil {
		a.log.Warn("test notification failed", "err", err)
		return web.MsgHAFailed
	}
	return nil
}

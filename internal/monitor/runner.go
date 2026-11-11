package monitor

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

// PrinterFeed is one printer as the printer manager sees it.
type PrinterFeed struct {
	Serial string
	Name   string
	// Connected is true while the MQTT session is up.
	Connected  bool
	LastReport time.Time
	// Report is a copy of the merged print object (nil before the first).
	Report map[string]json.RawMessage
}

// Source is the printer side: reports, and camera frames.
type Source interface {
	// Feeds lists the printers the link holds a session for (an access
	// code is stored and not known to be wrong).
	Feeds() []PrinterFeed
	// Grab returns one JPEG from the printer's own camera.
	Grab(ctx context.Context, serial string) ([]byte, error)
}

// Uplink is the Spoken Parts side.
type Uplink interface {
	Frame(ctx context.Context, serial string, m api.FrameMeta, jpeg []byte) (*api.FrameAnswer, error)
	Alerts(ctx context.Context, serial string, b api.AlertBatch) error
}

// Notice is a phone notification (Home Assistant).
type Notice struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Key      string `json:"key"`
	Printer  string `json:"printer"`
	ImageURL string `json:"image_url,omitempty"`
}

// Notifier sends phone notifications. Ready is false when nothing is set up.
type Notifier interface {
	Ready() (bool, Severity)
	Notify(ctx context.Context, n Notice) error
}

// Config for a Runner. Zero values get defaults.
type Config struct {
	StatePath  string // /data/state/monitor.json
	FramesDir  string // /data/frames
	Source     Source
	Uplink     Uplink
	Notify     Notifier
	Log        *logx.Logger
	Now        func() time.Time
	Tick       time.Duration // 30 s, one rule "poll"
	StaleAfter time.Duration // 3 min without a report counts as a lost feed
}

// Camera states for the page and for sync.
const (
	CamOff       = "off"
	CamWaiting   = "waiting"
	CamOK        = "ok"
	CamFailing   = "failing"
	CamStreamOff = "stream_off"
)

// Limits for what is kept while Spoken Parts cannot be reached.
const (
	maxBatches   = 50
	maxBatchSize = 50
)

// Runner evaluates the rules for every printer once a tick and hands alerts
// and frames to one worker per printer, so a slow camera or upload never
// holds up the rules or the printer sessions.
type Runner struct {
	cfg Config

	mu       sync.Mutex
	st       saved
	workers  map[string]chan struct{}
	ctx      context.Context
	wg       sync.WaitGroup
	lastSave time.Time
	ticks    atomic.Uint64
}

// Ticks counts finished ticks (tests wait on it).
func (r *Runner) Ticks() uint64 { return r.ticks.Load() }

type saved struct {
	Settings     Settings           `json:"settings"`
	HaveSettings bool               `json:"have_settings"`
	Printers     map[string]*pstate `json:"printers"`
}

type pstate struct {
	Name         string              `json:"name,omitempty"`
	Last         *Snapshot           `json:"last,omitempty"`
	Mem          Memory              `json:"mem"`
	Sent         map[string]sentMark `json:"sent,omitempty"`
	Open         map[string]Severity `json:"open,omitempty"`
	LastGrabAt   time.Time           `json:"last_grab_at,omitzero"`
	LastFrameAt  time.Time           `json:"last_frame_at,omitzero"`
	CamReason    string              `json:"cam_reason,omitempty"`
	CamBlocked   bool                `json:"cam_blocked,omitempty"`
	GrabDue      bool                `json:"grab_due,omitempty"`
	Batches      []*batch            `json:"batches,omitempty"`
	RetryAt      time.Time           `json:"retry_at,omitzero"`
	BatchFailure int                 `json:"batch_failures,omitempty"`
}

type sentMark struct {
	At       time.Time `json:"at"`
	Severity Severity  `json:"severity"`
}

// batch is one alerts call waiting to go to Spoken Parts.
type batch struct {
	Alerts  []*pending `json:"alerts"`
	Cleared []string   `json:"cleared,omitempty"`
	Reset   bool       `json:"reset,omitempty"`
}

type pending struct {
	Alert      api.Alert `json:"alert"`
	WantFrame  string    `json:"want_frame,omitempty"` // frame reason, "" for none
	FrameTried bool      `json:"frame_tried,omitempty"`
	Notified   bool      `json:"notified,omitempty"`
}

// New loads the saved state (a missing or broken file starts fresh).
func New(cfg Config) *Runner {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 30 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 3 * time.Minute
	}
	r := &Runner{cfg: cfg, workers: map[string]chan struct{}{}}
	if cfg.StatePath != "" {
		if err := store.LoadJSON(cfg.StatePath, &r.st); err != nil {
			cfg.Log.Warn("could not read the saved monitor state, starting fresh", "err", err)
			r.st = saved{}
		}
	}
	if !r.st.HaveSettings {
		r.st.Settings = DefaultSettings()
	} else {
		r.st.Settings = r.st.Settings.clean()
	}
	if r.st.Printers == nil {
		r.st.Printers = map[string]*pstate{}
	}
	return r
}

// SetSettings takes the monitor block of a sync answer.
func (r *Runner) SetSettings(s Settings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.st.Settings.Enabled
	r.st.Settings, r.st.HaveSettings = s.clean(), true
	for _, ps := range r.st.Printers {
		ps.CamBlocked = false
	}
	if was != s.Enabled {
		r.cfg.Log.Info("monitoring settings changed", "enabled", s.Enabled)
	}
	r.saveLocked(true)
}

// Settings returns the settings in use.
func (r *Runner) Settings() Settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Settings
}

// Status is one printer's monitoring state for the page and for sync.
type Status struct {
	Camera      string
	CamReason   string
	LastFrameAt time.Time
	OpenAlerts  int
	Waiting     int // alerts not yet delivered to Spoken Parts
}

// Status returns the state for serial.
func (r *Runner) Status(serial string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := r.st.Printers[serial]
	if ps == nil {
		ps = &pstate{}
	}
	st := Status{Camera: r.cameraStateLocked(serial, ps), CamReason: ps.CamReason, LastFrameAt: ps.LastFrameAt, OpenAlerts: len(ps.Open)}
	for _, b := range ps.Batches {
		st.Waiting += len(b.Alerts)
	}
	return st
}

// Enabled reports whether the site turned monitoring on.
func (r *Runner) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Settings.Enabled
}

func (r *Runner) cameraStateLocked(serial string, ps *pstate) string {
	on, _ := r.st.Settings.camera(serial)
	switch {
	case !on || ps.CamBlocked:
		return CamOff
	case ps.Last != nil && ps.Last.RTSP == "off":
		return CamStreamOff
	case ps.Mem.CamFails > 0:
		return CamFailing
	case !ps.LastFrameAt.IsZero():
		return CamOK
	}
	return CamWaiting
}

// Reset forgets everything (the link was removed).
func (r *Runner) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st = saved{Settings: DefaultSettings(), Printers: map[string]*pstate{}}
	r.saveLocked(true)
	if r.cfg.FramesDir != "" {
		if err := os.RemoveAll(r.cfg.FramesDir); err != nil {
			r.cfg.Log.Warn("could not delete saved camera frames", "err", err)
		}
	}
}

// Run ticks until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	t := time.NewTicker(r.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.wg.Wait()
			r.mu.Lock()
			r.saveLocked(true)
			r.mu.Unlock()
			return
		case <-t.C:
			r.Tick()
		}
	}
}

// Tick runs the rules once for every printer.
func (r *Runner) Tick() {
	feeds := r.cfg.Source.Feeds()
	now := r.cfg.Now()
	defer r.ticks.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	S := r.st.Settings
	R := S.Rules
	for _, f := range feeds {
		if !serialRe.MatchString(f.Serial) {
			continue
		}
		ps := r.stateLocked(f.Serial)
		if f.Name != "" {
			ps.Name = f.Name
		}
		if !S.Enabled {
			// Off: nothing is watched, and turning it on later starts
			// from a fresh snapshot instead of a stale one.
			ps.Last, ps.Mem = nil, Memory{}
			continue
		}
		camOn, interval := S.camera(f.Serial)
		p := Printer{Serial: f.Serial, Name: ps.Name, Camera: camOn}
		fresh := f.Connected && f.Report != nil && !f.LastReport.IsZero() && now.Sub(f.LastReport) <= r.cfg.StaleAfter
		var res Result
		if fresh || ps.Last != nil {
			// Contact can only be lost after the first report.
			res = Feed(p, ps.Last, fresh, &ps.Mem, R)
		}
		var cur *Snapshot
		if fresh {
			cur = FromReport(f.Report, now)
			ev := Evaluate(p, ps.Last, cur, &ps.Mem, R)
			res.Events = append(res.Events, ev.Events...)
			res.Cleared = append(res.Cleared, ev.Cleared...)
			res.Reset = res.Reset || ev.Reset
			ps.Last = cur
		}
		r.applyLocked(f.Serial, ps, res, ps.Last, now)
		if fresh && camOn && !ps.CamBlocked && cur.Printing() && cur.RTSP != "off" &&
			now.Sub(ps.LastGrabAt) >= time.Duration(interval)*time.Second {
			ps.LastGrabAt = now
			ps.GrabDue = true
		}
		if ps.GrabDue || len(ps.Batches) > 0 {
			r.wakeLocked(f.Serial)
		}
	}
	r.saveLocked(false)
}

func (r *Runner) stateLocked(serial string) *pstate {
	ps := r.st.Printers[serial]
	if ps == nil {
		ps = &pstate{}
		r.st.Printers[serial] = ps
	}
	if ps.Sent == nil {
		ps.Sent = map[string]sentMark{}
	}
	if ps.Open == nil {
		ps.Open = map[string]Severity{}
	}
	return ps
}

// applyLocked de-duplicates the events (a key is not sent again within the
// cooldown unless its severity rose) and queues what is left.
func (r *Runner) applyLocked(serial string, ps *pstate, res Result, last *Snapshot, now time.Time) {
	if len(res.Events) == 0 && len(res.Cleared) == 0 && !res.Reset {
		return
	}
	cool := time.Duration(r.st.Settings.Rules.RepeatCooldownMin) * time.Minute
	camOn, _ := r.st.Settings.camera(serial)
	b := &batch{Reset: res.Reset}
	if res.Reset {
		ps.Sent, ps.Open = map[string]sentMark{}, map[string]Severity{}
	}
	for _, key := range res.Cleared {
		delete(ps.Open, key)
		b.Cleared = append(b.Cleared, key)
	}
	tail := ssdp.Tail(serial)
	for _, e := range res.Events {
		if s, ok := ps.Sent[e.Key]; ok && now.Sub(s.At) < cool && e.Severity.Rank() <= s.Severity.Rank() {
			r.cfg.Log.Debug("alert held back by the cooldown", "serial_tail", tail, "key", e.Key)
			continue
		}
		ps.Sent[e.Key] = sentMark{At: now, Severity: e.Severity}
		if e.Severity != Info {
			ps.Open[e.Key] = e.Severity
		}
		a := api.Alert{
			ID: newID(), TS: now.Format(time.RFC3339), Severity: string(e.Severity), Key: e.Key,
			Message: e.Message, Data: e.Data, Job: jobOf(last),
		}
		if a.Data == nil {
			a.Data = map[string]any{}
		}
		p := &pending{Alert: a}
		if camOn && !ps.CamBlocked && wantsFrame(e) && (last == nil || last.RTSP != "off") {
			switch e.Key {
			case "state:FINISH":
				p.WantFrame = "finish"
			case "state:FAILED":
				p.WantFrame = "failed"
			default:
				p.WantFrame = "alert"
			}
		}
		b.Alerts = append(b.Alerts, p)
		r.cfg.Log.Info("printer alert", "serial_tail", tail, "severity", string(e.Severity), "key", e.Key)
	}
	if len(b.Alerts) == 0 && len(b.Cleared) == 0 && !b.Reset {
		return
	}
	// Join with the last waiting batch when that keeps the order right.
	if n := len(ps.Batches); n > 0 && !b.Reset && len(ps.Batches[n-1].Alerts)+len(b.Alerts) <= maxBatchSize {
		lb := ps.Batches[n-1]
		lb.Alerts = append(lb.Alerts, b.Alerts...)
		for _, c := range b.Cleared {
			if !containsStr(lb.Cleared, c) {
				lb.Cleared = append(lb.Cleared, c)
			}
		}
	} else {
		ps.Batches = append(ps.Batches, b)
	}
	if len(ps.Batches) > maxBatches {
		// Offline for a long time: keep the newest, and say so.
		drop := len(ps.Batches) - maxBatches
		r.cfg.Log.Warn("Spoken Parts unreachable for a long time, dropping the oldest waiting alerts", "serial_tail", tail, "batches", drop)
		ps.Batches = ps.Batches[drop:]
	}
	r.saveLocked(true)
}

// wantsFrame: a still goes with every alert a person should look at, and
// with the finish, but not with camera or feed trouble.
func wantsFrame(e Event) bool {
	if strings.HasPrefix(e.Key, "camera_") || strings.HasPrefix(e.Key, "feed_") || e.Key == "xcam_settings" {
		return false
	}
	return e.Severity != Info || e.Key == "state:FINISH"
}

func jobOf(s *Snapshot) api.Job {
	if s == nil {
		return api.Job{}
	}
	return api.Job{Task: s.Task, TaskID: s.TaskID, GcodeState: s.GcodeState, Layer: cloneInt(s.Layer),
		TotalLayers: cloneInt(s.TotalLayers), Percent: cloneInt(s.Percent), RemainingMin: cloneInt(s.RemainingMin)}
}

func (r *Runner) wakeLocked(serial string) {
	if r.ctx == nil {
		return // not running (tests call Work directly)
	}
	ch, ok := r.workers[serial]
	if !ok {
		ch = make(chan struct{}, 1)
		r.workers[serial] = ch
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case <-r.ctx.Done():
					return
				case <-ch:
					r.Work(r.ctx, serial)
				}
			}
		}()
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Work does what is waiting for one printer: a due camera frame, then the
// waiting alerts (each with a fresh frame when it wants one), the phone
// notifications, and the alerts call. Only one Work runs per printer.
func (r *Runner) Work(ctx context.Context, serial string) {
	r.mu.Lock()
	ps := r.st.Printers[serial]
	if ps == nil {
		r.mu.Unlock()
		return
	}
	due := ps.GrabDue
	ps.GrabDue = false
	r.mu.Unlock()

	if due {
		r.grabAndSend(ctx, serial, "interval", "")
	}
	// Frames and phone notifications first, for every waiting batch: they
	// do not wait for Spoken Parts (the phone works with the internet down).
	r.mu.Lock()
	batches := append([]*batch(nil), ps.Batches...)
	r.mu.Unlock()
	for _, b := range batches {
		if ctx.Err() != nil {
			return
		}
		r.prepare(ctx, serial, b)
	}
	for ctx.Err() == nil {
		r.mu.Lock()
		if len(ps.Batches) == 0 || r.cfg.Now().Before(ps.RetryAt) {
			r.mu.Unlock()
			return
		}
		b := ps.Batches[0]
		r.mu.Unlock()
		r.prepare(ctx, serial, b)

		r.mu.Lock()
		call := api.AlertBatch{Cleared: append([]string(nil), b.Cleared...), Reset: b.Reset}
		for _, p := range b.Alerts {
			call.Alerts = append(call.Alerts, p.Alert)
		}
		r.mu.Unlock()
		err := r.cfg.Uplink.Alerts(ctx, serial, call)
		r.mu.Lock()
		switch {
		case err == nil || badRequest(err):
			if err != nil {
				r.cfg.Log.Warn("Spoken Parts refused an alerts call, dropping it", "serial_tail", ssdp.Tail(serial), "err", err)
			}
			if len(ps.Batches) > 0 && ps.Batches[0] == b {
				ps.Batches = ps.Batches[1:]
			}
			ps.BatchFailure = 0
			r.saveLocked(true)
			r.mu.Unlock()
		default:
			ps.BatchFailure++
			wait := max(api.Backoff(ps.BatchFailure, r.cfg.Tick, 10*time.Minute), api.RetryAfter(err))
			ps.RetryAt = r.cfg.Now().Add(wait)
			r.cfg.Log.Debug("alerts call failed, trying again later", "serial_tail", ssdp.Tail(serial), "err", err)
			r.mu.Unlock()
			return
		}
	}
}

// prepare grabs and uploads one frame for the alerts of b that want one,
// then sends their phone notifications. Each happens once per alert.
func (r *Runner) prepare(ctx context.Context, serial string, b *batch) {
	r.mu.Lock()
	var want *pending
	for _, p := range b.Alerts {
		if p.WantFrame != "" && !p.FrameTried {
			want = p
			break
		}
	}
	notify := false
	for _, p := range b.Alerts {
		notify = notify || !p.Notified
	}
	r.mu.Unlock()
	if want == nil && !notify {
		return
	}
	var imageURL string
	if want != nil {
		ans := r.grabAndSend(ctx, serial, want.WantFrame, want.Alert.Key)
		r.mu.Lock()
		for _, p := range b.Alerts {
			if p.WantFrame != "" && !p.FrameTried {
				p.FrameTried = true
				if ans != nil && ans.FrameID != "" {
					id := ans.FrameID
					p.Alert.FrameID = &id
				}
			}
		}
		r.mu.Unlock()
		if ans != nil {
			imageURL = ans.ImageURL
		}
	}
	r.notify(ctx, serial, b, imageURL)
}

func badRequest(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500 && e.Status != 401 && e.Status != 403 && e.Status != 408 && e.Status != 429
}

// grabAndSend takes one frame, keeps it on disk and uploads it. The camera
// rule (failures in a row) is checked here. It returns the upload answer, or
// nil.
func (r *Runner) grabAndSend(ctx context.Context, serial, reason, alertKey string) *api.FrameAnswer {
	gctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	jpeg, err := r.cfg.Source.Grab(gctx, serial)
	cancel()
	now := r.cfg.Now()
	r.mu.Lock()
	ps := r.stateLocked(serial)
	p := Printer{Serial: serial, Name: ps.Name, Camera: true}
	why := ""
	if err != nil {
		why = grabReason(err)
		ps.CamReason = why
	} else {
		ps.CamReason = ""
		ps.LastFrameAt = now
	}
	res := Camera(p, err == nil, why, &ps.Mem, r.st.Settings.Frames.FailAlertAfter)
	r.applyLocked(serial, ps, res, ps.Last, now)
	last := ps.Last
	keep := r.st.Settings.Frames.Keep
	r.mu.Unlock()
	if err != nil {
		r.cfg.Log.Info("camera frame failed", "serial_tail", ssdp.Tail(serial), "reason", why)
		return nil
	}
	id := newID()
	r.keepFrame(serial, id, now, jpeg, keep)
	m := api.FrameMeta{ID: id, TakenAt: now, Reason: reason, AlertKey: alertKey}
	if last != nil {
		m.Printing, m.TaskID, m.Layer, m.TotalLayers = last.Printing(), last.TaskID, last.Layer, last.TotalLayers
	}
	ans, err := r.cfg.Uplink.Frame(ctx, serial, m, jpeg)
	if err != nil {
		if api.HasCode(err, api.CodeCameraOff) {
			r.mu.Lock()
			ps.CamBlocked = true
			r.mu.Unlock()
			r.cfg.Log.Info("Spoken Parts does not want frames from this printer right now", "serial_tail", ssdp.Tail(serial))
		} else {
			r.cfg.Log.Debug("frame upload failed", "serial_tail", ssdp.Tail(serial), "err", err)
		}
		return nil
	}
	return ans
}

// grabReason turns a grab error into a few plain words for the page and the
// camera alert. The error text never holds a secret (the RTSP client and
// the camera code make sure of that), but it is cut short anyway.
func grabReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// keepFrame writes the frame to the printer's folder (mode 0600) and keeps
// only the newest keep frames.
func (r *Runner) keepFrame(serial, id string, at time.Time, jpeg []byte, keep int) {
	if r.cfg.FramesDir == "" {
		return
	}
	dir := filepath.Join(r.cfg.FramesDir, serial)
	name := at.UTC().Format("20060102-150405") + "-" + id[:8] + ".jpg"
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, name), jpeg); err != nil {
		r.cfg.Log.Warn("could not save a camera frame", "serial_tail", ssdp.Tail(serial), "err", err)
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	sort.Strings(files)
	for len(files) > keep {
		_ = os.Remove(files[0])
		files = files[1:]
	}
}

func (r *Runner) notify(ctx context.Context, serial string, b *batch, imageURL string) {
	if r.cfg.Notify == nil {
		return
	}
	ready, minSev := r.cfg.Notify.Ready()
	r.mu.Lock()
	var todo []*pending
	for _, p := range b.Alerts {
		if !p.Notified {
			p.Notified = true
			if ready && Severity(p.Alert.Severity).Rank() >= minSev.Rank() {
				todo = append(todo, p)
			}
		}
	}
	name := serial
	if ps := r.st.Printers[serial]; ps != nil && ps.Name != "" {
		name = ps.Name
	}
	r.mu.Unlock()
	for _, p := range todo {
		n := Notice{
			Title: "Spoken Parts: " + name, Message: p.Alert.Message, Severity: p.Alert.Severity,
			Key: p.Alert.Key, Printer: name,
		}
		if p.Alert.FrameID != nil {
			n.ImageURL = imageURL
		}
		if err := r.cfg.Notify.Notify(ctx, n); err != nil {
			r.cfg.Log.Warn("phone notification through Home Assistant failed", "serial_tail", ssdp.Tail(serial), "err", err)
		}
	}
}

// saveLocked writes the state file, at most every 10 s unless force.
func (r *Runner) saveLocked(force bool) {
	if r.cfg.StatePath == "" {
		return
	}
	now := r.cfg.Now()
	if !force && now.Sub(r.lastSave) < 10*time.Second {
		return
	}
	r.lastSave = now
	if err := store.SaveJSON(r.cfg.StatePath, r.st); err != nil {
		r.cfg.Log.Warn("could not save the monitor state", "err", err)
	}
}

// Save writes the state now (shutdown).
func (r *Runner) Save() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveLocked(true)
}

// Forget drops what is kept for a printer (removed on the link page).
func (r *Runner) Forget(serial string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.st.Printers, serial)
	r.saveLocked(true)
	if r.cfg.FramesDir != "" && serialRe.MatchString(serial) {
		_ = os.RemoveAll(filepath.Join(r.cfg.FramesDir, serial))
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

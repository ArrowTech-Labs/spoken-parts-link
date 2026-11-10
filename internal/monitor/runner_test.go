package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
)

var tinyJPEG = []byte{0xFF, 0xD8, 0xFF, 0xDB, 0x00, 0x02, 0xFF, 0xD9}

type fakeSource struct {
	mu      sync.Mutex
	feeds   []PrinterFeed
	grabs   int
	grabErr error
}

func (f *fakeSource) Feeds() []PrinterFeed {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PrinterFeed, len(f.feeds))
	copy(out, f.feeds)
	return out
}

func (f *fakeSource) Grab(ctx context.Context, serial string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grabs++
	if f.grabErr != nil {
		return nil, f.grabErr
	}
	return tinyJPEG, nil
}

type fakeUplink struct {
	mu      sync.Mutex
	frames  []api.FrameMeta
	batches []api.AlertBatch
	fail    error
	frameN  int
}

func (u *fakeUplink) Frame(ctx context.Context, serial string, m api.FrameMeta, jpeg []byte) (*api.FrameAnswer, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.frames = append(u.frames, m)
	u.frameN++
	ans := &api.FrameAnswer{FrameID: m.ID}
	if m.Reason != "interval" {
		ans.ImageURL = "https://spokenparts.test/f/" + m.ID
	}
	return ans, nil
}

func (u *fakeUplink) Alerts(ctx context.Context, serial string, b api.AlertBatch) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail != nil {
		return u.fail
	}
	u.batches = append(u.batches, b)
	return nil
}

func (u *fakeUplink) sent() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, b := range u.batches {
		if b.Reset {
			out = append(out, "reset")
		}
		for _, a := range b.Alerts {
			s := a.Severity + " " + a.Key
			if a.FrameID != nil {
				s += " +frame"
			}
			out = append(out, s)
		}
		for _, c := range b.Cleared {
			out = append(out, "cleared "+c)
		}
	}
	return out
}

type fakeNotify struct {
	mu      sync.Mutex
	notices []Notice
}

func (n *fakeNotify) Ready() (bool, Severity) { return true, Warning }
func (n *fakeNotify) Notify(ctx context.Context, x Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notices = append(n.notices, x)
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func onSettings() Settings {
	return Settings{Enabled: true, Printers: map[string]PrinterSettings{h2d.Serial: {Camera: true}}}
}

type harness struct {
	r   *Runner
	src *fakeSource
	up  *fakeUplink
	ha  *fakeNotify
	clk *clock
	dir string
}

func newHarness(t *testing.T, dir string) *harness {
	h := &harness{src: &fakeSource{}, up: &fakeUplink{}, ha: &fakeNotify{}, clk: &clock{}, dir: dir}
	h.r = New(Config{
		StatePath: filepath.Join(dir, "state", "monitor.json"),
		FramesDir: filepath.Join(dir, "frames"),
		Source:    h.src, Uplink: h.up, Notify: h.ha,
		Log: logx.New(os.Stderr, redact.New(), logx.Error), Now: h.clk.Now,
	})
	return h
}

// step sets the printer's merged report and runs one tick plus the work.
func (h *harness) step(at time.Time, merged map[string]json.RawMessage) {
	h.clk.Set(at)
	h.src.mu.Lock()
	h.src.feeds = []PrinterFeed{{Serial: h2d.Serial, Name: "H2D", Connected: true, LastReport: at, Report: merged}}
	h.src.mu.Unlock()
	h.r.Tick()
	h.r.Work(context.Background(), h2d.Serial)
}

func TestRunnerNov10(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, dir)
	h.r.SetSettings(onSettings())
	var merged map[string]json.RawMessage
	lines := loadFixture(t, "nov10_h2d.jsonl")
	for _, l := range lines {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	want := []string{
		"reset",
		"info state:STARTED",
		"critical hms:0C00_0100_0002_0017 +frame",
		"critical skip +frame",
		"critical hms:0C00_0300_0003_0008 +frame",
		// 02:35 skip: same key and severity within 30 minutes, held back.
		"info state:FINISH +frame",
		"cleared hms:0C00_0300_0003_0008",
	}
	if got := h.up.sent(); !slices.Equal(got, want) {
		t.Fatalf("sent:\n got  %q\n want %q", got, want)
	}
	// Phone: the three critical alerts, each with its frame link.
	if len(h.ha.notices) != 3 {
		t.Fatalf("notices %+v", h.ha.notices)
	}
	for _, n := range h.ha.notices {
		if n.Severity != "critical" || !strings.HasPrefix(n.ImageURL, "https://spokenparts.test/f/") || n.Title != "Spoken Parts: H2D" {
			t.Errorf("notice %+v", n)
		}
	}
	// Interval frames only while printing (stage 0), alert frames with reasons.
	var reasons []string
	for _, f := range h.up.frames {
		reasons = append(reasons, f.Reason)
		if f.Reason == "interval" && !f.Printing {
			t.Errorf("interval frame while not printing: %+v", f)
		}
	}
	if !slices.Contains(reasons, "finish") || !slices.Contains(reasons, "alert") || !slices.Contains(reasons, "interval") {
		t.Errorf("frame reasons %q", reasons)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "frames", h2d.Serial, "*.jpg"))
	if len(files) != len(h.up.frames) {
		t.Errorf("%d frames on disk, %d uploaded", len(files), len(h.up.frames))
	}
	for _, f := range files {
		st, _ := os.Stat(f)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, st.Mode())
		}
	}
	st, err := os.Stat(filepath.Join(dir, "state", "monitor.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v %v", st, err)
	}

	// Restart: a new runner with the same files sees the same report and
	// says nothing new.
	h2 := newHarness(t, dir)
	h2.step(lines[len(lines)-1].At.Add(time.Minute), merged)
	if got := h2.up.sent(); len(got) != 0 {
		t.Fatalf("restart re-alerted: %q", got)
	}
	if s := h2.r.Status(h2d.Serial); s.Camera != CamOK || s.LastFrameAt.IsZero() {
		t.Errorf("status after restart %+v", s)
	}
}

func TestRunnerOffByDefault(t *testing.T) {
	h := newHarness(t, t.TempDir())
	var merged map[string]json.RawMessage
	for _, l := range loadFixture(t, "nov10_h2d.jsonl") {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	if got := h.up.sent(); len(got) != 0 || h.src.grabs != 0 {
		t.Fatalf("monitoring is off by default, but sent %q and grabbed %d", got, h.src.grabs)
	}
	if s := h.r.Status(h2d.Serial); s.Camera != CamOff {
		t.Errorf("camera %q", s.Camera)
	}
}

func TestRunnerCameraOffNoFrames(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true})
	var merged map[string]json.RawMessage
	for _, l := range loadFixture(t, "nov10_h2d.jsonl") {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	if h.src.grabs != 0 {
		t.Fatalf("grabbed %d frames with the camera off", h.src.grabs)
	}
	if got := h.up.sent(); len(got) != 7 || strings.Contains(strings.Join(got, ","), "+frame") {
		t.Fatalf("sent %q", got)
	}
}

func TestRunnerSeverityRiseBeatsCooldown(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true})
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	ps := h.r.stateLocked(h2d.Serial)
	h.r.applyLocked(h2d.Serial, ps, Result{Events: []Event{{Severity: Warning, Key: "skip", Message: "a"}}}, nil, base)
	h.r.applyLocked(h2d.Serial, ps, Result{Events: []Event{{Severity: Warning, Key: "skip", Message: "b"}}}, nil, base.Add(5*time.Minute))
	h.r.applyLocked(h2d.Serial, ps, Result{Events: []Event{{Severity: Critical, Key: "skip", Message: "c"}}}, nil, base.Add(6*time.Minute))
	h.r.applyLocked(h2d.Serial, ps, Result{Events: []Event{{Severity: Warning, Key: "skip", Message: "d"}}}, nil, base.Add(40*time.Minute))
	var msgs []string
	for _, b := range ps.Batches {
		for _, p := range b.Alerts {
			msgs = append(msgs, p.Alert.Message)
		}
	}
	if !slices.Equal(msgs, []string{"a", "c", "d"}) {
		t.Fatalf("%q", msgs)
	}
}

func TestRunnerOfflineThenDelivers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true})
	h.up.fail = &api.Error{Status: 503}
	var merged map[string]json.RawMessage
	lines := loadFixture(t, "nov10_h2d.jsonl")
	for _, l := range lines[:8] {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	if len(h.up.sent()) != 0 || h.r.Status(h2d.Serial).Waiting != 2 {
		t.Fatalf("waiting %+v", h.r.Status(h2d.Serial))
	}
	// Phone notifications do not wait for Spoken Parts.
	if len(h.ha.notices) != 1 {
		t.Fatalf("notices %d", len(h.ha.notices))
	}
	h.up.fail = nil
	h.clk.Set(lines[7].At.Add(30 * time.Minute))
	h.r.Work(context.Background(), h2d.Serial)
	if got := h.up.sent(); !slices.Equal(got, []string{"reset", "info state:STARTED", "critical hms:0C00_0100_0002_0017"}) {
		t.Fatalf("after reconnect %q", got)
	}
	if len(h.ha.notices) != 1 {
		t.Fatalf("notified twice: %d", len(h.ha.notices))
	}
}

func TestRunnerFeedLost(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true})
	var merged map[string]json.RawMessage
	lines := loadFixture(t, "nov10_h2d.jsonl")
	for _, l := range lines[:5] {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	last := lines[4].At
	for i := 1; i <= 5; i++ {
		at := last.Add(time.Duration(i) * time.Minute)
		h.clk.Set(at)
		h.src.mu.Lock()
		h.src.feeds = []PrinterFeed{{Serial: h2d.Serial, Name: "H2D", Connected: false, LastReport: last, Report: merged}}
		h.src.mu.Unlock()
		h.r.Tick()
		h.r.Work(context.Background(), h2d.Serial)
	}
	h.step(last.Add(6*time.Minute), merged)
	got := h.up.sent()
	if !slices.Contains(got, "critical feed_lost") || !slices.Contains(got, "info feed_back") || !slices.Contains(got, "cleared feed_lost") {
		t.Fatalf("%q", got)
	}
}

func TestRunnerCameraFailures(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true, Frames: FrameSettings{IntervalS: 30}, Printers: map[string]PrinterSettings{h2d.Serial: {Camera: true}}})
	h.src.grabErr = errors.New("camera answered 401 after login")
	var merged map[string]json.RawMessage
	lines := loadFixture(t, "nov10_h2d.jsonl")
	for _, l := range lines[:7] {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	st := h.r.Status(h2d.Serial)
	if st.Camera != CamFailing || st.CamReason != "camera answered 401 after login" {
		t.Fatalf("status %+v", st)
	}
	if got := h.up.sent(); !slices.Contains(got, "warning camera_fail") {
		t.Fatalf("%q", got)
	}
	h.src.grabErr = nil
	for _, l := range lines[7:10] {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	if got := h.up.sent(); !slices.Contains(got, "info camera_back") || !slices.Contains(got, "cleared camera_fail") {
		t.Fatalf("%q", got)
	}
}

func TestRunnerCameraOff409(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(onSettings())
	h.r.cfg.Uplink = refuseFrames{h.up}
	var merged map[string]json.RawMessage
	lines := loadFixture(t, "nov10_h2d.jsonl")
	for _, l := range lines[:6] {
		merged, _ = Merge(merged, l.Payload)
		h.step(l.At, merged)
	}
	if h.src.grabs != 1 {
		t.Fatalf("kept grabbing after camera_off: %d", h.src.grabs)
	}
	if h.r.Status(h2d.Serial).Camera != CamOff {
		t.Fatal("status")
	}
	h.r.SetSettings(onSettings()) // the next sync says on again
	if h.r.Status(h2d.Serial).Camera == CamOff {
		t.Fatal("still off after new settings")
	}
}

type refuseFrames struct{ *fakeUplink }

func (refuseFrames) Frame(context.Context, string, api.FrameMeta, []byte) (*api.FrameAnswer, error) {
	return nil, &api.Error{Status: 409, Code: api.CodeCameraOff}
}

func TestRunnerKeepsNewestFrames(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, dir)
	h.r.SetSettings(Settings{Enabled: true, Frames: FrameSettings{Keep: 3}, Printers: map[string]PrinterSettings{h2d.Serial: {Camera: true}}})
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for i := range 6 {
		h.r.keepFrame(h2d.Serial, newID(), base.Add(time.Duration(i)*time.Minute), tinyJPEG, 3)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "frames", h2d.Serial, "*.jpg"))
	if len(files) != 3 || !strings.Contains(files[0], "090300") {
		t.Fatalf("%q", files)
	}
	h.r.Forget(h2d.Serial)
	if _, err := os.Stat(filepath.Join(dir, "frames", h2d.Serial)); !os.IsNotExist(err) {
		t.Fatal("frames kept after forget")
	}
}

func TestParseSettings(t *testing.T) {
	s, ok := ParseSettings(json.RawMessage(`{"enabled":true,"rules":{"stall_min":1},"frames":{"interval_s":5,"keep":9999},
	 "printers":{"0947CJ641600041":{"camera":true,"frame_interval_s":10},"bad serial!":{"camera":true}}}`))
	if !ok || !s.Enabled || s.Rules.StallMin != 5 || s.Frames.IntervalS != 30 || s.Frames.Keep != maxKeep || len(s.Printers) != 1 {
		t.Fatalf("%+v", s)
	}
	if on, iv := s.camera("0947CJ641600041"); !on || iv != 30 {
		t.Fatalf("camera %v %d", on, iv)
	}
	if _, ok := ParseSettings(nil); ok {
		t.Fatal("nil accepted")
	}
	if d := DefaultSettings(); d.Enabled {
		t.Fatal("on by default")
	}
}

func TestRunnerNoFeedLostBeforeFirstReport(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.r.SetSettings(Settings{Enabled: true})
	for i := range 10 {
		h.clk.Set(time.Date(2026, 11, 10, 9, i, 0, 0, time.UTC))
		h.src.mu.Lock()
		h.src.feeds = []PrinterFeed{{Serial: h2d.Serial, Name: "H2D"}}
		h.src.mu.Unlock()
		h.r.Tick()
		h.r.Work(context.Background(), h2d.Serial)
	}
	if got := h.up.sent(); len(got) != 0 {
		t.Fatalf("never heard, but sent %q", got)
	}
}

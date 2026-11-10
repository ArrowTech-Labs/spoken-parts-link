package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/camera"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/rtsp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const webhookID = "spokenparts-e2e-webhook-0123456789abcdef" // made up for this test

// monitorAPI answers the 0.3 frames and alerts calls like the backend.
type monitorAPI struct {
	mu      sync.Mutex
	frames  []recorded
	batches []api.AlertBatch
}

func (m *monitorAPI) serve(w http.ResponseWriter, r *http.Request, body []byte, _ *fakeLink) bool {
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/frames") && strings.HasPrefix(r.URL.Path, "/api/link/v1/printers/"+serial):
		if r.Header.Get("Content-Type") != "image/jpeg" || len(body) > api.MaxFrame || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":"invalid"}`))
			return true
		}
		m.mu.Lock()
		m.frames = append(m.frames, recorded{Method: r.Method, Path: r.URL.Path, Body: body, Query: r.URL.RawQuery})
		m.mu.Unlock()
		q := r.URL.Query()
		img := "null"
		if q.Get("reason") != "interval" {
			img = `"https://spokenparts.test/frames/` + q.Get("id") + `/image?sig=x"`
		}
		w.WriteHeader(201)
		_, _ = fmt.Fprintf(w, `{"frame_id":%q,"duplicate":false,"image_url":%s}`, q.Get("id"), img)
		return true
	case r.Method == "POST" && r.URL.Path == "/api/link/v1/printers/"+serial+"/alerts":
		var b api.AlertBatch
		if err := json.Unmarshal(body, &b); err != nil {
			w.WriteHeader(400)
			return true
		}
		m.mu.Lock()
		m.batches = append(m.batches, b)
		m.mu.Unlock()
		_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"alerts":[]}`))
		return true
	}
	return false
}

func (m *monitorAPI) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, b := range m.batches {
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

// fakeHA is Home Assistant's webhook endpoint.
type fakeHA struct {
	mu      sync.Mutex
	paths   []string
	notices []map[string]any
}

func (f *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var n map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&n)
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.notices = append(f.notices, n)
	f.mu.Unlock()
	w.WriteHeader(200)
}

// grabber is the real camera grabber when ffmpeg is installed. Without
// ffmpeg it still runs the real RTSPS session (auth, H.264 key frame) and
// returns a stand-in JPEG, so the test runs everywhere.
type grabber struct {
	real *camera.Grabber
	mu   sync.Mutex
	cmds [][]string // argv and env of every ffmpeg run
}

func (g *grabber) Grab(ctx context.Context, serial string, conn net.Conn, host, code string) ([]byte, error) {
	if g.real != nil {
		return g.real.Grab(ctx, serial, conn, host, code)
	}
	au, err := rtsp.GrabIDR(ctx, conn, host, "/streaming/live/1", "bblp", []byte(code), 10*time.Second)
	if err != nil {
		return nil, err
	}
	if len(au) == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	return []byte{0xFF, 0xD8, 0xFF, 0xDB, 0x00, 0x02, 0xFF, 0xD9}, nil
}

type monitorWorld struct {
	h      *harness
	be     *backend
	ma     *monitorAPI
	ha     *fakeHA
	broker *testutil.Broker
	cam    *testutil.RTSPServer
	g      *grabber
	dir    string
	logs   *lockedBuf
	mods   []func(*Config)
	ca     *testutil.CA
	t      *testing.T
}

func newMonitorWorld(t *testing.T) *monitorWorld {
	t.Helper()
	w := &monitorWorld{t: t, dir: t.TempDir(), be: newBackend(t), ma: &monitorAPI{}, ha: &fakeHA{}, logs: &lockedBuf{}}
	w.be.extra = w.ma.serve
	w.be.monitor = `{"enabled":true,"rules":{},"frames":{"interval_s":30,"keep":60,"fail_alert_after":3},"printers":{"` + serial + `":{"camera":true}}}`
	srv := httptestServer(t, w.be)
	haSrv := httptest.NewServer(w.ha)
	t.Cleanup(haSrv.Close)
	w.ca = testutil.NewCA(t, "Test Printer CA")
	first := loadNov10(t)[0]
	w.broker = testutil.NewBroker(t, w.ca.Leaf(t, serial), serial, accessCode, first)
	w.cam = testutil.NewRTSPServer(t, w.ca.Leaf(t, serial), testutil.RTSPOptions{Code: accessCode, Qop: true})
	w.g = &grabber{}
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		w.g.real = &camera.Grabber{FFmpeg: ff, Run: func(c *exec.Cmd) error {
			w.g.mu.Lock()
			w.g.cmds = append(w.g.cmds, append(append([]string(nil), c.Args...), c.Env...))
			w.g.mu.Unlock()
			return c.Run()
		}}
	}
	w.mods = []func(*Config){func(c *Config) {
		c.MonitorTick = 20 * time.Millisecond
		c.HAURL = haSrv.URL
		c.Grabber = w.g
		c.PrinterDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			switch addr {
			case "192.168.1.40:8883":
				return d.DialContext(ctx, network, w.broker.Addr)
			case "192.168.1.40:322":
				return d.DialContext(ctx, network, w.cam.Addr)
			}
			return nil, fmt.Errorf("dialed %s", addr)
		}
	}}
	w.h = newHarness(t, w.dir, w.be, srv, w.broker, w.ca, w.logs, w.mods...)
	w.h.post("/pair", url.Values{"code": {pairCode}})
	w.be.setAll("active")
	waitFor(t, "ready", func() bool { return w.h.app.View().CodesReady })
	waitFor(t, "monitor on", func() bool { return w.h.app.monitor.Enabled() })
	w.h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(w.h.app.View().Printers) == 1 })
	w.h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}})
	waitFor(t, "printer report", func() bool {
		p := w.h.app.View().Printers[0]
		return p.Access == printers.AccessOK && p.Connected && p.GcodeState == "FINISH"
	})
	t.Cleanup(func() { w.h.stop() })
	return w
}

func loadNov10(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("../monitor/testdata/nov10_h2d.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var l struct {
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && len(l.Payload) > 0 {
			out = append(out, append([]byte(nil), l.Payload...))
		}
	}
	return out
}

// push sends one report and waits until the monitor has looked at it twice.
func (w *monitorWorld) push(payload []byte) {
	w.t.Helper()
	var want struct {
		Print map[string]json.RawMessage `json:"print"`
	}
	_ = json.Unmarshal(payload, &want)
	w.broker.Push(payload)
	waitFor(w.t, "report applied", func() bool {
		for _, f := range w.h.app.printers.Feeds() {
			if f.Serial != serial {
				continue
			}
			for k, v := range want.Print {
				var a, b any
				_ = json.Unmarshal(v, &a)
				_ = json.Unmarshal(f.Report[k], &b)
				if fmt.Sprint(a) != fmt.Sprint(b) {
					return false
				}
			}
			return true
		}
		return false
	})
	t0 := w.h.app.monitor.Ticks()
	waitFor(w.t, "monitor ticks", func() bool { return w.h.app.monitor.Ticks() >= t0+2 })
	waitFor(w.t, "work done", func() bool { return w.h.app.monitor.Status(serial).Waiting == 0 })
}

// The Nov 10 incident replayed through the fake printer: the alerts reach
// Spoken Parts with camera frames from the fake RTSPS camera, the phone gets
// the critical ones, and no secret goes anywhere.
func TestMonitorNov10EndToEnd(t *testing.T) {
	w := newMonitorWorld(t)
	w.h.post("/ha", url.Values{"webhook": {webhookID}, "level": {"critical"}})
	waitFor(t, "ha set", func() bool { return w.h.app.View().HASet })

	lines := loadNov10(t)
	for _, p := range lines[1:] {
		w.push(p)
	}
	want := []string{
		"reset",
		"info state:STARTED",
		"critical hms:0C00_0100_0002_0017 +frame",
		"critical skip +frame",
		"critical hms:0C00_0300_0003_0008 +frame",
		"info state:FINISH +frame",
		"cleared hms:0C00_0300_0003_0008",
	}
	if got := w.ma.sent(); !slices.Equal(got, want) {
		t.Fatalf("alerts:\n got  %q\n want %q\n%s", got, want, w.logs.String())
	}

	// Frames: real JPEGs over the signed API, with their reasons.
	w.ma.mu.Lock()
	frames := append([]recorded(nil), w.ma.frames...)
	w.ma.mu.Unlock()
	var reasons []string
	for _, f := range frames {
		q, _ := url.ParseQuery(f.Query)
		reasons = append(reasons, q.Get("reason"))
		if !bytes.HasPrefix(f.Body, []byte{0xFF, 0xD8}) || !bytes.HasSuffix(f.Body, []byte{0xFF, 0xD9}) {
			t.Errorf("frame is not a JPEG")
		}
	}
	for _, r := range []string{"interval", "alert", "finish"} {
		if !slices.Contains(reasons, r) {
			t.Errorf("no %s frame in %q", r, reasons)
		}
	}
	if w.g.real != nil {
		w.g.mu.Lock()
		for _, c := range w.g.cmds {
			joined := strings.Join(c, " ")
			if strings.Contains(joined, accessCode) || strings.Contains(joined, "rtsp") || strings.Contains(joined, "127.0.0.1") {
				t.Errorf("ffmpeg saw a secret or a URL: %q", joined)
			}
		}
		if len(w.g.cmds) == 0 {
			t.Error("ffmpeg never ran")
		}
		w.g.mu.Unlock()
	}

	// Phone: the three critical alerts, with the frame link.
	w.ha.mu.Lock()
	if len(w.ha.notices) != 3 {
		t.Errorf("phone got %d notices", len(w.ha.notices))
	}
	for i, n := range w.ha.notices {
		if w.ha.paths[i] != "/api/webhook/"+webhookID || n["severity"] != "critical" || !strings.HasPrefix(fmt.Sprint(n["image_url"]), "https://spokenparts.test/frames/") {
			t.Errorf("notice %d: %s %v", i, w.ha.paths[i], n)
		}
	}
	w.ha.mu.Unlock()

	// The page and the sync say the camera works.
	if page := w.h.get("/"); !strings.Contains(page, "Last frame") {
		t.Error("page does not show the camera")
	}
	waitFor(t, "sync camera ok", func() bool {
		s := w.be.lastSync()
		return s != nil && len(s.Printers) == 1 && s.Printers[0].Monitor != nil && s.Printers[0].Monitor.Camera == "ok" && s.MonitorVersion == 1
	})

	// The MQTT side stayed read only: one pushall, nothing else.
	for _, p := range w.broker.Publishes() {
		if p.Payload != mqtt.PushAllPayload {
			t.Fatalf("published %q", p.Payload)
		}
	}

	// Restart: same report, no new alerts.
	before := len(w.ma.sent())
	// On reconnect the fake printer answers pushall with its state now.
	merged := map[string]json.RawMessage{}
	for _, p := range lines {
		var m struct {
			Print map[string]json.RawMessage `json:"print"`
		}
		_ = json.Unmarshal(p, &m)
		for k, v := range m.Print {
			merged[k] = v
		}
	}
	full, _ := json.Marshal(map[string]any{"print": merged})
	w.broker.SetReports(full)
	w.h.stop()
	w.h = newHarness(t, w.dir, w.be, w.h.srv, w.broker, w.ca, w.logs, w.mods...)
	waitFor(t, "codes reopened", func() bool { return w.h.app.View().CodesReady })
	w.h.sendSSDP()
	waitFor(t, "reconnected", func() bool {
		ps := w.h.app.View().Printers
		return len(ps) == 1 && ps[0].Connected
	})
	w.push(lines[len(lines)-1])
	if got := w.ma.sent(); len(got) != before {
		t.Fatalf("restart re-alerted: %q", got[before:])
	}
	if !w.h.app.View().HASet {
		t.Fatal("webhook lost across restart")
	}
	w.h.stop()

	assertNoLeak(t, w.dir, w.be, w.logs, accessCode, pairCode, webhookID)
	if st, err := os.Stat(filepath.Join(w.dir, "secrets", "ha.enc")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("ha.enc: %v", err)
	}
	if st, err := os.Stat(filepath.Join(w.dir, "state", "monitor.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("monitor.json: %v", err)
	}
	jpgs, _ := filepath.Glob(filepath.Join(w.dir, "frames", serial, "*.jpg"))
	if len(jpgs) == 0 {
		t.Fatal("no frames kept on the link")
	}
}

// A wrong access code on the camera never shows up anywhere, and the camera
// alert comes after three failures.
func TestMonitorCameraWrongCode(t *testing.T) {
	w := newMonitorWorld(t)
	w.cam.Close()
	w.cam = testutil.NewRTSPServer(t, w.ca.Leaf(t, serial), testutil.RTSPOptions{Code: "Other123"})
	lines := loadNov10(t)
	for _, p := range lines[1:11] {
		w.push(p)
	}
	st := w.h.app.monitor.Status(serial)
	if st.Camera != "failing" {
		t.Fatalf("camera %+v", st)
	}
	if !slices.Contains(w.ma.sent(), "warning camera_fail") {
		t.Fatalf("%q", w.ma.sent())
	}
	w.h.stop()
	assertNoLeak(t, w.dir, w.be, w.logs, accessCode, pairCode)
}

// Monitoring off in Spoken Parts: no alerts, no camera.
func TestMonitorOffSendsNothing(t *testing.T) {
	w := newMonitorWorld(t)
	w.be.mu.Lock()
	w.be.monitor = `{"enabled":false}`
	w.be.mu.Unlock()
	waitFor(t, "monitor off", func() bool { return !w.h.app.monitor.Enabled() })
	for _, p := range loadNov10(t)[1:] {
		w.push(p)
	}
	if got := w.ma.sent(); len(got) != 0 {
		t.Fatalf("sent %q", got)
	}
	w.ma.mu.Lock()
	n := len(w.ma.frames)
	w.ma.mu.Unlock()
	if n != 0 || w.cam.PacketsSent() != 0 {
		t.Fatalf("frames %d packets %d", n, w.cam.PacketsSent())
	}
}

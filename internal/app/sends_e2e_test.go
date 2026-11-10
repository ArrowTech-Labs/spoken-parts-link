package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/sends"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

// ---- a fake Spoken Parts sends API (signed, behind the e2e backend) ----

type fakeSend struct {
	send      api.Send
	data      []byte
	token     string
	state     string // queued, delivered, failed
	linkState string
	badHeader bool // X-SP-SHA256 says something else
	expireOne bool // the first file call answers 403 (token expired)
}

type stateCall struct {
	ID string
	api.StateRequest
}

type sendsAPI struct {
	mu        sync.Mutex
	sends     map[string]*fakeSend
	states    []stateCall
	events    []api.Event
	fileCalls int
	n         int
}

func newSendsAPI(be *backend) *sendsAPI {
	sa := &sendsAPI{sends: map[string]*fakeSend{}}
	be.mu.Lock()
	be.extra = sa.serve
	be.mu.Unlock()
	return sa
}

const (
	fileName = "SP2-SP1052-1-A1B2.gcode.3mf"
	subtask  = "SP2-SP1052-1-A1B2"
)

// add queues a send for the test printer and returns it.
func (sa *sendsAPI) add(f testutil.SlicedFile, mods ...func(*fakeSend)) *fakeSend {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	fs := &fakeSend{
		send: api.Send{
			ID:          newUUID(),
			State:       "queued",
			Printer:     api.SendPrinter{Number: 2, Name: "H2D P1", Serial: serial, Model: "H2D", ModelCode: "O1D"},
			PlateIndex:  1,
			FileName:    fileName,
			SubtaskName: subtask,
			SHA256:      f.SHA256,
			Mapping:     []api.MapRow{{Filament: 1, Material: "PLA", Color: "#000000", AmsID: 0, SlotID: 0, Nozzle: "left", Tray: 0}},
			Options:     api.SendOptions{BedLeveling: true, LayerInspect: true, UseAMS: true},
		},
		data:  f.Data,
		state: "queued",
	}
	for _, m := range mods {
		m(fs)
	}
	sa.sends[fs.send.ID] = fs
	return fs
}

func (sa *sendsAPI) serve(w http.ResponseWriter, r *http.Request, body []byte, _ *fakeLink) bool {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/api/link/v1/sends":
		out := []api.Send{}
		for _, fs := range sa.sends {
			if fs.state != "queued" {
				continue
			}
			s := fs.send
			s.LinkState = fs.linkState
			if fs.linkState == "" || fs.linkState == "uploading" {
				sa.n++
				fs.token = fmt.Sprintf("tok-%d-%s", sa.n, newUUID())
				s.FileURL = "/api/link/v1/sends/" + s.ID + "/file"
				s.FileToken = fs.token
			}
			out = append(out, s)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sends": out})
		return true
	case r.Method == "GET" && strings.HasSuffix(path, "/file"):
		sa.fileCalls++
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/link/v1/sends/"), "/file")
		fs := sa.sends[id]
		tok := r.Header.Get("X-SP-File-Token")
		if fs == nil || tok == "" || tok != fs.token || fs.expireOne || strings.Contains(r.URL.RawQuery, tok) {
			if fs != nil {
				fs.expireOne = false
			}
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"code":"file_token_invalid"}`))
			return true
		}
		fs.token = ""
		sum := fs.send.SHA256
		if fs.badHeader {
			sum = strings.Repeat("0", 64)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-SP-SHA256", sum)
		_, _ = w.Write(fs.data)
		return true
	case r.Method == "POST" && strings.HasSuffix(path, "/state"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/link/v1/sends/"), "/state")
		fs := sa.sends[id]
		var req api.StateRequest
		if fs == nil || json.Unmarshal(body, &req) != nil {
			w.WriteHeader(404)
			return true
		}
		sa.states = append(sa.states, stateCall{id, req})
		switch req.State {
		case "delivered":
			if fs.state != "queued" {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"code":"send_closed"}`))
				return true
			}
			if req.SHA256 != fs.send.SHA256 {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"code":"sha_mismatch"}`))
				return true
			}
			fs.state, fs.linkState = "delivered", "started"
		case "failed":
			fs.state = "failed"
		case "started":
		default:
			fs.linkState = req.State
		}
		_, _ = w.Write([]byte(`{}`))
		return true
	case r.Method == "POST" && path == "/api/link/v1/printers/"+serial+"/events":
		var req struct {
			Events []api.Event `json:"events"`
		}
		_ = json.Unmarshal(body, &req)
		sa.events = append(sa.events, req.Events...)
		_, _ = w.Write([]byte(`{"results":[]}`))
		return true
	}
	return false
}

func (sa *sendsAPI) statesOf(id string) []string {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	var out []string
	for _, s := range sa.states {
		if s.ID == id {
			out = append(out, s.State)
		}
	}
	return out
}

func (sa *sendsAPI) lastState(id, state string) *api.StateRequest {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	for i := len(sa.states) - 1; i >= 0; i-- {
		if sa.states[i].ID == id && sa.states[i].State == state {
			r := sa.states[i].StateRequest
			return &r
		}
	}
	return nil
}

func (sa *sendsAPI) set(fs *fakeSend, f func(*fakeSend)) {
	sa.mu.Lock()
	f(fs)
	sa.mu.Unlock()
}

func (sa *sendsAPI) gcodeStates() []string {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	var out []string
	for _, e := range sa.events {
		if len(out) == 0 || out[len(out)-1] != e.GcodeState {
			out = append(out, e.GcodeState)
		}
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func httptestServer(t *testing.T, be *backend) *httptest.Server {
	srv := httptest.NewServer(be)
	t.Cleanup(srv.Close)
	return srv
}

// ---- the printer: MQTT broker and FTPS server on one fake address ----

const (
	cloudFun = "3EC1AFFF9CFF" // MQTT signature required: Cloud mode
	devFun   = "3EC18FFF9CFF" // Developer Mode
)

func printerReport(state, fun string, storage bool) []byte {
	return []byte(fmt.Sprintf(`{"print":{"command":"push_status","gcode_state":%q,"fun":%q,"sdcard":%t,"mc_percent":0,`+
		`"ams":{"ams":[{"id":"0","info":"0103","tray":[{"id":"0","tray_type":"PLA","tray_color":"000000FF"},{"id":"1"}]},`+
		`{"id":"1","info":"0003","tray":[{"id":"0","tray_type":"PLA","tray_color":"0000FFFF"}]}]},`+
		`"device":{"nozzle":{"info":[{"id":0,"diameter":0.4},{"id":1,"diameter":0.4}]}}}}`, state, fun, storage))
}

type sendsWorld struct {
	h      *harness
	be     *backend
	sa     *sendsAPI
	broker *testutil.Broker
	ftp    *testutil.FTPServer
	dir    string
	logs   *lockedBuf
	file   testutil.SlicedFile
	mods   []func(*Config)
	ca     *testutil.CA
	t      *testing.T
}

func sendsConfig(ftp *testutil.FTPServer, broker *testutil.Broker, autostart bool) func(*Config) {
	return func(c *Config) {
		c.AutoStart = autostart
		c.FTPSPort = 990
		c.Sends = sends.Config{PollBusy: 40 * time.Millisecond, PollIdle: 40 * time.Millisecond, Tick: 20 * time.Millisecond, EventEvery: time.Millisecond}
		c.PrinterDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			var d net.Dialer
			switch {
			case host != "192.168.1.40":
				return nil, fmt.Errorf("dialed %s", addr)
			case port == "8883":
				return d.DialContext(ctx, network, broker.Addr)
			case port == "990":
				return d.DialContext(ctx, network, ftp.Addr)
			default: // the FTPS data connection (PASV port)
				return d.DialContext(ctx, network, "127.0.0.1:"+port)
			}
		}
	}
}

// newSendsWorld pairs a link, connects the printer (Cloud mode unless fun
// says otherwise) and returns everything ready for a send.
func newSendsWorld(t *testing.T, fun string, storage, autostart bool) *sendsWorld {
	t.Helper()
	dir := t.TempDir()
	be := newBackend(t)
	sa := newSendsAPI(be)
	srv := httptestServer(t, be)
	ca := testutil.NewCA(t, "Test Printer CA")
	broker := testutil.NewBroker(t, ca.Leaf(t, serial), serial, accessCode, printerReport("FINISH", fun, storage))
	ftp := testutil.NewFTPServer(t, ca.Leaf(t, serial), accessCode)
	logs := &lockedBuf{}
	mods := []func(*Config){sendsConfig(ftp, broker, autostart)}
	h := newHarness(t, dir, be, srv, broker, ca, logs, mods...)
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(h.app.View().Printers) == 1 })
	h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}})
	waitFor(t, "printer report", func() bool {
		p := h.app.View().Printers[0]
		return p.Access == printers.AccessOK && p.Connected && p.GcodeState == "FINISH" && p.Storage != nil
	})
	f := testutil.Sliced(1, testutil.SlicedFilament{ID: 1, Type: "PLA", Color: "#000000", Map: "1"})
	w := &sendsWorld{h: h, be: be, sa: sa, broker: broker, ftp: ftp, dir: dir, logs: logs, file: f, mods: mods, ca: ca, t: t}
	t.Cleanup(func() { w.h.stop() })
	return w
}

func (w *sendsWorld) restart() {
	w.h.stop()
	w.h = newHarness(w.t, w.dir, w.be, w.h.srv, w.broker, w.ca, w.logs, w.mods...)
	waitFor(w.t, "codes reopened", func() bool { return w.h.app.View().CodesReady })
	w.h.sendSSDP()
	waitFor(w.t, "reconnected", func() bool {
		ps := w.h.app.View().Printers
		return len(ps) == 1 && ps[0].Connected && ps[0].GcodeState != ""
	})
}

func (w *sendsWorld) spoolFiles() []string {
	entries, _ := os.ReadDir(filepath.Join(w.dir, "spool"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (w *sendsWorld) waitState(id, state string) {
	w.t.Helper()
	waitFor(w.t, "state "+state, func() bool { return has(w.sa.statesOf(id), state) })
}

func (w *sendsWorld) screenStart(extra string) {
	w.broker.Push([]byte(`{"print":{"gcode_state":"RUNNING","subtask_name":"` + subtask + `","gcode_file":"/data/Metadata/plate_1.gcode","mc_percent":10,"layer_num":1,"total_layer_num":3` + extra + `}}`))
}

func (w *sendsWorld) projectFiles() int {
	n := 0
	for _, p := range w.broker.Publishes() {
		if strings.Contains(p.Payload, `"command":"project_file"`) {
			n++
		}
	}
	return n
}

// ---- the Cloud mode flow ----

// TestCloudSendUploadTapStartProgressFinish: the link downloads the file
// once, uploads it (TLS session reused), says uploaded, sees the print start
// from the printer screen, reports delivered, layers and HMS, then the
// finish, and deletes its file from the printer. It never sends a command
// other than pushall, and the access code never leaves the device except in
// the FTPS login to that printer.
func TestCloudSendUploadTapStartProgressFinish(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file)
	id := fs.send.ID
	w.waitState(id, "uploaded")
	if got := w.ftp.Files()[fileName]; !bytes.Equal(got, w.file.Data) {
		t.Fatalf("printer has %d bytes", len(got))
	}
	if r := w.ftp.Resumed(); len(r) != 1 || !r[0] {
		t.Fatalf("data connection resumed: %v", r)
	}
	if got := w.sa.lastState(id, "uploaded"); got.FileName != fileName {
		t.Fatalf("uploaded %+v", got)
	}
	if states := w.sa.statesOf(id); states[0] != "uploading" {
		t.Fatalf("states %v", states)
	}
	v := w.h.app.View()
	if st := v.Sends[serial]; st.FileName != fileName || !strings.Contains(st.Text, "tap it on the printer screen") {
		t.Fatalf("page status %+v", st)
	}
	if page := w.h.get("/"); !strings.Contains(page, fileName) || !strings.Contains(page, "No, upload only") {
		t.Fatalf("page lacks the send or the start mode:\n%s", page)
	}
	if len(w.spoolFiles()) != 1 {
		t.Fatalf("spool %v", w.spoolFiles())
	}
	// The worker taps the file on the printer screen.
	w.screenStart("")
	w.waitState(id, "delivered")
	if got := w.sa.lastState(id, "delivered"); got.SHA256 != w.file.SHA256 {
		t.Fatalf("delivered sha %q", got.SHA256)
	}
	w.broker.Push([]byte(`{"print":{"gcode_state":"RUNNING","mc_percent":60,"layer_num":2,"hms":[{"attr":50331904,"code":196615}]}}`))
	waitFor(t, "layer 2 event", func() bool {
		w.sa.mu.Lock()
		defer w.sa.mu.Unlock()
		for _, e := range w.sa.events {
			if e.Layer != nil && *e.Layer == 2 && len(e.HMS) == 1 && e.HMS[0] == "HMS_0300_0100_0003_0007" {
				return true
			}
		}
		return false
	})
	w.broker.Push([]byte(`{"print":{"gcode_state":"FINISH","mc_percent":100,"layer_num":3}}`))
	waitFor(t, "finish event and cleanup", func() bool {
		return has(w.sa.gcodeStates(), "FINISH") && len(w.ftp.Files()) == 0
	})
	if got := w.sa.gcodeStates(); strings.Join(got, ",") != "RUNNING,FINISH" {
		t.Fatalf("event states %v", got)
	}
	w.sa.mu.Lock()
	keys := map[string]bool{}
	for _, e := range w.sa.events {
		if keys[e.Key] || e.SendID != id {
			t.Errorf("event %+v repeated or for another send", e)
		}
		keys[e.Key] = true
	}
	w.sa.mu.Unlock()
	waitFor(t, "record closed", func() bool { _, ok := w.h.app.sends.Status(serial); return !ok })
	if len(w.spoolFiles()) != 0 {
		t.Fatalf("spool not emptied: %v", w.spoolFiles())
	}
	if w.projectFiles() != 0 {
		t.Fatal("a project_file went to a Cloud mode printer")
	}
	for _, p := range w.broker.Publishes() {
		if p.Payload != mqtt.PushAllPayload {
			t.Fatalf("printer got %q", p.Payload)
		}
	}
	// Access code: only in the printer's own logins; nowhere else.
	for _, l := range w.ftp.Logins() {
		if l.Username != "bblp" || l.Password != accessCode {
			t.Fatalf("ftp login %+v", l)
		}
	}
	for _, c := range w.ftp.Commands() {
		if strings.Contains(c, accessCode) {
			t.Fatalf("code in an FTP command %q", c)
		}
	}
	for name, data := range w.ftp.Files() {
		if strings.Contains(name, accessCode) || bytes.Contains(data, []byte(accessCode)) {
			t.Fatal("code in an uploaded file")
		}
	}
	w.h.stop()
	assertNoLeak(t, w.dir, w.be, w.logs, accessCode)
	for _, r := range w.be.requests() {
		if strings.Contains(r.Path, "tok-") || strings.Contains(r.Header.Get("Referer"), "tok-") {
			t.Fatal("file token in a URL")
		}
	}
}

func TestShaMismatchFailsAndLeavesNothing(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.data = append(append([]byte(nil), f.data...), 'x') })
	w.waitState(fs.send.ID, "failed")
	if r := w.sa.lastState(fs.send.ID, "failed"); r.Reason != "The file did not match its checksum." {
		t.Fatalf("reason %q", r.Reason)
	}
	hdr := w.sa.add(w.file, func(f *fakeSend) { f.badHeader = true })
	w.waitState(hdr.send.ID, "failed")
	if len(w.ftp.Files()) != 0 || len(w.spoolFiles()) != 0 {
		t.Fatal("a bad file reached the printer or stayed in the spool")
	}
	if !strings.Contains(w.h.get("/"), "The file did not match its checksum.") {
		t.Fatal("last delivery error not on the page")
	}
}

func TestWrongPrinterModelFails(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.Printer.ModelCode = "C12" })
	w.waitState(fs.send.ID, "failed")
	w.sa.mu.Lock()
	calls := w.sa.fileCalls
	w.sa.mu.Unlock()
	if calls != 0 || len(w.ftp.Files()) != 0 {
		t.Fatal("downloaded or uploaded for the wrong printer")
	}
}

func TestNotAZipFails(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	junk := []byte("not a zip at all")
	sum := sha256.Sum256(junk)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.data, f.send.SHA256 = junk, hex.EncodeToString(sum[:]) })
	w.waitState(fs.send.ID, "failed")
	if r := w.sa.lastState(fs.send.ID, "failed"); r.Reason != "The file is not a complete sliced plate." {
		t.Fatalf("reason %q", r.Reason)
	}
}

func TestNoUSBStickThenTryAgain(t *testing.T) {
	w := newSendsWorld(t, cloudFun, false, false)
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "upload_failed")
	if r := w.sa.lastState(fs.send.ID, "upload_failed"); r.Code != "no_storage" {
		t.Fatalf("code %q", r.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(w.sa.statesOf(fs.send.ID)); n != 1 {
		t.Fatalf("kept trying without Try again: %v", w.sa.statesOf(fs.send.ID))
	}
	// The worker puts a stick in and taps Try again.
	w.broker.Push(printerReport("FINISH", cloudFun, true))
	w.sa.set(fs, func(f *fakeSend) { f.linkState = "" })
	w.waitState(fs.send.ID, "uploaded")
}

func TestPrinterRefusesTheFile553(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	w.ftp.SetNoStorage(true) // the report says a stick, vsftpd says 553
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "upload_failed")
	if r := w.sa.lastState(fs.send.ID, "upload_failed"); r.Code != "storage_refused" {
		t.Fatalf("code %q", r.Code)
	}
	if len(w.spoolFiles()) != 1 {
		t.Fatal("the downloaded file should wait in the spool for Try again")
	}
}

func TestBusyPrinterWaits(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	w.broker.Push([]byte(`{"print":{"gcode_state":"RUNNING","subtask_name":"someone else"}}`))
	waitFor(t, "busy", func() bool { return w.h.app.View().Printers[0].GcodeState == "RUNNING" })
	fs := w.sa.add(w.file)
	time.Sleep(200 * time.Millisecond)
	w.sa.mu.Lock()
	calls := w.sa.fileCalls
	w.sa.mu.Unlock()
	if calls != 0 || len(w.sa.statesOf(fs.send.ID)) != 0 {
		t.Fatal("started while the printer was busy")
	}
	w.broker.Push([]byte(`{"print":{"gcode_state":"FINISH"}}`))
	w.waitState(fs.send.ID, "uploaded")
}

func TestExpiredTokenIsRetriedWithANewOne(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.expireOne = true })
	w.waitState(fs.send.ID, "uploaded")
	w.sa.mu.Lock()
	defer w.sa.mu.Unlock()
	if w.sa.fileCalls != 2 {
		t.Fatalf("file calls %d", w.sa.fileCalls)
	}
}

func TestWrongFileThenTheRightOne(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "uploaded")
	w.broker.Push([]byte(`{"print":{"gcode_state":"RUNNING","subtask_name":"benchy"}}`))
	w.waitState(fs.send.ID, "wrong_file")
	if r := w.sa.lastState(fs.send.ID, "wrong_file"); r.SubtaskName != "benchy" {
		t.Fatalf("%+v", r)
	}
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(strings.Join(w.sa.statesOf(fs.send.ID), ","), "wrong_file"); n != 1 {
		t.Fatalf("wrong_file reported %d times", n)
	}
	w.broker.Push([]byte(`{"print":{"gcode_state":"FINISH","subtask_name":"benchy"}}`))
	w.screenStart("")
	w.waitState(fs.send.ID, "delivered")
}

func TestWrongAMSSlotAfterTheStartAlerts(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "uploaded")
	// Filament 1 should print from AMS A slot 1 (0x0000); the printer uses AMS B slot 1.
	w.screenStart(`,"mapping":[256]`)
	w.waitState(fs.send.ID, "delivered")
	waitFor(t, "alert", func() bool {
		r := w.sa.lastState(fs.send.ID, "started")
		return r != nil && r.Alert != nil && r.Alert.Kind == "wrong_slot"
	})
	if r := w.sa.lastState(fs.send.ID, "started"); r.Alert.Detail != "Filament 1 prints from AMS B slot 1, not AMS A slot 1." {
		t.Fatalf("detail %q", r.Alert.Detail)
	}
	if w.projectFiles() != 0 {
		t.Fatal("the link must not try to stop or change the print")
	}
}

func TestRevokedMidSendDeletesTheSpool(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "uploaded")
	if len(w.spoolFiles()) != 1 {
		t.Fatal("no spool file")
	}
	w.be.setAll("revoked")
	waitFor(t, "unpaired", func() bool { return w.h.app.View().Phase == web.PhaseUnpaired })
	if len(w.spoolFiles()) != 0 {
		t.Fatalf("spool kept after revoke: %v", w.spoolFiles())
	}
	var saved []json.RawMessage
	b, _ := os.ReadFile(filepath.Join(w.dir, "state", "sends.json"))
	_ = json.Unmarshal(b, &saved)
	if len(saved) != 0 {
		t.Fatal("send records kept after revoke")
	}
}

func TestRestartAfterUploadWatchesWithoutUploadingAgain(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, false)
	fs := w.sa.add(w.file)
	w.waitState(fs.send.ID, "uploaded")
	w.restart()
	time.Sleep(150 * time.Millisecond)
	if n := strings.Count(strings.Join(w.sa.statesOf(fs.send.ID), ","), "uploaded"); n != 1 {
		t.Fatalf("uploaded again after restart: %v", w.sa.statesOf(fs.send.ID))
	}
	w.screenStart("")
	w.waitState(fs.send.ID, "delivered")
}

// ---- auto start: off by default, Developer Mode only, never twice ----

func TestAutoStartIsOffByDefault(t *testing.T) {
	w := newSendsWorld(t, devFun, true, false)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.AutoStart = true })
	w.waitState(fs.send.ID, "uploaded")
	time.Sleep(150 * time.Millisecond)
	if w.projectFiles() != 0 {
		t.Fatal("project_file sent with auto start off")
	}
}

func TestAutoStartNeverForACloudModePrinter(t *testing.T) {
	w := newSendsWorld(t, cloudFun, true, true)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.AutoStart = true })
	w.waitState(fs.send.ID, "uploaded")
	time.Sleep(150 * time.Millisecond)
	if w.projectFiles() != 0 {
		t.Fatal("project_file sent to a Cloud mode printer")
	}
	if got := w.h.app.printers.StartProjectFile(serial, mqtt.ProjectFile{}); got != printers.ErrAutoStart {
		t.Fatalf("manager allowed a start: %v", got)
	}
}

func TestDeveloperModeAutoStartRunsToTheFinish(t *testing.T) {
	w := newSendsWorld(t, devFun, true, true)
	w.broker.SetMode("developer", 30*time.Millisecond)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.AutoStart = true })
	w.waitState(fs.send.ID, "delivered")
	waitFor(t, "finish", func() bool { return has(w.sa.gcodeStates(), "FINISH") && len(w.ftp.Files()) == 0 })
	if w.projectFiles() != 1 {
		t.Fatalf("project_file sent %d times", w.projectFiles())
	}
	var pf struct {
		Print map[string]any `json:"print"`
	}
	for _, p := range w.broker.Publishes() {
		if strings.Contains(p.Payload, "project_file") {
			_ = json.Unmarshal([]byte(p.Payload), &pf)
		}
	}
	if pf.Print["url"] != "ftp:///"+fileName || pf.Print["md5"] != w.file.MD5 || pf.Print["param"] != "Metadata/plate_1.gcode" || pf.Print["subtask_name"] != subtask {
		t.Fatalf("project_file %+v", pf.Print)
	}
	if m, _ := json.Marshal(pf.Print["ams_mapping2"]); string(m) != `[{"ams_id":0,"slot_id":0}]` {
		t.Fatalf("ams_mapping2 %s", m)
	}
}

func TestRefusedAutoStartFallsBackToTheScreen(t *testing.T) {
	// The printer says Developer Mode but refuses the start (authorization):
	// the link waits for someone to tap the file, and never tries again.
	w := newSendsWorld(t, devFun, true, true)
	w.broker.SetMode("cloud", 0)
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.AutoStart = true })
	w.waitState(fs.send.ID, "uploaded")
	waitFor(t, "refused", func() bool { return strings.Contains(w.logs.String(), "refused the auto start") })
	w.screenStart("")
	w.waitState(fs.send.ID, "delivered")
	if w.projectFiles() != 1 {
		t.Fatalf("project_file sent %d times", w.projectFiles())
	}
}

func TestRestartAfterAutoStartNeverStartsTwice(t *testing.T) {
	w := newSendsWorld(t, devFun, true, true)
	w.broker.SetMode("developer", time.Hour) // accepted, but the print never shows up
	fs := w.sa.add(w.file, func(f *fakeSend) { f.send.AutoStart = true })
	w.waitState(fs.send.ID, "uploaded")
	waitFor(t, "start sent", func() bool { return w.projectFiles() == 1 })
	w.restart()
	time.Sleep(250 * time.Millisecond)
	if w.projectFiles() != 1 {
		t.Fatalf("project_file sent %d times across a restart", w.projectFiles())
	}
}

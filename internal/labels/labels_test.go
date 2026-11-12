package labels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/haapi"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
)

const (
	jobID     = "6f1c3c3e-1f7a-4c62-9d1e-2b8f0c8d9a10"
	printerID = "nelko.p21:aa:bb:cc:dd:ee:ff"
	token     = "test-token-for-labels-0123456789" // made up for this test
	secretB64 = "U0VDUkVUUEFHRURBVEE="             // the page bytes; must never be logged
)

// ---- fake Spoken Parts ----

type fakeJob struct {
	ref     api.LabelJobRef
	payload string
	state   string // queued, claimed, printing, printed, failed
	// sticky keeps the job listed and claimable after it was claimed (a
	// broken backend): the link must still never print it twice.
	sticky bool
}

type stateCall struct {
	ID string
	api.LabelStateRequest
}

type fakeAPI struct {
	mu        sync.Mutex
	jobs      []*fakeJob
	reports   []api.LabelReport
	lists     []time.Time
	claims    []string
	states    []stateCall
	claim409  bool
	stateFail int // the next n state calls answer 503
	// failFinal: the next n final (printed or failed) state calls answer 503.
	failFinal int
}

func (f *fakeAPI) ReportLabelPrinters(_ context.Context, r api.LabelReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(r) // as the wire sees it
	var c api.LabelReport
	_ = json.Unmarshal(b, &c)
	f.reports = append(f.reports, c)
	return nil
}

func (f *fakeAPI) ListLabelJobs(context.Context) ([]api.LabelJobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, time.Now())
	var out []api.LabelJobRef
	for _, j := range f.jobs {
		if j.state == "queued" || j.sticky {
			out = append(out, j.ref)
		}
	}
	return out, nil
}

func (f *fakeAPI) ClaimLabelJob(_ context.Context, id string) (*api.LabelJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, id)
	for _, j := range f.jobs {
		if j.ref.ID != id {
			continue
		}
		if f.claim409 || (j.state != "queued" && !j.sticky) {
			return nil, &api.Error{Status: 409, Code: api.CodeNotQueued}
		}
		j.state = "claimed"
		var out api.LabelJob
		if err := json.Unmarshal([]byte(j.payload), &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, &api.Error{Status: 404, Code: "not_found"}
}

func (f *fakeAPI) LabelJobState(_ context.Context, id string, req api.LabelStateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, stateCall{id, req})
	if f.stateFail > 0 {
		f.stateFail--
		return &api.Error{Status: 503}
	}
	if f.failFinal > 0 && req.State != "printing" {
		f.failFinal--
		return &api.Error{Status: 503}
	}
	for _, j := range f.jobs {
		if j.ref.ID == id {
			j.state = req.State
		}
	}
	return nil
}

func (f *fakeAPI) add(payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, &fakeJob{ref: api.LabelJobRef{ID: jobID, PrinterID: printerID}, payload: payload, state: "queued"})
}

func (f *fakeAPI) snapshot() (reports []api.LabelReport, lists int, claims []string, states []stateCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.LabelReport(nil), f.reports...), len(f.lists), append([]string(nil), f.claims...), append([]stateCall(nil), f.states...)
}

func (f *fakeAPI) stateNames() []string {
	_, _, _, st := f.snapshot()
	var out []string
	for _, s := range st {
		out = append(out, s.State)
	}
	return out
}

func payload(format string) string {
	return `{"id":"` + jobID + `","printer_id":"` + printerID + `","driver":"nelko.p21","document_type":"spool_label","format":"` + format + `",` +
		`"media":{"id":"14x40","kind":"die_cut","width_mm":14,"length_mm":40},"copies":2,` +
		`"pages":[{"width":96,"height":320,"data_b64":"` + secretB64 + `"}]}`
}

// ---- fake Home Assistant (through the real haapi client) ----

type fakeHA struct {
	mu       sync.Mutex
	srv      *httptest.Server
	printers string // the printers list of list_printers
	missing  bool   // the integration is not installed
	answer   string // print_raster's service_response
	hang     bool   // print_raster never answers
	lists    int
	prints   [][]byte
	auth     []string
}

func newFakeHA(t *testing.T) *fakeHA {
	h := &fakeHA{
		printers: `[{"id":"` + printerID + `","name":"Label maker","driver":"nelko.p21","transport":"spp","address":"AA:BB:CC:DD:EE:FF","state":"ready","last_error":"","battery":80,"media":"14x40","last_seen":"2026-11-11T20:00:00Z"}]`,
		answer:   `{"ok":true,"bytes":4242,"pages":1}`,
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		missing, hang := h.missing, h.hang
		var resp string
		switch r.URL.Path {
		case "/core/api/services/spoken_parts_printers/list_printers":
			h.lists++
			resp = `{"version":"0.1.0","printers":` + h.printers + `}`
		case "/core/api/services/spoken_parts_printers/print_raster":
			h.prints = append(h.prints, body)
			resp = h.answer
		default:
			t.Errorf("unexpected Home Assistant call %s", r.URL.Path)
		}
		h.mu.Unlock()
		if r.URL.RawQuery != "return_response" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		if missing {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("400: Bad Request"))
			return
		}
		if hang && strings.HasSuffix(r.URL.Path, "/print_raster") {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"changed_states":[],"service_response":` + resp + `}`))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHA) set(f func(*fakeHA)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func (h *fakeHA) counts() (lists, prints int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lists, len(h.prints)
}

// ---- harness ----

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type world struct {
	t    *testing.T
	api  *fakeAPI
	ha   *fakeHA
	r    *Runner
	logs *lockedBuf
	stop func()
}

func newWorld(t *testing.T, mods ...func(*Config)) *world {
	t.Helper()
	w := &world{t: t, api: &fakeAPI{}, ha: newFakeHA(t), logs: &lockedBuf{}}
	red := redact.New()
	red.Add(token)
	cfg := Config{
		API:          w.api,
		HA:           haapi.New(w.ha.srv.URL+"/core/api", token),
		Log:          logx.New(w.logs, red, logx.Debug),
		ReportEvery:  time.Hour,
		PollFast:     20 * time.Millisecond,
		PollSlow:     time.Hour,
		StateBackoff: 5 * time.Millisecond,
	}
	for _, m := range mods {
		m(&cfg)
	}
	w.r = New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.r.Run(ctx); close(done) }()
	w.stop = func() { cancel(); <-done }
	t.Cleanup(w.stop)
	return w
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (w *world) checkLogs() {
	w.t.Helper()
	logs := w.logs.String()
	for _, bad := range []string{secretB64, token, "data_b64"} {
		if strings.Contains(logs, bad) {
			w.t.Errorf("logs contain %q:\n%s", bad, logs)
		}
	}
}

// ---- tests ----

func TestReportAndFullJobFlow(t *testing.T) {
	w := newWorld(t)
	w.api.add(payload("raster"))
	w.r.SetEnabled(true)
	waitFor(t, "printed", func() bool { return strings.Join(w.api.stateNames(), ",") == "printing,printed" })
	waitFor(t, "report after the job", func() bool { r, _, _, _ := w.api.snapshot(); return len(r) >= 2 })

	reports, _, claims, states := w.api.snapshot()
	rep := reports[0]
	if !rep.Integration.Present || rep.Integration.Version != "0.1.0" || rep.Integration.Error != "" || len(rep.Printers) != 1 {
		t.Fatalf("report %+v", rep)
	}
	p := rep.Printers[0]
	if p.ID != printerID || p.Name != "Label maker" || p.Driver != "nelko.p21" || p.Transport != "spp" || p.State != "ready" ||
		p.Battery == nil || *p.Battery != 80 || p.Media != "14x40" || p.Address != "AA:BB:CC:DD:EE:FF" || p.LastSeen != "2026-11-11T20:00:00Z" {
		t.Fatalf("printer %+v", p)
	}
	if len(claims) != 1 || claims[0] != jobID {
		t.Fatalf("claims %v", claims)
	}
	if states[1].Bytes != 4242 || states[1].Code != "" {
		t.Fatalf("printed %+v", states[1])
	}

	// print_raster got exactly the claimed fields, the pages byte for byte.
	w.ha.mu.Lock()
	body := w.ha.prints[0]
	auth := w.ha.auth
	w.ha.mu.Unlock()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	if len(got) != 7 {
		t.Fatalf("print fields %v", keys)
	}
	var claimed map[string]json.RawMessage
	_ = json.Unmarshal([]byte(payload("raster")), &claimed)
	for _, k := range []string{"printer_id", "driver", "document_type", "media", "copies", "pages"} {
		if !bytes.Equal(got[k], claimed[k]) {
			t.Errorf("%s: %s, claimed %s", k, got[k], claimed[k])
		}
	}
	if string(got["job_id"]) != `"`+jobID+`"` {
		t.Errorf("job_id %s", got["job_id"])
	}
	for _, a := range auth {
		if a != "Bearer "+token {
			t.Fatalf("auth header %q", a)
		}
	}

	st := w.r.Status()
	if !st.Checked || !st.Present || len(st.Printers) != 1 || st.Printers[0].Name != "Label maker" || st.LastJob == nil ||
		st.LastJob.State != "printed" || st.LastJob.Printer != "Label maker" {
		t.Fatalf("status %+v", st)
	}
	w.checkLogs()
}

func TestNoIntegrationStillReports(t *testing.T) {
	w := newWorld(t)
	w.ha.set(func(h *fakeHA) { h.missing = true })
	w.r.SetEnabled(true)
	waitFor(t, "report", func() bool { r, _, _, _ := w.api.snapshot(); return len(r) >= 1 })
	reports, _, _, _ := w.api.snapshot()
	rep := reports[0]
	if rep.Integration.Present || rep.Integration.Error != "not_installed" || rep.Printers == nil || len(rep.Printers) != 0 {
		t.Fatalf("report %+v", rep)
	}
	st := w.r.Status()
	if !st.Checked || st.Present || st.Error != "not_installed" {
		t.Fatalf("status %+v", st)
	}
}

func TestNoTokenAndUnreachable(t *testing.T) {
	api1 := &fakeAPI{}
	r := New(Config{API: api1, HA: haapi.New("http://127.0.0.1:1", ""), Log: logx.New(io.Discard, redact.New(), logx.Debug)})
	r.mu.Lock()
	r.enabled = true
	r.mu.Unlock()
	r.report(context.Background(), 0)
	if rep := api1.reports[0]; rep.Integration.Present || rep.Integration.Error != "no_token" {
		t.Fatalf("no token: %+v", rep)
	}
	r = New(Config{API: api1, HA: haapi.New("http://127.0.0.1:1", token), Log: logx.New(io.Discard, redact.New(), logx.Debug)})
	r.mu.Lock()
	r.enabled = true
	r.mu.Unlock()
	r.report(context.Background(), 0)
	if rep := api1.reports[1]; rep.Integration.Present || rep.Integration.Error != "ha_unreachable" {
		t.Fatalf("unreachable: %+v", rep)
	}
}

func TestHAFailureIsReportedWithItsCode(t *testing.T) {
	w := newWorld(t)
	w.ha.set(func(h *fakeHA) {
		h.answer = `{"ok":false,"code":"offline","message":"Could not reach the printer over Bluetooth"}`
	})
	w.api.add(payload("raster"))
	w.r.SetEnabled(true)
	waitFor(t, "failed", func() bool { return strings.Join(w.api.stateNames(), ",") == "printing,failed" })
	_, _, _, st := w.api.snapshot()
	if st[1].Code != "offline" || st[1].Message != "Could not reach the printer over Bluetooth" {
		t.Fatalf("failed %+v", st[1])
	}
	if s := w.r.Status(); s.LastJob == nil || s.LastJob.State != "failed" || s.LastJob.Code != "offline" {
		t.Fatalf("status %+v", s.LastJob)
	}
}

func TestResultCodes(t *testing.T) {
	cases := []struct {
		raw     string
		err     error
		present bool
		state   string
		code    string
	}{
		{`{"ok":true,"bytes":10}`, nil, true, "printed", ""},
		{`{"ok":false,"code":"no_media","message":"m"}`, nil, true, "failed", "no_media"},
		{`{"ok":false,"code":"kaboom"}`, nil, true, "failed", "printer_error"},
		{`{"ok":false,"code":"Robert'); DROP"}`, nil, true, "failed", "printer_error"},
		{`{"bytes":10}`, nil, true, "failed", "printer_error"},
		{`"yes"`, nil, true, "failed", "printer_error"},
		{"", haapi.ErrTimeout, true, "failed", "timeout"},
		{"", haapi.ErrUnreachable, true, "failed", "timeout"},
		{"", haapi.ErrNotInstalled, false, "failed", "no_integration"},
		{"", haapi.ErrNoToken, false, "failed", "no_integration"},
		{"", haapi.ErrNotInstalled, true, "failed", "bad_format"},
		{"", &haapi.Error{Status: 500, Message: "boom"}, true, "failed", "printer_error"},
		{"", errors.New("odd"), true, "failed", "printer_error"},
	}
	for _, tc := range cases {
		got := result(json.RawMessage(tc.raw), tc.err, tc.present)
		if got.State != tc.state || got.Code != tc.code {
			t.Errorf("%s %v: %+v", tc.raw, tc.err, got)
		}
	}
}

func TestHATimeoutFailsAndIsNeverResent(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.PrintTimeout = 150 * time.Millisecond })
	w.ha.set(func(h *fakeHA) { h.hang = true })
	w.api.add(payload("raster"))
	w.api.mu.Lock()
	w.api.jobs[0].sticky = true // the backend keeps offering it
	w.api.mu.Unlock()
	w.r.SetEnabled(true)
	waitFor(t, "failed", func() bool { return strings.Join(w.api.stateNames(), ",") == "printing,failed" })
	_, _, _, st := w.api.snapshot()
	if st[1].Code != "timeout" {
		t.Fatalf("failed %+v", st[1])
	}
	_, lists0, _, _ := w.api.snapshot()
	waitFor(t, "more polls", func() bool { _, l, _, _ := w.api.snapshot(); return l > lists0+5 })
	_, _, claims, states := w.api.snapshot()
	if _, prints := w.ha.counts(); prints != 1 || len(claims) != 1 || len(states) != 2 {
		t.Fatalf("job handed to Home Assistant again: prints=%d claims=%d states=%d", prints, len(claims), len(states))
	}
}

func TestClaim409IsSkippedQuietly(t *testing.T) {
	quiet := &lockedBuf{}
	w := newWorld(t, func(c *Config) { c.Log = logx.New(quiet, redact.New(), logx.Info) })
	w.api.add(payload("raster"))
	w.api.mu.Lock()
	w.api.claim409 = true
	w.api.mu.Unlock()
	w.r.SetEnabled(true)
	waitFor(t, "claims", func() bool { _, _, c, _ := w.api.snapshot(); return len(c) >= 3 })
	_, _, _, states := w.api.snapshot()
	if _, prints := w.ha.counts(); prints != 0 || len(states) != 0 {
		t.Fatalf("409 claim went on: prints=%d states=%v", prints, states)
	}
	if w.r.Status().LastJob != nil {
		t.Fatal("a skipped job shows as the last job")
	}
	if strings.Contains(quiet.String(), "label job") {
		t.Fatalf("a 409 claim was logged above debug: %s", quiet.String())
	}
}

func TestBadFormatIsNotHandedToHA(t *testing.T) {
	w := newWorld(t)
	w.api.add(payload("pdf"))
	w.r.SetEnabled(true)
	waitFor(t, "failed", func() bool { return strings.Join(w.api.stateNames(), ",") == "failed" })
	_, _, _, st := w.api.snapshot()
	if st[0].Code != "bad_format" {
		t.Fatalf("%+v", st[0])
	}
	if _, prints := w.ha.counts(); prints != 0 {
		t.Fatal("a pdf job went to print_raster")
	}
}

func TestFinalStateReportIsRetried(t *testing.T) {
	w := newWorld(t)
	w.api.add(payload("raster"))
	w.api.mu.Lock()
	w.api.failFinal = 2 // "printed" fails twice, then works
	w.api.mu.Unlock()
	w.r.SetEnabled(true)
	waitFor(t, "printed", func() bool {
		n := w.api.stateNames()
		return len(n) > 0 && n[len(n)-1] == "printed" && len(n) == 4
	})
	if got := strings.Join(w.api.stateNames(), ","); got != "printing,printed,printed,printed" {
		t.Fatalf("states %s", got)
	}
	if _, prints := w.ha.counts(); prints != 1 {
		t.Fatalf("printed %d times", prints)
	}
}

func TestUnconfirmedPrintingIsNeverPrinted(t *testing.T) {
	w := newWorld(t)
	w.api.add(payload("raster"))
	w.api.mu.Lock()
	w.api.stateFail = 1 // Spoken Parts does not accept "printing"
	w.api.mu.Unlock()
	w.r.SetEnabled(true)
	waitFor(t, "failed", func() bool {
		n := w.api.stateNames()
		return len(n) > 0 && n[len(n)-1] == "failed"
	})
	if got := strings.Join(w.api.stateNames(), ","); got != "printing,failed" {
		t.Fatalf("states %s", got)
	}
	if _, prints := w.ha.counts(); prints != 0 {
		t.Fatalf("printed %d times after an unconfirmed printing report", prints)
	}
	w.api.mu.Lock()
	last := w.api.states[len(w.api.states)-1].LabelStateRequest
	w.api.mu.Unlock()
	if last.Code != "too_late" {
		t.Fatalf("code %q", last.Code)
	}
}

func TestPausedOrOffDoesNothing(t *testing.T) {
	w := newWorld(t)
	w.api.add(payload("raster"))
	time.Sleep(100 * time.Millisecond) // not enabled yet
	w.r.SetPaused(true)
	w.r.SetEnabled(true)
	time.Sleep(150 * time.Millisecond)
	reports, lists, claims, _ := w.api.snapshot()
	haLists, prints := w.ha.counts()
	if len(reports)+lists+len(claims)+haLists+prints != 0 {
		t.Fatalf("paused link acted: reports=%d lists=%d claims=%d ha=%d prints=%d", len(reports), lists, len(claims), haLists, prints)
	}
	w.r.SetPaused(false)
	waitFor(t, "printed after resume", func() bool { return strings.Join(w.api.stateNames(), ",") == "printing,printed" })

	w.r.Reset()
	if st := w.r.Status(); st.Checked || st.LastJob != nil {
		t.Fatalf("reset kept status %+v", st)
	}
	_, l0, _, _ := w.api.snapshot()
	time.Sleep(100 * time.Millisecond)
	if _, l1, _, _ := w.api.snapshot(); l1 != l0 {
		t.Fatal("polled after reset")
	}
}

func TestPollCadenceFollowsPrinters(t *testing.T) {
	w := newWorld(t, func(c *Config) {
		c.ReportEvery = 50 * time.Millisecond
		c.PollFast = 10 * time.Millisecond
		c.PollSlow = 400 * time.Millisecond
	})
	w.r.SetEnabled(true)
	waitFor(t, "a report", func() bool { r, _, _, _ := w.api.snapshot(); return len(r) >= 1 })
	count := func(d time.Duration) int {
		_, a, _, _ := w.api.snapshot()
		time.Sleep(d)
		_, b, _, _ := w.api.snapshot()
		return b - a
	}
	if n := count(300 * time.Millisecond); n < 8 {
		t.Fatalf("with a printer: %d polls in 300 ms, want fast polling", n)
	}
	w.ha.set(func(h *fakeHA) { h.printers = `[]` })
	waitFor(t, "empty report", func() bool { r, _, _, _ := w.api.snapshot(); return len(r[len(r)-1].Printers) == 0 })
	time.Sleep(50 * time.Millisecond) // let the last fast wait run out
	if n := count(300 * time.Millisecond); n > 2 {
		t.Fatalf("without printers: %d polls in 300 ms, want slow polling", n)
	}
	w.ha.set(func(h *fakeHA) { h.missing = true })
	waitFor(t, "missing report", func() bool { r, _, _, _ := w.api.snapshot(); return !r[len(r)-1].Integration.Present })
	time.Sleep(50 * time.Millisecond)
	if n := count(300 * time.Millisecond); n > 2 {
		t.Fatalf("without the integration: %d polls in 300 ms", n)
	}
}

func TestPrinterObjectsAreCleaned(t *testing.T) {
	w := newWorld(t)
	w.ha.set(func(h *fakeHA) {
		h.printers = `[{"id":"a","name":"` + strings.Repeat("n", 100) + `","state":"exploded","battery":900},` +
			`{"id":"a","name":"dupe","state":"ready"},{"id":"","name":"no id","state":"ready"},{"id":"has space","state":"ready"},` +
			`{"id":"b","name":"","state":"waiting","battery":null}]`
	})
	w.r.SetEnabled(true)
	waitFor(t, "report", func() bool { r, _, _, _ := w.api.snapshot(); return len(r) >= 1 })
	reports, _, _, _ := w.api.snapshot()
	ps := reports[0].Printers
	if len(ps) != 2 || ps[0].ID != "a" || len(ps[0].Name) != 60 || ps[0].State != "error" || ps[0].LastError == "" || ps[0].Battery != nil {
		t.Fatalf("printers %+v", ps)
	}
	if ps[1].Name != "b" || ps[1].State != "waiting" {
		t.Fatalf("printer b %+v", ps[1])
	}
}

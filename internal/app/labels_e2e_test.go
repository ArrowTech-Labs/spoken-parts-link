package app

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/labels"
)

// ---- a fake Spoken Parts label API (signed, behind the e2e backend) ----

type labelJobRow struct {
	id      string
	payload string
	state   string
}

type labelsAPI struct {
	mu      sync.Mutex
	reports []api.LabelReport
	jobs    []*labelJobRow
	states  map[string][]string
	lists   int
}

func newLabelsAPI(be *backend) *labelsAPI {
	la := &labelsAPI{states: map[string][]string{}}
	be.mu.Lock()
	be.extra = la.serve
	be.mu.Unlock()
	return la
}

func (la *labelsAPI) serve(w http.ResponseWriter, r *http.Request, body []byte, _ *fakeLink) bool {
	la.mu.Lock()
	defer la.mu.Unlock()
	const p = "/api/link/v1/"
	switch {
	case r.Method == "POST" && r.URL.Path == p+"label-printers":
		var rep api.LabelReport
		if err := json.Unmarshal(body, &rep); err != nil {
			w.WriteHeader(400)
			return true
		}
		la.reports = append(la.reports, rep)
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == "GET" && r.URL.Path == p+"label-jobs" && r.URL.RawQuery == "state=queued":
		la.lists++
		out := []map[string]string{}
		for _, j := range la.jobs {
			if j.state == "queued" {
				out = append(out, map[string]string{"id": j.id, "printer_id": labelPrinterID, "created_at": "2026-11-11T20:00:00Z", "expires_at": "2026-11-11T20:03:00Z"})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": out})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, p+"label-jobs/") && strings.HasSuffix(r.URL.Path, "/claim"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, p+"label-jobs/"), "/claim")
		for _, j := range la.jobs {
			if j.id == id && j.state == "queued" {
				j.state = "claimed"
				_, _ = w.Write([]byte(j.payload))
				return true
			}
		}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"code":"not_queued"}`))
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, p+"label-jobs/") && strings.HasSuffix(r.URL.Path, "/state"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, p+"label-jobs/"), "/state")
		var req api.LabelStateRequest
		_ = json.Unmarshal(body, &req)
		la.states[id] = append(la.states[id], req.State)
		for _, j := range la.jobs {
			if j.id == id {
				j.state = req.State
			}
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		return false
	}
	return true
}

const (
	labelPrinterID = "nelko.p21:aa:bb:cc:dd:ee:ff"
	labelPageB64   = "TEFCRUxQQUdFQllURVM="
	haTestToken    = "e2e-supervisor-token-0123456789abcdef" // made up for this test
)

func (la *labelsAPI) add() string {
	la.mu.Lock()
	defer la.mu.Unlock()
	id := newUUID()
	la.jobs = append(la.jobs, &labelJobRow{id: id, state: "queued", payload: `{"id":"` + id + `","printer_id":"` + labelPrinterID +
		`","driver":"nelko.p21","document_type":"spool_label","format":"raster","media":{"id":"14x40","kind":"die_cut","width_mm":14,"length_mm":40},` +
		`"copies":1,"pages":[{"width":96,"height":320,"data_b64":"` + labelPageB64 + `"}]}`})
	return id
}

func (la *labelsAPI) statesOf(id string) string {
	la.mu.Lock()
	defer la.mu.Unlock()
	return strings.Join(la.states[id], ",")
}

func (la *labelsAPI) counts() (reports, lists int) {
	la.mu.Lock()
	defer la.mu.Unlock()
	return len(la.reports), la.lists
}

// ---- a fake Supervisor proxy to Home Assistant ----

type fakeSupervisor struct {
	mu     sync.Mutex
	paths  []string
	prints []string
	bad    []string
}

func newFakeSupervisor(t *testing.T) (*fakeSupervisor, *httptest.Server) {
	fs := &fakeSupervisor{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.paths = append(fs.paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+haTestToken || r.URL.RawQuery != "return_response" {
			fs.bad = append(fs.bad, r.URL.String())
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/core/api/services/spoken_parts_printers/list_printers":
			_, _ = w.Write([]byte(`{"changed_states":[],"service_response":{"version":"0.1.0","printers":[{"id":"` + labelPrinterID +
				`","name":"Spool labels","driver":"nelko.p21","transport":"spp","address":"AA:BB:CC:DD:EE:FF","state":"ready","last_error":"","battery":76,"media":"14x40","last_seen":"2026-11-11T20:00:00Z"}]}}`))
		case "/core/api/services/spoken_parts_printers/print_raster":
			fs.prints = append(fs.prints, string(body))
			_, _ = w.Write([]byte(`{"changed_states":[],"service_response":{"ok":true,"bytes":3840,"pages":1}}`))
		default:
			fs.bad = append(fs.bad, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)
	return fs, srv
}

func (fs *fakeSupervisor) snapshot() (paths, prints, bad []string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.paths...), append([]string(nil), fs.prints...), append([]string(nil), fs.bad...)
}

func (fs *fakeSupervisor) printCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.prints)
}

// TestLabelPrintersEndToEnd: pair, approve, then the link reports the
// Home Assistant printers, prints a queued label through the Supervisor,
// honours a pause, and stops when the link is removed. The token and the
// page bytes never reach a log or Spoken Parts.
func TestLabelPrintersEndToEnd(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	la := newLabelsAPI(be)
	sup, supSrv := newFakeSupervisor(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) {
		c.HABase = supSrv.URL + "/core/api"
		c.HAToken = haTestToken
		c.Labels = labels.Config{ReportEvery: 200 * time.Millisecond, PollFast: 20 * time.Millisecond, PollSlow: time.Hour, StateBackoff: 5 * time.Millisecond}
	})
	defer h.stop()

	// Nothing before the link is active.
	time.Sleep(100 * time.Millisecond)
	if r, l := la.counts(); r+l != 0 {
		t.Fatalf("label calls before pairing: %d %d", r, l)
	}
	if paths, _, _ := sup.snapshot(); len(paths) != 0 {
		t.Fatalf("Home Assistant called before pairing: %v", paths)
	}
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "printers report", func() bool { r, _ := la.counts(); return r > 0 })
	la.mu.Lock()
	rep := la.reports[0]
	la.mu.Unlock()
	if !rep.Integration.Present || rep.Integration.Version != "0.1.0" || len(rep.Printers) != 1 || rep.Printers[0].Name != "Spool labels" || *rep.Printers[0].Battery != 76 {
		t.Fatalf("report %+v", rep)
	}

	id := la.add()
	waitFor(t, "printed", func() bool { return la.statesOf(id) == "printing,printed" })
	_, prints, _ := sup.snapshot()
	if len(prints) != 1 || !strings.Contains(prints[0], `"data_b64":"`+labelPageB64+`"`) || !strings.Contains(prints[0], `"job_id":"`+id+`"`) {
		t.Fatalf("print call %v", prints)
	}
	v := h.app.View()
	if !v.Labels.Present || len(v.Labels.Printers) != 1 || v.Labels.LastJob == nil || v.Labels.LastJob.State != "printed" {
		t.Fatalf("view %+v", v.Labels)
	}
	page := html.UnescapeString(h.get("/"))
	if !strings.Contains(page, "Through Home Assistant: 1 printer.") || !strings.Contains(page, "Spool labels: ready, battery 76 percent") || !strings.Contains(page, "Last label printed on Spool labels") {
		t.Fatalf("page lacks the label printers section")
	}

	// Paused in Spoken Parts: nothing is claimed or printed.
	be.mu.Lock()
	be.paused = true
	be.mu.Unlock()
	waitFor(t, "paused", func() bool { return h.app.View().Paused })
	time.Sleep(50 * time.Millisecond)
	id2 := la.add()
	_, l0 := la.counts()
	time.Sleep(200 * time.Millisecond)
	if _, l1 := la.counts(); l1 != l0 || la.statesOf(id2) != "" || sup.printCount() != 1 {
		t.Fatalf("paused link acted: lists %d -> %d, states %q, prints %d", l0, l1, la.statesOf(id2), sup.printCount())
	}
	be.mu.Lock()
	be.paused = false
	be.mu.Unlock()
	waitFor(t, "printed after resume", func() bool { return la.statesOf(id2) == "printing,printed" })

	// Removed in Spoken Parts: the runner stops and forgets.
	be.setAll("revoked")
	waitFor(t, "removed", func() bool { return h.app.View().Phase == "unpaired" })
	if st := h.app.View().Labels; st.Checked || st.LastJob != nil {
		t.Fatalf("labels status kept after removal: %+v", st)
	}
	_, l0 = la.counts()
	p0, _, _ := sup.snapshot()
	time.Sleep(200 * time.Millisecond)
	if _, l1 := la.counts(); l1 != l0 {
		t.Fatal("label calls after removal")
	}
	if p1, _, _ := sup.snapshot(); len(p1) != len(p0) {
		t.Fatal("label calls after removal")
	}

	if _, _, bad := sup.snapshot(); len(bad) != 0 {
		t.Fatalf("bad Home Assistant calls: %v", bad)
	}
	for _, s := range []string{haTestToken, labelPageB64} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains %q", s)
		}
	}
	for _, r := range be.requests() {
		if strings.Contains(string(r.Body), haTestToken) || strings.Contains(r.Query, haTestToken) {
			t.Fatalf("token sent to Spoken Parts in %s", r.Path)
		}
		for _, v := range r.Header {
			if strings.Contains(strings.Join(v, " "), haTestToken) {
				t.Fatalf("token in a header to Spoken Parts (%s)", r.Path)
			}
		}
	}
}

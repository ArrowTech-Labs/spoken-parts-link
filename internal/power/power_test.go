package power

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/haapi"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
)

var t0 = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

func feed(serial, state string, layer, percent int, extra map[string]any) monitor.PrinterFeed {
	r := map[string]json.RawMessage{}
	put := func(k string, v any) { b, _ := json.Marshal(v); r[k] = b }
	put("gcode_state", state)
	put("layer_num", layer)
	put("mc_percent", percent)
	put("subtask_name", "plate 1")
	for k, v := range extra {
		put(k, v)
	}
	return monitor.PrinterFeed{Serial: serial, Connected: true, LastReport: t0.Add(-time.Second), Report: r}
}

func TestStateMapping(t *testing.T) {
	cases := []struct {
		f    monitor.PrinterFeed
		want string
	}{
		{feed("A", "IDLE", 0, 0, nil), "idle"},
		{feed("A", "FINISH", 200, 100, nil), "finished"},
		{feed("A", "FAILED", 20, 10, nil), "failed"},
		{feed("A", "PREPARE", 0, 0, nil), "starting"},
		{feed("A", "SLICING", 0, 0, nil), "starting"},
		{feed("A", "RUNNING", 0, 0, map[string]any{"stg_cur": 2}), "starting"},
		{feed("A", "RUNNING", 5, 3, nil), "printing"},
		{feed("A", "PAUSE", 5, 3, nil), "paused"},
		{feed("A", "RUNNING", 5, 3, map[string]any{"upgrade_state": map[string]any{"status": "FLASHING"}}), "updating"},
		{feed("A", "IDLE", 0, 0, map[string]any{"upgrade_state": map[string]any{"status": "UPGRADE_SUCCESS"}}), "idle"},
		{feed("A", "WHAT", 0, 0, nil), "unknown"},
	}
	for i, c := range cases {
		if got := State(c.f, t0); got != c.want {
			t.Errorf("%d: %s, want %s", i, got, c.want)
		}
	}
	old := feed("A", "RUNNING", 5, 3, nil)
	old.LastReport = t0.Add(-31 * time.Second)
	if State(old, t0) != "unknown" {
		t.Error("a report older than 30 s is unknown")
	}
	off := feed("A", "RUNNING", 5, 3, nil)
	off.Connected = false
	if State(off, t0) != "unknown" {
		t.Error("no session is unknown")
	}
}

func TestBuildValue(t *testing.T) {
	total := int64(12400)
	jobs := map[string]api.PowerJob{"P1": {Serial: "P1", TotalMills: &total, Name: "SP-4840 plate 1"}}
	ps := Build([]monitor.PrinterFeed{
		feed("p1", "RUNNING", 50, 25, nil),
		feed("P2", "PREPARE", 0, 0, nil),
		feed("P3", "RUNNING", 50, 25, nil),
		feed("P4", "IDLE", 0, 0, nil),
	}, jobs, t0)
	if *ps[0].ValueMills != 3100 || ps[0].Basis != "percent" || ps[0].Job != "SP-4840 plate 1" || ps[0].Serial != "P1" {
		t.Fatalf("printing with a job: %+v", ps[0])
	}
	if *ps[1].ValueMills != 0 || ps[1].Basis != "start" {
		t.Fatalf("starting: %+v", ps[1])
	}
	if ps[2].ValueMills != nil || ps[2].Basis != "none" {
		t.Fatalf("no job: %+v", ps[2])
	}
	if ps[3].ValueMills != nil || ps[3].State != "idle" || ps[0].AgeMS != 1000 {
		t.Fatalf("idle: %+v", ps[3])
	}
}

type fakeEngine struct {
	mu      sync.Mutex
	calls   []string
	data    []map[string]any
	entries []map[string]any
	acked   int64
	err     error
	config  int
}

func (f *fakeEngine) Call(_ context.Context, typ string, data map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, typ)
	f.data = append(f.data, data)
	if f.err != nil {
		return nil, f.err
	}
	var out any
	switch typ {
	case "spoken_parts_power/entries":
		after := toInt(data["after"])
		var es []map[string]any
		for _, e := range f.entries {
			if toInt(e["seq"]) > after {
				es = append(es, e)
			}
		}
		out = map[string]any{"engine_id": "e-1", "entries": es, "last_seq": len(f.entries)}
	case "spoken_parts_power/ack":
		f.acked = toInt(data["through"])
		out = map[string]any{"left": 0}
	case "spoken_parts_power/status":
		out = map[string]any{
			"engine":   map[string]any{"id": "e-1", "version": "0.1.0", "config_version": f.config},
			"circuits": []any{map[string]any{"id": "c1", "state": "WATCH"}},
			"plugs":    []any{map[string]any{"ieee": "00:0d"}, map[string]any{"ieee": "00:0e"}},
			"errors":   []any{"Circuit B: the restore line must be under the shed line"},
		}
	case "spoken_parts_power/config":
		f.config = 7
		out = map[string]any{"accepted": true}
	case "spoken_parts_power/command":
		out = map[string]any{"result": "done"}
	default:
		out = map[string]any{"ok": true}
	}
	b, _ := json.Marshal(out)
	return b, nil
}

func (f *fakeEngine) Close() {}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

type fakeAPI struct {
	mu      sync.Mutex
	syncs   []api.PowerSyncRequest
	events  []api.PowerEventsRequest
	failEv  bool
	answer  api.PowerSyncResponse
	syncErr error
}

func (a *fakeAPI) PowerSync(_ context.Context, r api.PowerSyncRequest) (*api.PowerSyncResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.syncs = append(a.syncs, r)
	if a.syncErr != nil {
		return nil, a.syncErr
	}
	ans := a.answer
	return &ans, nil
}

func (a *fakeAPI) PowerEvents(_ context.Context, r api.PowerEventsRequest) (*api.PowerEventsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failEv {
		return nil, errors.New("offline")
	}
	a.events = append(a.events, r)
	last := r.Entries[len(r.Entries)-1].Seq
	return &api.PowerEventsResponse{StoredThrough: last}, nil
}

type src struct{ f []monitor.PrinterFeed }

func (s src) Feeds() []monitor.PrinterFeed { return s.f }

func newRunner(t *testing.T, eng *fakeEngine, a *fakeAPI, now *time.Time, path string) *Runner {
	r := New(Config{API: a, Engine: eng, Source: src{[]monitor.PrinterFeed{feed("P1", "RUNNING", 50, 25, nil)}},
		Now: func() time.Time { return *now }, QueuePath: path})
	r.SetEnabled(true)
	return r
}

func TestRunnerSyncConfigRequestsAndJobs(t *testing.T) {
	eng := &fakeEngine{}
	total := int64(10000)
	a := &fakeAPI{answer: api.PowerSyncResponse{
		ConfigVersion: 7, Config: json.RawMessage(`{"version":7,"circuits":[],"plugs":[]}`),
		Jobs:     []api.PowerJob{{Serial: "p1", TotalMills: &total}},
		Requests: []api.PowerRequest{{ID: "r1", Kind: "power_on", Plug: "00:0d", By: "Marc"}, {ID: "r2", Kind: "switch_on", Plug: "00:0d"}},
		NextS:    5,
	}}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.Step(context.Background())
	if len(a.syncs) != 1 || string(a.syncs[0].Engine) == "" || !strings.Contains(string(a.syncs[0].Plugs), "00:0e") {
		t.Fatalf("sync: %+v", a.syncs)
	}
	joined := strings.Join(eng.calls, ",")
	if !strings.Contains(joined, "spoken_parts_power/config") || strings.Count(joined, "spoken_parts_power/command") != 1 {
		t.Fatalf("calls: %s", joined)
	}
	st := r.Status()
	if st.Engine != "ok" || st.Circuits != 1 || st.Plugs != 2 || st.EngineVersion != "0.1.0" {
		t.Fatalf("status %+v", st)
	}
	// Next feed carries the job value; the config is not sent again.
	now = now.Add(6 * time.Second)
	r.Step(context.Background())
	var fed map[string]any
	for i, c := range eng.calls {
		if c == "spoken_parts_power/feed" {
			fed = eng.data[i]
		}
	}
	ps := fed["printers"].([]Printer)
	if ps[0].ValueMills == nil || *ps[0].ValueMills != 2500 {
		t.Fatalf("feed value %+v", ps[0])
	}
	if strings.Count(strings.Join(eng.calls, ","), "spoken_parts_power/config") != 1 {
		t.Fatal("config sent again although the engine has it")
	}
}

// Studio Q-73: a make_room request goes to the engine with its watts; one without
// them, or with a bad value, is dropped (an older link drops every make_room).
func TestRunnerForwardsMakeRoomWithItsWatts(t *testing.T) {
	eng := &fakeEngine{}
	need, booked, bad := 1320.0, 250.0, -5.0
	a := &fakeAPI{answer: api.PowerSyncResponse{
		ConfigVersion: 0,
		Requests: []api.PowerRequest{
			{ID: "m1", Kind: "make_room", Plug: "00:0d", By: "Kim", NeedW: &need, BookedW: &booked},
			{ID: "m2", Kind: "make_room", Plug: "00:0d"},
			{ID: "m3", Kind: "make_room", Plug: "00:0d", NeedW: &bad},
			{ID: "m4", Kind: "make_room", Plug: "00:0d", NeedW: &need},
			{ID: "m5", Kind: "make_room", Plug: "00:0d", NeedW: &need, BookedW: &bad},
		},
		NextS: 5,
	}}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.Step(context.Background())
	var got []map[string]any
	for i, c := range eng.calls {
		if c == "spoken_parts_power/command" {
			got = append(got, eng.data[i]["request"].(map[string]any))
		}
	}
	if len(got) != 2 {
		t.Fatalf("forwarded %d requests: %+v", len(got), got)
	}
	if got[0]["id"] != "m1" || got[0]["kind"] != "make_room" || got[0]["need_w"] != 1320.0 || got[0]["booked_w"] != 250.0 {
		t.Fatalf("first %+v", got[0])
	}
	if got[1]["id"] != "m4" || got[1]["booked_w"] != 0.0 {
		t.Fatalf("second %+v", got[1])
	}
	for _, g := range got {
		if g["plug"] != "00:0d" {
			t.Fatalf("plug %+v", g)
		}
	}
}

func TestRunnerQueuesAndUploadsEntries(t *testing.T) {
	eng := &fakeEngine{}
	for i := 1; i <= 3; i++ {
		eng.entries = append(eng.entries, map[string]any{"seq": i, "kind": "trip", "circuit": "c1", "at": "2026-11-15T10:00:00Z", "mono": 1.5, "data": map[string]any{"n": i}})
	}
	a := &fakeAPI{failEv: true}
	now := t0
	path := filepath.Join(t.TempDir(), "q.json")
	r := newRunner(t, eng, a, &now, path)
	r.Step(context.Background())
	if eng.acked != 3 || r.Status().Queued != 3 {
		t.Fatalf("acked %d queued %d", eng.acked, r.Status().Queued)
	}
	// The queue survives a restart of the link.
	r2 := newRunner(t, eng, a, &now, path)
	if r2.Status().Queued != 3 {
		t.Fatal("queue lost")
	}
	a.failEv = false
	now = now.Add(2 * time.Minute)
	r2.Step(context.Background())
	if len(a.events) != 1 || len(a.events[0].Entries) != 3 || a.events[0].EngineID != "e-1" || r2.Status().Queued != 0 {
		t.Fatalf("upload: %+v queued %d", a.events, r2.Status().Queued)
	}
	b, _ := json.Marshal(a.events[0].Entries[0])
	if strings.Contains(string(b), "mono") {
		t.Fatal("only the contract's fields go up")
	}
	// Nothing new: nothing uploaded again.
	now = now.Add(2 * time.Minute)
	r2.Step(context.Background())
	if len(a.events) != 1 {
		t.Fatal("uploaded twice")
	}
}

func TestPausedSendsNothingButFeedsTheEngine(t *testing.T) {
	eng := &fakeEngine{}
	a := &fakeAPI{}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.SetPaused(true)
	r.Step(context.Background())
	if len(a.syncs) != 0 || !strings.Contains(strings.Join(eng.calls, ","), "spoken_parts_power/feed") {
		t.Fatalf("paused: syncs %d calls %v", len(a.syncs), eng.calls)
	}
}

func TestEngineMissingIsReported(t *testing.T) {
	eng := &fakeEngine{err: haapi.ErrNotInstalled}
	a := &fakeAPI{}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.Step(context.Background())
	if len(a.syncs) != 1 || a.syncs[0].EngineError == nil || *a.syncs[0].EngineError != "not_installed" {
		t.Fatalf("sync %+v", a.syncs)
	}
	if r.Status().Engine != "not_installed" {
		t.Fatal(r.Status())
	}
}

func TestOldBackendBacksOff(t *testing.T) {
	eng := &fakeEngine{}
	a := &fakeAPI{syncErr: &api.Error{Status: 404}}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.Step(context.Background())
	now = now.Add(time.Minute)
	r.Step(context.Background())
	if len(a.syncs) != 1 {
		t.Fatalf("retried a 404 after a minute: %d", len(a.syncs))
	}
}

func TestSyncCarriesEngineConfigErrors(t *testing.T) {
	eng := &fakeEngine{}
	a := &fakeAPI{}
	now := t0
	r := newRunner(t, eng, a, &now, "")
	r.Step(context.Background())
	var e struct {
		ID     string   `json:"id"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(a.syncs[0].Engine, &e); err != nil {
		t.Fatal(err)
	}
	if e.ID != "e-1" || len(e.Errors) != 1 || !strings.Contains(e.Errors[0], "restore line") {
		t.Fatalf("engine %s", a.syncs[0].Engine)
	}
	// An engine that already sends engine.errors is passed on as it is.
	got := withErrors(json.RawMessage(`{"id":"e","errors":["x"]}`), []string{"y"})
	if string(got) != `{"id":"e","errors":["x"]}` {
		t.Fatalf("%s", got)
	}
}

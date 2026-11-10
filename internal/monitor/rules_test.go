package monitor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixtureLine is one recorded report: when it arrived and the MQTT payload.
type fixtureLine struct {
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

func loadFixture(t *testing.T, name string) []fixtureLine {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fixtureLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var l fixtureLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// got is an event or a clear, as a short string for comparisons.
func flatten(at time.Time, r Result) []string {
	var out []string
	ts := at.In(time.FixedZone("MST", -7*3600)).Format("15:04")
	if r.Reset {
		out = append(out, ts+" reset")
	}
	for _, e := range r.Events {
		out = append(out, fmt.Sprintf("%s %s %s", ts, e.Severity, e.Key))
	}
	for _, c := range r.Cleared {
		out = append(out, ts+" cleared "+c)
	}
	return out
}

var h2d = Printer{Serial: "0947CJ641600041", Name: "H2D", Camera: true}

// replay feeds a fixture through Merge, FromReport and Evaluate, one
// evaluation per report, and returns everything the rules said.
func replay(t *testing.T, lines []fixtureLine, p Printer, R Rules) ([]string, []Event) {
	t.Helper()
	var merged map[string]json.RawMessage
	var prev *Snapshot
	mem := &Memory{}
	var got []string
	var events []Event
	for _, l := range lines {
		var ok bool
		merged, ok = Merge(merged, l.Payload)
		if !ok {
			t.Fatalf("bad payload at %s", l.At)
		}
		cur := FromReport(merged, l.At)
		r := Evaluate(p, prev, cur, mem, R)
		got = append(got, flatten(l.At, r)...)
		events = append(events, r.Events...)
		prev = cur
	}
	return got, events
}

// The Nov 10 H2D incident (DESIGN section 1) must give exactly these alerts:
// the start, the dirty lens HMS at 01:58 (critical: AI camera module), 12
// objects skipped at 02:25 (critical: a burst), possible spaghetti at 02:31
// (critical), the next skips at 02:35 (critical again, held back by the
// cooldown in the runner), the finish, and the spaghetti message clearing.
func TestNov10H2DIncident(t *testing.T) {
	got, events := replay(t, loadFixture(t, "nov10_h2d.jsonl"), h2d, DefaultRules())
	want := []string{
		"01:12 reset",
		"01:12 info state:STARTED",
		"01:58 critical hms:0C00_0100_0002_0017",
		"02:25 critical skip",
		"02:31 critical hms:0C00_0300_0003_0008",
		"02:35 critical skip",
		"02:36 info state:FINISH",
		"02:37 cleared hms:0C00_0300_0003_0008",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("alerts:\n got  %q\n want %q", got, want)
	}
	byKey := map[string]Event{}
	for _, e := range events {
		byKey[e.Key] = e
	}
	lens := byKey["hms:0C00_0100_0002_0017"]
	if lens.Data["since"] != "2026-11-10T08:58:27Z" {
		t.Errorf("HMS time stamp (printer UTC+8) not converted: %v", lens.Data["since"])
	}
	if !strings.Contains(lens.Message, "Nozzle camera lens dirty on H2D, job Gridfinity - H2D SuperPack, layer 2/160") {
		t.Errorf("lens message: %q", lens.Message)
	}
	spag := byKey["hms:0C00_0300_0003_0008"]
	if !strings.HasPrefix(spag.Message, "Possible spaghetti on H2D, job Gridfinity - H2D SuperPack, layer 11/160") ||
		!strings.Contains(spag.Message, "Stop the print on the printer screen if needed.") {
		t.Errorf("spaghetti message: %q", spag.Message)
	}
	if spag.Data["wiki"] != "https://wiki.bambulab.com/en/h2/troubleshooting/hmscode/0C00_0300_0003_0008" {
		t.Errorf("wiki: %v", spag.Data["wiki"])
	}
	fin := byKey["state:FINISH"]
	if fin.Message != "Print finished on H2D, job Gridfinity - H2D SuperPack, layer 12/160. The plate needs clearing." {
		t.Errorf("finish message: %q", fin.Message)
	}
	for _, e := range events {
		if strings.ContainsAny(e.Message, "–—") {
			t.Errorf("dash in %q", e.Message)
		}
	}
}

// The prototype's rules (no 0C00 or burst escalation) would have said only
// warnings here: the port must be stricter.
func TestNov10SkipBurstIsCritical(t *testing.T) {
	_, events := replay(t, loadFixture(t, "nov10_h2d.jsonl"), h2d, DefaultRules())
	for _, e := range events {
		if e.Key == "skip" {
			if e.Severity != Critical || !strings.Contains(e.Message, "Parts may be coming loose") {
				t.Errorf("skip event %+v", e)
			}
		}
	}
}

func ip(n int) *int         { return &n }
func fp(f float64) *float64 { return &f }
func at(min int) time.Time {
	return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}
func running(min int) *Snapshot {
	return &Snapshot{At: at(min), GcodeState: "RUNNING", Stage: ip(0), Task: "Bins", Layer: ip(10), TotalLayers: ip(100), Percent: ip(10), RemainingMin: ip(300), HMS: map[string]HMS{}}
}

func TestRulesTable(t *testing.T) {
	R := DefaultRules()
	type step func(s *Snapshot)
	cases := []struct {
		name  string
		p     Printer
		prev  func() *Snapshot
		cur   func() *Snapshot
		mem   Memory
		want  []string
		clear []string
		reset bool
	}{
		{name: "failed is critical", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.GcodeState = "FAILED"; return s },
			want: []string{"critical state:FAILED"}},
		{name: "pause alone is a warning", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.GcodeState = "PAUSE"; return s },
			want: []string{"warning state:PAUSE"}},
		{name: "pause with print error is critical", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.GcodeState = "PAUSE"; s.PrintError = 0x0300800A; return s },
			want: []string{"critical state:PAUSE", "critical print_error"}},
		{name: "pause with HMS is critical", prev: func() *Snapshot { s := running(0); s.HMS["0300_0100_0001_0007"] = HMS{Level: 3}; return s },
			cur: func() *Snapshot {
				s := running(1)
				s.GcodeState = "PAUSE"
				s.HMS["0300_0100_0001_0007"] = HMS{Level: 3}
				return s
			},
			want: []string{"critical state:PAUSE"}},
		{name: "resume clears pause", prev: func() *Snapshot { s := running(0); s.GcodeState = "PAUSE"; return s },
			cur: func() *Snapshot { return running(1) }, want: []string{"info state:RESUMED"}, clear: []string{"state:PAUSE"}},
		{name: "finish is info", prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot { s := running(1); s.GcodeState = "FINISH"; return s }, want: []string{"info state:FINISH"}},
		{name: "start resets", prev: func() *Snapshot { s := running(0); s.GcodeState = "IDLE"; return s },
			cur: func() *Snapshot { s := running(1); s.GcodeState = "PREPARE"; return s }, want: []string{"info state:STARTED"}, reset: true},
		{name: "new HMS is a warning", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.HMS["0700_2000_0002_0001"] = HMS{Level: 2}; return s },
			want: []string{"warning hms:0700_2000_0002_0001"}},
		{name: "fatal HMS is critical", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.HMS["0300_0D00_0001_0003"] = HMS{Level: 1}; return s },
			want: []string{"critical hms:0300_0D00_0001_0003"}},
		{name: "any AI camera HMS is critical", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.HMS["0C00_0300_0002_0001"] = HMS{Level: 4}; return s },
			want: []string{"critical hms:0C00_0300_0002_0001"}},
		{name: "HMS gone clears", prev: func() *Snapshot { s := running(0); s.HMS["0700_2000_0002_0001"] = HMS{Level: 2}; return s },
			cur: func() *Snapshot { return running(1) }, clear: []string{"hms:0700_2000_0002_0001"}},
		{name: "same print error does not repeat", prev: func() *Snapshot { s := running(0); s.PrintError = 5; return s },
			cur: func() *Snapshot { s := running(1); s.PrintError = 5; return s }},
		{name: "print error gone clears", prev: func() *Snapshot { s := running(0); s.PrintError = 5; return s },
			cur: func() *Snapshot { return running(1) }, clear: []string{"print_error"}},
		{name: "xcam detection", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.XCamStatus = "2"; return s },
			want: []string{"critical xcam"}},
		{name: "xcam back to zero clears", prev: func() *Snapshot { s := running(0); s.XCamStatus = "2"; return s },
			cur: func() *Snapshot { return running(1) }, clear: []string{"xcam"}},
		{name: "AI settings changed", prev: func() *Snapshot { s := running(0); s.XCam.SpaghettiDetector = boolP(true); return s },
			cur:  func() *Snapshot { s := running(1); s.XCam.SpaghettiDetector = boolP(false); return s },
			want: []string{"info xcam_settings"}},
		{name: "one object skipped is a warning", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.Skipped = []int{7}; return s },
			want: []string{"warning skip"}},
		{name: "second skip within 10 minutes is critical", mem: Memory{Skips: []Skip{{At: at(-5), N: 1}}},
			prev: func() *Snapshot { s := running(0); s.Skipped = []int{7}; return s },
			cur:  func() *Snapshot { s := running(1); s.Skipped = []int{7, 9}; return s },
			want: []string{"critical skip"}},
		{name: "skip after the window is a warning", mem: Memory{Skips: []Skip{{At: at(-30), N: 1}}},
			prev: func() *Snapshot { s := running(0); s.Skipped = []int{7}; return s },
			cur:  func() *Snapshot { s := running(1); s.Skipped = []int{7, 9}; return s },
			want: []string{"warning skip"}},
		{name: "more than 30 percent of objects is critical", p: Printer{Name: "P1", Objects: 3},
			prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.Skipped = []int{4}; return s },
			want: []string{"critical skip"}},
		{name: "skips while idle are ignored", prev: func() *Snapshot { s := running(0); s.GcodeState = "IDLE"; return s },
			cur: func() *Snapshot { s := running(1); s.GcodeState = "IDLE"; s.Skipped = []int{1, 2}; return s }},
		{name: "progress jump", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.Percent = ip(40); s.Layer = ip(40); return s },
			want: []string{"warning progress_jump"}},
		{name: "progress backwards", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.Percent = ip(5); return s },
			want: []string{"warning progress_jump"}},
		{name: "eta jump", prev: func() *Snapshot { return running(0) },
			cur:  func() *Snapshot { s := running(1); s.RemainingMin = ip(200); return s },
			want: []string{"warning eta_jump"}},
		{name: "normal countdown is quiet", prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot { s := running(10); s.RemainingMin = ip(290); s.Layer = ip(11); return s }},
		{name: "no jump checks while heating", prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot { s := running(1); s.Stage = ip(2); s.Percent = ip(60); return s }},
		{name: "stall after 20 minutes", mem: Memory{StallLayer: ip(10), StallPercent: ip(10), StallSince: at(0)},
			prev: func() *Snapshot { return running(20) },
			cur:  func() *Snapshot { return running(21) },
			want: []string{"critical stall"}},
		{name: "stall said once", mem: Memory{StallLayer: ip(10), StallPercent: ip(10), StallSince: at(0), StallAlerted: true},
			prev: func() *Snapshot { return running(30) },
			cur:  func() *Snapshot { return running(31) }},
		{name: "progress clears stall", mem: Memory{StallLayer: ip(10), StallPercent: ip(10), StallSince: at(0), StallAlerted: true},
			prev:  func() *Snapshot { return running(30) },
			cur:   func() *Snapshot { s := running(31); s.Layer = ip(11); return s },
			clear: []string{"stall"}},
		{name: "temperature off 4 polls", mem: Memory{TempBad: 3},
			prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot {
				s := running(1)
				s.Extruders = []Extruder{{ID: 0, Temp: 190, Target: 220}}
				return s
			},
			want: []string{"critical temp_dev"}},
		{name: "temperature off 3 polls is quiet", mem: Memory{TempBad: 2},
			prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot {
				s := running(1)
				s.Bed, s.BedTarget = fp(50), fp(65)
				return s
			}},
		{name: "temperature back clears", mem: Memory{TempBad: 5, TempAlerted: true},
			prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot {
				s := running(1)
				s.Extruders = []Extruder{{ID: 0, Temp: 219, Target: 220}}
				return s
			},
			clear: []string{"temp_dev"}},
		{name: "low nozzle target ignored", mem: Memory{TempBad: 3},
			prev: func() *Snapshot { return running(0) },
			cur: func() *Snapshot {
				s := running(1)
				s.Extruders = []Extruder{{ID: 1, Temp: 38, Target: 90}}
				return s
			}},
		{name: "camera stream off", p: Printer{Name: "H2D", Camera: true},
			prev: func() *Snapshot { s := running(0); s.RTSP = "on"; return s },
			cur:  func() *Snapshot { s := running(1); s.RTSP = "off"; return s },
			want: []string{"warning camera_stream_off"}},
		{name: "camera stream off without camera is quiet", p: Printer{Name: "X2D"},
			prev: func() *Snapshot { s := running(0); s.RTSP = "on"; return s },
			cur:  func() *Snapshot { s := running(1); s.RTSP = "off"; return s }},
		{name: "first snapshot lists HMS as info", prev: func() *Snapshot { return nil },
			cur: func() *Snapshot {
				s := running(0)
				s.HMS["0C00_0100_0002_0017"] = HMS{Level: 2}
				s.PrintError = 7
				return s
			},
			want: []string{"info hms:0C00_0100_0002_0017", "warning print_error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.p
			if p.Name == "" {
				p = h2d
			}
			mem := c.mem
			r := Evaluate(p, c.prev(), c.cur(), &mem, R)
			var got []string
			for _, e := range r.Events {
				got = append(got, string(e.Severity)+" "+e.Key)
				if e.Message == "" || strings.ContainsAny(e.Message, "–—") {
					t.Errorf("message %q", e.Message)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("events %q, want %q", got, c.want)
			}
			if !slices.Equal(r.Cleared, c.clear) {
				t.Errorf("cleared %q, want %q", r.Cleared, c.clear)
			}
			if r.Reset != c.reset {
				t.Errorf("reset %v", r.Reset)
			}
		})
	}
}

func boolP(b bool) *bool { return &b }

func TestFeed(t *testing.T) {
	R := DefaultRules()
	mem := &Memory{}
	last := running(0)
	var got []string
	for i, fresh := range []bool{false, false, false, false, false, true, true} {
		r := Feed(h2d, last, fresh, mem, R)
		got = append(got, flatten(at(i), r)...)
	}
	want := []string{"09:04 critical feed_lost", "09:05 info feed_back", "09:05 cleared feed_lost"}
	if !slices.Equal(stripTime(got), stripTime(want)) {
		t.Fatalf("got %q want %q", got, want)
	}
	// Idle printer: only a warning.
	mem = &Memory{}
	idle := running(0)
	idle.GcodeState = "IDLE"
	var sev Severity
	for range 4 {
		if r := Feed(h2d, idle, false, mem, R); len(r.Events) > 0 {
			sev = r.Events[0].Severity
		}
	}
	if sev != Warning {
		t.Fatalf("idle feed lost severity %q", sev)
	}
}

func stripTime(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		_, rest, _ := strings.Cut(s, " ")
		out[i] = rest
	}
	return out
}

func TestCamera(t *testing.T) {
	mem := &Memory{}
	var got []string
	for _, ok := range []bool{false, false, false, false, true, true} {
		r := Camera(h2d, ok, "timeout", mem, 3)
		got = append(got, stripTime(flatten(at(0), r))...)
	}
	want := []string{"warning camera_fail", "info camera_back", "cleared camera_fail"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnapshotFromReport(t *testing.T) {
	payload := `{"print":{"gcode_state":"running","stg_cur":"0","mc_percent":"12","mc_remaining_time":90,"layer_num":3,
	"total_layer_num":50,"print_error":0,"hms":[{"attr":201326848,"code":131095,"ts_unix":"20261110165827"},{"attr":1,"code":65537,"ts":1762790000}],
	"s_obj":[5,"3"],"xcam_status":1,"xcam":{"spaghetti_detector":"enable","halt_print_sensitivity":"medium"},
	"device":{"extruder":{"info":[{"id":0,"temp":14418140},{"id":1,"temp":38}]}},"bed_temper":64.5,"bed_target_temper":65,
	"ipcam":{"rtsp_url":"disable","timelapse":"disable"},"task_id":"0","subtask_name":"a\u0001b"}}`
	m, ok := Merge(nil, []byte(payload))
	if !ok {
		t.Fatal("merge")
	}
	s := FromReport(m, at(0))
	if s.GcodeState != "RUNNING" || !s.Printing() || *s.Percent != 12 || *s.Layer != 3 || s.TaskID != "" || s.Task != "ab" {
		t.Errorf("basic fields %+v", s)
	}
	if h, ok := s.HMS["0C00_0100_0002_0017"]; !ok || h.Level != 2 || !h.Since.Equal(time.Date(2026, 11, 10, 8, 58, 27, 0, time.UTC)) {
		t.Errorf("HMS %+v", s.HMS)
	}
	if h, ok := s.HMS["0000_0001_0001_0001"]; !ok || h.Level != 1 || h.Since.Unix() != 1762790000 {
		t.Errorf("HMS unix %+v", s.HMS)
	}
	if !slices.Equal(s.Skipped, []int{3, 5}) || s.XCamStatus != "1" || s.XCam.SpaghettiDetector == nil || !*s.XCam.SpaghettiDetector {
		t.Errorf("skip/xcam %+v %+v", s.Skipped, s.XCam)
	}
	if len(s.Extruders) != 2 || s.Extruders[0].Temp != 220 || s.Extruders[0].Target != 220 || s.Extruders[1].Target != 0 {
		t.Errorf("extruders %+v", s.Extruders)
	}
	if s.RTSP != "off" || *s.BedTarget != 65 {
		t.Errorf("rtsp %q bed %v", s.RTSP, *s.BedTarget)
	}
	// Partial updates keep earlier fields.
	m, _ = Merge(m, []byte(`{"print":{"layer_num":4,"ipcam":{"rtsp_url":"rtsps://10.0.0.2/streaming/live/1"}}}`))
	s = FromReport(m, at(1))
	if *s.Layer != 4 || *s.Percent != 12 || s.RTSP != "on" {
		t.Errorf("merge %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "10.0.0.2") {
		t.Error("the stream URL must not be kept")
	}
	if _, ok := Merge(m, []byte(`[1,2]`)); ok {
		t.Error("non object accepted")
	}
}

func TestMergeBounds(t *testing.T) {
	var m map[string]json.RawMessage
	var b strings.Builder
	b.WriteString(`{"print":{`)
	for i := range 400 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":1`, i)
	}
	b.WriteString(`,"big":"` + strings.Repeat("x", 70<<10) + `"}}`)
	m, _ = Merge(m, []byte(b.String()))
	if len(m) > maxMergedKeys {
		t.Errorf("%d keys kept", len(m))
	}
	if _, ok := m["big"]; ok {
		t.Error("oversized field kept")
	}
}

func TestRulesClamp(t *testing.T) {
	r := Rules{StallMin: 1, ETAJumpMin: -5}.Clamp()
	if r.StallMin != 5 || r.ETAJumpMin != 10 || r.TempDevPolls != 4 || r.RepeatCooldownMin != 30 {
		t.Errorf("%+v", r)
	}
}

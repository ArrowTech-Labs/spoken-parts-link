package monitor

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Severity of an alert.
type Severity string

// Severities, lowest first.
const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Rank orders severities: info 0, warning 1, critical 2, unknown -1.
func (s Severity) Rank() int {
	switch s {
	case Info:
		return 0
	case Warning:
		return 1
	case Critical:
		return 2
	}
	return -1
}

// Rules are the thresholds. Spoken Parts sends them (per site); these are
// the defaults of farm_watch's printers.json.
type Rules struct {
	StallMin          int `json:"stall_min"`
	ETAJumpMin        int `json:"eta_jump_min"`
	ProgressJumpPct   int `json:"progress_jump_pct"`
	NozzleDevC        int `json:"nozzle_dev_c"`
	BedDevC           int `json:"bed_dev_c"`
	TempDevPolls      int `json:"temp_dev_polls"`
	FeedLostPolls     int `json:"feed_lost_polls"`
	RepeatCooldownMin int `json:"repeat_cooldown_min"`
	SkipBurstCount    int `json:"skip_burst_count"`
	SkipBurstMin      int `json:"skip_burst_min"`
	SkipFractionPct   int `json:"skip_fraction_pct"`
}

// DefaultRules are used until Spoken Parts sends others.
func DefaultRules() Rules {
	return Rules{
		StallMin: 20, ETAJumpMin: 45, ProgressJumpPct: 15, NozzleDevC: 12, BedDevC: 6,
		TempDevPolls: 4, FeedLostPolls: 4, RepeatCooldownMin: 30,
		SkipBurstCount: 2, SkipBurstMin: 10, SkipFractionPct: 30,
	}
}

// Clamp replaces missing or silly values with the defaults' bounds, so a
// bad answer cannot turn a rule into noise or switch it off by accident.
func (r Rules) Clamp() Rules {
	d := DefaultRules()
	fix := func(v *int, def, lo, hi int) {
		if *v == 0 {
			*v = def
		}
		*v = min(max(*v, lo), hi)
	}
	fix(&r.StallMin, d.StallMin, 5, 240)
	fix(&r.ETAJumpMin, d.ETAJumpMin, 10, 1440)
	fix(&r.ProgressJumpPct, d.ProgressJumpPct, 5, 100)
	fix(&r.NozzleDevC, d.NozzleDevC, 3, 100)
	fix(&r.BedDevC, d.BedDevC, 2, 60)
	fix(&r.TempDevPolls, d.TempDevPolls, 2, 60)
	fix(&r.FeedLostPolls, d.FeedLostPolls, 2, 120)
	fix(&r.RepeatCooldownMin, d.RepeatCooldownMin, 1, 1440)
	fix(&r.SkipBurstCount, d.SkipBurstCount, 1, 100)
	fix(&r.SkipBurstMin, d.SkipBurstMin, 1, 240)
	fix(&r.SkipFractionPct, d.SkipFractionPct, 1, 100)
	return r
}

// Printer is what the rules need to know about the printer itself.
type Printer struct {
	Serial string
	Name   string
	// Camera is true when frames are expected from this printer's own
	// camera (so a stream that turns off is worth saying).
	Camera bool
	// Objects is the number of objects on the plate, 0 if not known.
	Objects int
}

// Event is one alert the rules raised.
type Event struct {
	Severity Severity       `json:"severity"`
	Key      string         `json:"key"`
	Message  string         `json:"message"`
	Data     map[string]any `json:"data,omitempty"`
}

// Result of one evaluation.
type Result struct {
	Events []Event
	// Cleared are rule keys whose condition is over (Spoken Parts resolves
	// the open alert).
	Cleared []string
	// Reset is true when a new print started: every alert of the printer is
	// over and the rule memory starts fresh.
	Reset bool
}

func (r *Result) add(sev Severity, key, msg string, data map[string]any) {
	r.Events = append(r.Events, Event{Severity: sev, Key: key, Message: msg, Data: data})
}

func (r *Result) clear(key string) {
	if !slices.Contains(r.Cleared, key) {
		r.Cleared = append(r.Cleared, key)
	}
}

// Memory is what the rules remember about one printer between evaluations.
// It is saved with the last snapshot, so a restart does not alert again.
type Memory struct {
	StallLayer   *int      `json:"stall_layer,omitempty"`
	StallPercent *int      `json:"stall_percent,omitempty"`
	StallSince   time.Time `json:"stall_since,omitzero"`
	StallAlerted bool      `json:"stall_alerted,omitempty"`
	TempBad      int       `json:"temp_bad,omitempty"`
	TempAlerted  bool      `json:"temp_alerted,omitempty"`
	Skips        []Skip    `json:"skips,omitempty"`
	// FeedFails counts evaluations in a row with no fresh report.
	FeedFails int `json:"feed_fails,omitempty"`
	// CamFails counts camera grabs in a row that failed.
	CamFails int `json:"cam_fails,omitempty"`
}

// Skip is one change of the skipped objects list.
type Skip struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}

// Evaluate compares the previous and current snapshot of one printer and
// returns the alerts. prev is nil for the first snapshot ever seen.
func Evaluate(p Printer, prev, cur *Snapshot, mem *Memory, R Rules) Result {
	var res Result
	w := where(p, cur)
	if prev == nil {
		for _, code := range sortedHMS(cur.HMS) {
			h := cur.HMS[code]
			res.add(Info, "hms:"+code, fmt.Sprintf("Printer message %s was already showing when the link started watching %s.", code, name(p)), hmsData(code, h))
		}
		if cur.PrintError != 0 {
			res.add(Warning, "print_error", fmt.Sprintf("Printer error %s was already showing when the link started watching %s. Check the printer screen.", errHex(cur.PrintError), name(p)), map[string]any{"print_error": cur.PrintError})
		}
		return res
	}

	st, pst := cur.GcodeState, prev.GcodeState
	if st != pst && st != "" {
		switch {
		case st == "FAILED":
			res.add(Critical, "state:FAILED", "Print failed "+w+". Check the printer screen for the reason, then clear the plate.", map[string]any{"fail_reason": cur.FailReason, "print_error": cur.PrintError})
		case st == "PAUSE":
			sev := Warning
			if cur.PrintError != 0 || len(cur.HMS) > 0 {
				sev = Critical
			}
			res.add(sev, "state:PAUSE", "Print paused "+w+". Check the printer screen and the plate, then resume or stop it there.", map[string]any{"print_error": cur.PrintError, "hms": sortedHMS(cur.HMS)})
		case st == "FINISH":
			res.add(Info, "state:FINISH", "Print finished "+w+". The plate needs clearing.", nil)
		case st == "RUNNING" && pst == "PAUSE":
			res.add(Info, "state:RESUMED", "Print resumed "+w+".", nil)
			res.clear("state:PAUSE")
		case (st == "RUNNING" || st == "PREPARE") && (pst == "IDLE" || pst == "FINISH" || pst == "FAILED" || pst == ""):
			res.Reset = true
			*mem = Memory{FeedFails: mem.FeedFails, CamFails: mem.CamFails}
			res.add(Info, "state:STARTED", "Print started "+w+".", nil)
		}
		if !cur.Active() {
			if mem.StallAlerted {
				res.clear("stall")
			}
			if mem.TempAlerted {
				res.clear("temp_dev")
			}
			mem.StallLayer, mem.StallPercent, mem.StallSince, mem.StallAlerted = nil, nil, time.Time{}, false
			mem.TempBad, mem.TempAlerted = 0, false
		}
	}

	for _, code := range sortedHMS(cur.HMS) {
		h := cur.HMS[code]
		if _, ok := prev.HMS[code]; ok {
			continue
		}
		sev := Warning
		if h.Level == 1 || strings.HasPrefix(code, "0C00_") {
			// Module 0C00 is the AI camera (Nov 10: dirty lens, then
			// spaghetti below the pause threshold). A person must look.
			sev = Critical
		}
		res.add(sev, "hms:"+code, hmsMessage(code, w), hmsData(code, h))
	}
	if !res.Reset {
		for _, code := range sortedHMS(prev.HMS) {
			if _, ok := cur.HMS[code]; !ok {
				res.clear("hms:" + code)
			}
		}
	}

	if cur.PrintError != 0 && cur.PrintError != prev.PrintError {
		res.add(Critical, "print_error", fmt.Sprintf("Printer error %s %s. Check the printer screen.", errHex(cur.PrintError), w), map[string]any{"print_error": cur.PrintError})
	} else if cur.PrintError == 0 && prev.PrintError != 0 {
		res.clear("print_error")
	}

	if cur.XCamStatus != prev.XCamStatus {
		if cur.XCamStatus != "" {
			res.add(Critical, "xcam", fmt.Sprintf("The printer's AI camera saw a problem (code %s) %s. Check the plate. Stop the print on the printer screen if needed.", cur.XCamStatus, w), map[string]any{"xcam_status": cur.XCamStatus})
		} else {
			res.clear("xcam")
		}
	}

	if p.Camera && prev.RTSP != "" && cur.RTSP != prev.RTSP {
		switch cur.RTSP {
		case "off":
			res.add(Warning, "camera_stream_off", "The camera stream on "+name(p)+" is off. Turn LAN Only Liveview back on at the printer (Settings, LAN Only). Keep Cloud mode and leave Developer Mode off.", nil)
		case "on":
			res.add(Info, "camera_stream_on", "The camera stream on "+name(p)+" is on again.", nil)
			res.clear("camera_stream_off")
		}
	}

	if xcamKnown(prev.XCam) && xcamKnown(cur.XCam) && !sameJSON(prev.XCam, cur.XCam) {
		res.add(Info, "xcam_settings", "AI detection settings changed on "+name(p)+".", map[string]any{"before": prev.XCam, "after": cur.XCam})
	}

	if (st == "RUNNING" || st == "PAUSE") && !res.Reset && !slices.Equal(cur.Skipped, prev.Skipped) {
		added := newItems(prev.Skipped, cur.Skipped)
		if len(added) > 0 {
			mem.Skips = append(mem.Skips, Skip{At: cur.At, N: len(added)})
			window := time.Duration(R.SkipBurstMin) * time.Minute
			kept, recent := mem.Skips[:0], 0
			for _, s := range mem.Skips {
				if cur.At.Sub(s.At) <= window {
					kept = append(kept, s)
					recent += s.N
				}
			}
			mem.Skips = kept
			sev := Warning
			many := p.Objects > 0 && len(cur.Skipped)*100 > R.SkipFractionPct*p.Objects
			if recent >= R.SkipBurstCount || many {
				sev = Critical
			}
			total := fmt.Sprintf("%d skipped so far", len(cur.Skipped))
			if p.Objects > 0 {
				total = fmt.Sprintf("%d of %d skipped so far", len(cur.Skipped), p.Objects)
			}
			msg := fmt.Sprintf("%s skipped %s (%s). Check the plate for parts that came loose.", count(len(added), "object"), w, total)
			if sev == Critical {
				msg = fmt.Sprintf("%s skipped in %d minutes %s (%s). Parts may be coming loose. Check the plate. Stop the print on the printer screen if needed.", count(recent, "object"), R.SkipBurstMin, w, total)
			}
			res.add(sev, "skip", msg, map[string]any{"new": added, "total": len(cur.Skipped), "recent": recent, "objects": p.Objects})
		}
	}

	printing := cur.Printing()
	if printing && !res.Reset {
		if !sameInt(cur.Layer, mem.StallLayer) || !sameInt(cur.Percent, mem.StallPercent) || mem.StallSince.IsZero() {
			if mem.StallAlerted {
				res.clear("stall")
			}
			mem.StallLayer, mem.StallPercent, mem.StallSince, mem.StallAlerted = cloneInt(cur.Layer), cloneInt(cur.Percent), cur.At, false
		} else if d := cur.At.Sub(mem.StallSince); d > time.Duration(R.StallMin)*time.Minute && !mem.StallAlerted {
			mem.StallAlerted = true
			res.add(Critical, "stall", fmt.Sprintf("No progress for %d minutes %s. Check the printer.", int(d.Minutes()), w), map[string]any{"minutes": int(d.Minutes())})
		}
	} else if !cur.Active() || res.Reset {
		mem.StallLayer, mem.StallPercent, mem.StallSince = nil, nil, time.Time{}
	}

	if printing && pst == "RUNNING" {
		if cur.Percent != nil && prev.Percent != nil {
			a, b := *prev.Percent, *cur.Percent
			if b-a >= R.ProgressJumpPct || b < a {
				res.add(Warning, "progress_jump", fmt.Sprintf("Progress jumped from %d%% to %d%% %s. Objects may have been skipped or the print changed.", a, b, w), map[string]any{"from": a, "to": b})
			}
		}
		if cur.RemainingMin != nil && prev.RemainingMin != nil {
			expected := float64(*prev.RemainingMin) - cur.At.Sub(prev.At).Minutes()
			if diff := float64(*cur.RemainingMin) - expected; diff > float64(R.ETAJumpMin) || -diff > float64(R.ETAJumpMin) {
				res.add(Warning, "eta_jump", fmt.Sprintf("Time left jumped from %d to %d minutes %s.", *prev.RemainingMin, *cur.RemainingMin, w), map[string]any{"from": *prev.RemainingMin, "to": *cur.RemainingMin})
			}
		}
	}

	if printing {
		var bad []string
		for _, e := range cur.Extruders {
			if e.Target > 100 && abs(e.Temp-e.Target) > R.NozzleDevC {
				bad = append(bad, fmt.Sprintf("nozzle %d at %d C, should be %d C", e.ID+1, e.Temp, e.Target))
			}
		}
		if cur.BedTarget != nil && *cur.BedTarget > 0 {
			bed := 0.0
			if cur.Bed != nil {
				bed = *cur.Bed
			}
			if d := bed - *cur.BedTarget; d > float64(R.BedDevC) || -d > float64(R.BedDevC) {
				bad = append(bad, fmt.Sprintf("bed at %.0f C, should be %.0f C", bed, *cur.BedTarget))
			}
		}
		if len(bad) > 0 {
			mem.TempBad++
		} else {
			mem.TempBad = 0
			if mem.TempAlerted {
				mem.TempAlerted = false
				res.clear("temp_dev")
			}
		}
		if mem.TempBad >= R.TempDevPolls && !mem.TempAlerted {
			mem.TempAlerted = true
			res.add(Critical, "temp_dev", "Temperature off target "+w+": "+strings.Join(bad, ", ")+". Check the printer.", map[string]any{"readings": bad})
		}
	}
	return res
}

// Feed evaluates one tick of the report feed: fresh is false when the
// session is down or no report came for too long. last is the last snapshot
// (may be nil).
func Feed(p Printer, last *Snapshot, fresh bool, mem *Memory, R Rules) Result {
	var res Result
	if fresh {
		if mem.FeedFails >= R.FeedLostPolls {
			res.add(Info, "feed_back", "The link hears "+name(p)+" again.", nil)
			res.clear("feed_lost")
		}
		mem.FeedFails = 0
		return res
	}
	mem.FeedFails++
	if mem.FeedFails == R.FeedLostPolls {
		sev := Warning
		msg := "The link lost contact with " + name(p) + ". Check that it is on and on the network."
		if last != nil && last.Active() {
			sev = Critical
			msg = "The link lost contact with " + name(p) + " during a print" + job(last) + ". Check the printer and the plate."
		}
		res.add(sev, "feed_lost", msg, map[string]any{"polls": mem.FeedFails})
	}
	return res
}

// Camera evaluates one camera grab. failAfter is the number of failures in
// a row that raise an alert.
func Camera(p Printer, ok bool, reason string, mem *Memory, failAfter int) Result {
	var res Result
	if failAfter <= 0 {
		failAfter = 3
	}
	if ok {
		if mem.CamFails >= failAfter {
			res.add(Info, "camera_back", "Camera frames from "+name(p)+" are back.", nil)
			res.clear("camera_fail")
		}
		mem.CamFails = 0
		return res
	}
	mem.CamFails++
	if mem.CamFails == failAfter {
		res.add(Warning, "camera_fail", fmt.Sprintf("The link could not get a camera frame from %s %d times in a row (%s).", name(p), mem.CamFails, reason), map[string]any{"reason": reason})
	}
	return res
}

// ---- wording ----

func name(p Printer) string {
	if p.Name != "" {
		return p.Name
	}
	if len(p.Serial) > 4 {
		return "the printer ending " + p.Serial[len(p.Serial)-4:]
	}
	return "the printer"
}

func job(s *Snapshot) string {
	if s == nil || s.Task == "" {
		return ""
	}
	return ", job " + s.Task
}

// where says printer, job and layer: "on H2D, job Gridfinity, layer 12/160".
func where(p Printer, s *Snapshot) string {
	out := "on " + name(p) + job(s)
	if s != nil && s.Layer != nil && s.TotalLayers != nil && *s.TotalLayers > 0 {
		out += fmt.Sprintf(", layer %d/%d", *s.Layer, *s.TotalLayers)
	}
	return out
}

// hmsKnown are plain words for HMS codes seen on our printers: what it is,
// and what to do.
var hmsKnown = map[string][2]string{
	"0C00_0300_0003_0008": {"Possible spaghetti", "Check the plate. Stop the print on the printer screen if needed."},
	"0C00_0100_0002_0017": {"Nozzle camera lens dirty", "AI monitoring may miss problems. Clean the lens when the plate is free."},
}

func hmsMessage(code, w string) string {
	what, todo := "Printer message", "Check the printer screen and the plate. Stop the print on the printer screen if needed."
	if k, ok := hmsKnown[code]; ok {
		what, todo = k[0], k[1]
	} else if strings.HasPrefix(code, "0C00_") {
		what, todo = "The printer's AI camera reported a problem", "Check the plate. Stop the print on the printer screen if needed."
	}
	return what + " " + w + " (HMS " + code + "). " + todo
}

func hmsData(code string, h HMS) map[string]any {
	d := map[string]any{"code": code, "level": h.Level, "wiki": HMSWiki(code)}
	if !h.Since.IsZero() {
		d["since"] = h.Since.Format(time.RFC3339)
	}
	return d
}

func errHex(n int) string { return fmt.Sprintf("%08X", uint32(n)) }

func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// ---- helpers ----

func sortedHMS(m map[string]HMS) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func newItems(before, after []int) []int {
	seen := map[int]bool{}
	for _, v := range before {
		seen[v] = true
	}
	var out []int
	for _, v := range after {
		if !seen[v] {
			out = append(out, v)
		}
	}
	return out
}

func xcamKnown(x XCam) bool { return x != XCam{} }

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

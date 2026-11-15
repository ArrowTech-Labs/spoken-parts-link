package power

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
)

// Printer is one printer in a feed frame for the engine (DESIGN 4.2).
type Printer struct {
	Serial     string `json:"serial"`
	State      string `json:"state"`
	ValueMills *int64 `json:"value_mills"`
	Basis      string `json:"basis"`
	Job        string `json:"job,omitempty"`
	Layer      *int   `json:"layer,omitempty"`
	Percent    *int   `json:"percent,omitempty"`
	AgeMS      int64  `json:"age_ms"`
}

// unknownAfter: no printer report this long means the state is not known.
const unknownAfter = 30 * time.Second

// State maps a printer's own report to the engine's state (DESIGN 4.4).
// Nothing here is ever sent to a printer.
func State(f monitor.PrinterFeed, now time.Time) string {
	if !f.Connected || f.Report == nil || f.LastReport.IsZero() || now.Sub(f.LastReport) > unknownAfter {
		return "unknown"
	}
	if updating(f.Report) {
		return "updating"
	}
	s := monitor.FromReport(f.Report, f.LastReport)
	switch s.GcodeState {
	case "IDLE":
		return "idle"
	case "FINISH":
		return "finished"
	case "FAILED":
		return "failed"
	case "PREPARE", "SLICING":
		return "starting"
	case "RUNNING":
		if s.Layer == nil || *s.Layer < 1 {
			return "starting" // heating, calibrating: nothing laid down yet
		}
		return "printing"
	case "PAUSE":
		return "paused"
	}
	return "unknown"
}

// updating reads upgrade_state.status (a firmware update in progress).
func updating(r map[string]json.RawMessage) bool {
	raw, ok := r["upgrade_state"]
	if !ok {
		return false
	}
	var u struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return false
	}
	s := strings.ToUpper(u.Status)
	return strings.Contains(s, "DOWNLOAD") || strings.Contains(s, "FLASH") ||
		strings.Contains(s, "UPGRADING") || s == "UPDATING"
}

// Build makes the feed frame: state, and deposited value from the job values
// Spoken Parts sent (DESIGN 4.3, basis "percent"; "start" before the first
// layer; "none" for a print Spoken Parts has no value for).
func Build(feeds []monitor.PrinterFeed, jobs map[string]api.PowerJob, now time.Time) []Printer {
	out := make([]Printer, 0, len(feeds))
	for _, f := range feeds {
		serial := strings.ToUpper(strings.TrimSpace(f.Serial))
		if serial == "" {
			continue
		}
		p := Printer{Serial: serial, State: State(f, now), Basis: "none"}
		if !f.LastReport.IsZero() {
			p.AgeMS = max(0, now.Sub(f.LastReport).Milliseconds())
		}
		if f.Report != nil {
			s := monitor.FromReport(f.Report, f.LastReport)
			p.Layer, p.Percent = s.Layer, s.Percent
			p.Job = s.Task
		}
		job, hasJob := jobs[serial]
		if hasJob && job.Name != "" {
			p.Job = job.Name
		}
		switch p.State {
		case "starting":
			zero := int64(0)
			p.ValueMills, p.Basis = &zero, "start"
		case "printing", "paused":
			if hasJob && job.TotalMills != nil && p.Percent != nil {
				v := *job.TotalMills * int64(*p.Percent) / 100
				p.ValueMills, p.Basis = &v, "percent"
			}
		}
		if len(p.Job) > 80 {
			p.Job = p.Job[:80]
		}
		out = append(out, p)
	}
	return out
}

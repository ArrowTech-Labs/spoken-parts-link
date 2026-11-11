// Package monitor watches each printer's reports for problems and raises
// alerts. It is a port of Bambu Bot's farm_watch.py rules: no model and no
// guessing in the loop, only fixed rules with thresholds Spoken Parts sends.
// It never sends anything to a printer.
package monitor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HMS is one active health message.
type HMS struct {
	Level int `json:"level"` // 1 fatal, 2 serious, 3 common, 4 info
	// Since is when the printer raised it, in UTC, if it said.
	Since time.Time `json:"since,omitzero"`
}

// Extruder is one nozzle's temperature.
type Extruder struct {
	ID     int `json:"id"`
	Temp   int `json:"temp"`
	Target int `json:"target"`
}

// XCam is the printer's own AI detection settings.
type XCam struct {
	PrintingMonitor          *bool  `json:"printing_monitor,omitempty"`
	SpaghettiDetector        *bool  `json:"spaghetti_detector,omitempty"`
	FirstLayerInspector      *bool  `json:"first_layer_inspector,omitempty"`
	BuildplateMarkerDetector *bool  `json:"buildplate_marker_detector,omitempty"`
	PrintHalt                *bool  `json:"print_halt,omitempty"`
	HaltPrintSensitivity     string `json:"halt_print_sensitivity,omitempty"`
	AllowSkipParts           *bool  `json:"allow_skip_parts,omitempty"`
}

// Snapshot is what the rules look at: one printer's merged report reduced to
// the fields of the data contract (DESIGN section 3).
type Snapshot struct {
	At           time.Time      `json:"at"`
	GcodeState   string         `json:"gcode_state"`
	Stage        *int           `json:"stg_cur,omitempty"`
	Task         string         `json:"task,omitempty"`
	TaskID       string         `json:"task_id,omitempty"`
	File         string         `json:"file,omitempty"`
	Percent      *int           `json:"percent,omitempty"`
	RemainingMin *int           `json:"remaining_min,omitempty"`
	Layer        *int           `json:"layer,omitempty"`
	TotalLayers  *int           `json:"total_layers,omitempty"`
	PrintError   int            `json:"print_error,omitempty"`
	FailReason   string         `json:"fail_reason,omitempty"`
	HMS          map[string]HMS `json:"hms,omitempty"`
	Skipped      []int          `json:"skipped_objects,omitempty"`
	XCamStatus   string         `json:"xcam_status,omitempty"`
	XCam         XCam           `json:"xcam"`
	Extruders    []Extruder     `json:"extruders,omitempty"`
	Bed          *float64       `json:"bed,omitempty"`
	BedTarget    *float64       `json:"bed_target,omitempty"`
	// RTSP is the printer's ipcam.rtsp_url: "on" (a URL), "off" ("disable")
	// or "" (not reported). The URL itself is never kept.
	RTSP      string `json:"rtsp,omitempty"`
	Timelapse string `json:"timelapse,omitempty"`
}

// Printing is true while the printer is laying down plastic: RUNNING and
// stage 0 (other stages are heating, calibration or a filament change).
func (s *Snapshot) Printing() bool {
	return s.GcodeState == "RUNNING" && (s.Stage == nil || *s.Stage == 0 || *s.Stage == -1)
}

// Active is true for a print that is still on the plate.
func (s *Snapshot) Active() bool {
	switch s.GcodeState {
	case "RUNNING", "PREPARE", "PAUSE":
		return true
	}
	return false
}

// printerTZ is the zone of the printer's HMS time stamps (Bambu printers keep
// them in UTC+8, whatever their own clock setting says).
var printerTZ = time.FixedZone("UTC+8", 8*3600)

// HMSCode formats attr and code as Bambu's AAAA_BBBB_CCCC_DDDD and returns
// the level (upper 16 bits of code).
func HMSCode(attr, code uint32) (string, int) {
	return fmt.Sprintf("%04X_%04X_%04X_%04X", attr>>16, attr&0xFFFF, code>>16, code&0xFFFF), int(code >> 16)
}

// HMSWiki is the Bambu Lab wiki page for an HMS code.
func HMSWiki(code string) string {
	return "https://wiki.bambulab.com/en/h2/troubleshooting/hmscode/" + code
}

// hmsTime reads an HMS time stamp: 14 digits are printer local time
// (YYYYMMDDhhmmss in UTC+8), 9 to 10 digits are unix seconds.
func hmsTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return time.Time{}
		}
		s = n.String()
	}
	s = strings.TrimSpace(s)
	switch {
	case len(s) == 14:
		t, err := time.ParseInLocation("20060102150405", s, printerTZ)
		if err != nil {
			return time.Time{}
		}
		return t.UTC()
	case len(s) >= 9 && len(s) <= 10:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

// Merge applies one report payload to the merged print object, the way the
// printer means it: each report carries only the top level fields that
// changed. merged may be nil. It returns false for payloads that are not a
// JSON object with a print object.
func Merge(merged map[string]json.RawMessage, payload []byte) (map[string]json.RawMessage, bool) {
	var msg struct {
		Print map[string]json.RawMessage `json:"print"`
	}
	if json.Unmarshal(payload, &msg) != nil || msg.Print == nil {
		return merged, false
	}
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	for k, v := range msg.Print {
		if len(merged) >= maxMergedKeys {
			if _, ok := merged[k]; !ok {
				continue
			}
		}
		if len(v) > maxFieldBytes {
			continue
		}
		merged[k] = v
	}
	return merged, true
}

// Bounds for the merged report: a printer (or something answering for one)
// cannot make the link keep megabytes per printer.
const (
	maxMergedKeys = 300
	maxFieldBytes = 64 << 10
)

// FromReport reduces a merged print object to a Snapshot.
func FromReport(r map[string]json.RawMessage, at time.Time) *Snapshot {
	s := &Snapshot{At: at.UTC(), HMS: map[string]HMS{}}
	s.GcodeState = strings.ToUpper(text(r["gcode_state"], 20))
	s.Stage = intPtr(r["stg_cur"])
	s.Task = text(r["subtask_name"], 120)
	s.TaskID = text(r["task_id"], 24)
	if s.TaskID == "0" {
		s.TaskID = ""
	}
	s.File = text(r["gcode_file"], 200)
	if p := intPtr(r["mc_percent"]); p != nil {
		v := min(max(*p, 0), 100)
		s.Percent = &v
	}
	if p := intPtr(r["mc_remaining_time"]); p != nil && *p >= 0 {
		s.RemainingMin = p
	}
	if p := intPtr(r["layer_num"]); p != nil && *p >= 0 {
		s.Layer = p
	}
	if p := intPtr(r["total_layer_num"]); p != nil && *p >= 0 {
		s.TotalLayers = p
	}
	if p := intPtr(r["print_error"]); p != nil {
		s.PrintError = *p
	}
	s.FailReason = text(r["fail_reason"], 40)
	if s.FailReason == "0" {
		s.FailReason = ""
	}
	var hms []struct {
		Attr   int64           `json:"attr"`
		Code   int64           `json:"code"`
		TS     json.RawMessage `json:"ts"`
		TSUnix json.RawMessage `json:"ts_unix"`
	}
	if raw, ok := r["hms"]; ok && json.Unmarshal(raw, &hms) == nil {
		for _, h := range hms {
			if len(s.HMS) >= 30 {
				break
			}
			code, level := HMSCode(uint32(h.Attr), uint32(h.Code))
			e := HMS{Level: level}
			if len(h.TSUnix) > 0 {
				e.Since = hmsTime(h.TSUnix)
			} else if len(h.TS) > 0 {
				e.Since = hmsTime(h.TS)
			}
			s.HMS[code] = e
		}
	}
	var sobj []json.RawMessage
	if raw, ok := r["s_obj"]; ok && json.Unmarshal(raw, &sobj) == nil {
		for _, o := range sobj {
			if len(s.Skipped) >= 500 {
				break
			}
			if n, ok := flexInt(o); ok {
				s.Skipped = append(s.Skipped, n)
			}
		}
		sort.Ints(s.Skipped)
	}
	s.XCamStatus = text(r["xcam_status"], 20)
	if s.XCamStatus == "0" {
		s.XCamStatus = ""
	}
	var x map[string]json.RawMessage
	if raw, ok := r["xcam"]; ok && json.Unmarshal(raw, &x) == nil {
		s.XCam = XCam{
			PrintingMonitor:          boolPtr(x["printing_monitor"]),
			SpaghettiDetector:        boolPtr(x["spaghetti_detector"]),
			FirstLayerInspector:      boolPtr(x["first_layer_inspector"]),
			BuildplateMarkerDetector: boolPtr(x["buildplate_marker_detector"]),
			PrintHalt:                boolPtr(x["print_halt"]),
			HaltPrintSensitivity:     text(x["halt_print_sensitivity"], 20),
			AllowSkipParts:           boolPtr(x["allow_skip_parts"]),
		}
	}
	// H2D and X2D: device.extruder.info[].temp packs target<<16 | temp.
	var dev struct {
		Extruder struct {
			Info []struct {
				ID   int   `json:"id"`
				Temp int64 `json:"temp"`
			} `json:"info"`
		} `json:"extruder"`
	}
	if raw, ok := r["device"]; ok && json.Unmarshal(raw, &dev) == nil {
		for _, e := range dev.Extruder.Info {
			if len(s.Extruders) >= 4 {
				break
			}
			t := uint32(e.Temp)
			s.Extruders = append(s.Extruders, Extruder{ID: e.ID, Temp: int(t & 0xFFFF), Target: int(t >> 16)})
		}
	}
	if len(s.Extruders) == 0 {
		if t, ok := flexFloat(r["nozzle_temper"]); ok {
			tg, _ := flexFloat(r["nozzle_target_temper"])
			s.Extruders = []Extruder{{ID: 0, Temp: int(t + 0.5), Target: int(tg + 0.5)}}
		}
	}
	if v, ok := flexFloat(r["bed_temper"]); ok {
		s.Bed = &v
	}
	if v, ok := flexFloat(r["bed_target_temper"]); ok {
		s.BedTarget = &v
	}
	var cam struct {
		RTSP      *string `json:"rtsp_url"`
		Timelapse string  `json:"timelapse"`
	}
	if raw, ok := r["ipcam"]; ok && json.Unmarshal(raw, &cam) == nil {
		if cam.RTSP != nil {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(*cam.RTSP)), "rtsp") {
				s.RTSP = "on"
			} else {
				s.RTSP = "off"
			}
		}
		s.Timelapse = clean(cam.Timelapse, 20)
	}
	return s
}

func text(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = n.String()
	}
	return clean(s, limit)
}

// clean keeps printable characters only and caps the length.
func clean(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			break
		}
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func flexInt(raw json.RawMessage) (int, bool) {
	f, ok := flexFloat(raw)
	if !ok || f > 1<<31-1 || f < -(1<<31) {
		return 0, false
	}
	return int(f), true
}

func flexFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		n = json.Number(strings.TrimSpace(s))
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

func intPtr(raw json.RawMessage) *int {
	if n, ok := flexInt(raw); ok {
		return &n
	}
	return nil
}

func boolPtr(raw json.RawMessage) *bool {
	if len(raw) == 0 {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b
	}
	if n, ok := flexInt(raw); ok {
		b = n != 0
		return &b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "on", "enable", "1":
			b = true
			return &b
		case "false", "off", "disable", "0":
			return &b
		}
	}
	return nil
}

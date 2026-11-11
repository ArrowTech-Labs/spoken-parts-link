package monitor

import (
	"encoding/json"
	"regexp"
)

// Settings are the site's monitoring settings from the sync answer
// (CONTRACT 1.3). The link keeps the last ones, so the rules keep running
// while Spoken Parts cannot be reached.
type Settings struct {
	Enabled  bool                       `json:"enabled"`
	Rules    Rules                      `json:"rules"`
	Frames   FrameSettings              `json:"frames"`
	Printers map[string]PrinterSettings `json:"printers"`
}

// FrameSettings are the camera defaults.
type FrameSettings struct {
	IntervalS      int `json:"interval_s"`
	Keep           int `json:"keep"`
	FailAlertAfter int `json:"fail_alert_after"`
}

// PrinterSettings are the owner's choices for one printer.
type PrinterSettings struct {
	Camera         bool `json:"camera"`
	FrameIntervalS int  `json:"frame_interval_s"`
}

// Frame interval bounds (plan section 2: at least 30 s).
const (
	minFrameInterval = 30
	maxFrameInterval = 3600
	maxKeep          = 500
)

var serialRe = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)

// ParseSettings reads the monitor block of a sync answer. Nothing (an older
// backend) gives ok false; the caller keeps what it had.
func ParseSettings(raw json.RawMessage) (Settings, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return Settings{}, false
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, false
	}
	return s.clean(), true
}

func (s Settings) clean() Settings {
	s.Rules = s.Rules.Clamp()
	if s.Frames.IntervalS == 0 {
		s.Frames.IntervalS = 120
	}
	s.Frames.IntervalS = min(max(s.Frames.IntervalS, minFrameInterval), maxFrameInterval)
	if s.Frames.Keep <= 0 {
		s.Frames.Keep = 60
	}
	s.Frames.Keep = min(s.Frames.Keep, maxKeep)
	if s.Frames.FailAlertAfter <= 0 {
		s.Frames.FailAlertAfter = 3
	}
	s.Frames.FailAlertAfter = min(s.Frames.FailAlertAfter, 100)
	out := map[string]PrinterSettings{}
	for serial, p := range s.Printers {
		if !serialRe.MatchString(serial) || len(out) >= 200 {
			continue
		}
		if p.FrameIntervalS != 0 {
			p.FrameIntervalS = min(max(p.FrameIntervalS, minFrameInterval), maxFrameInterval)
		}
		out[serial] = p
	}
	s.Printers = out
	return s
}

// DefaultSettings: monitoring off until the owner turns it on in Spoken
// Parts.
func DefaultSettings() Settings { return Settings{}.clean() }

// camera reports whether frames are wanted from serial, and how often.
func (s Settings) camera(serial string) (bool, int) {
	p, ok := s.Printers[serial]
	if !s.Enabled || !ok || !p.Camera {
		return false, 0
	}
	if p.FrameIntervalS > 0 {
		return true, p.FrameIntervalS
	}
	return true, s.Frames.IntervalS
}

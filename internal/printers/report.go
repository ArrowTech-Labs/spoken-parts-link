package printers

import (
	"encoding/json"
	"strconv"
	"strings"
)

// devModeOffBit in print.fun means "MQTT signature required", which is the
// case when Developer mode is off (research notes, from ha-bambulab).
const devModeOffBit = 0x20000000

// gcodeStates are the values the contract allows. Anything else is sent as "".
var gcodeStates = map[string]bool{
	"IDLE": true, "RUNNING": true, "PAUSE": true, "FINISH": true,
	"FAILED": true, "PREPARE": true, "SLICING": true,
}

// update is what one report message changed. Nil fields were absent.
type update struct {
	gcodeState *string
	percent    *int
	remaining  *int
	devMode    *bool
	devKnown   bool
	firmware   string
}

// parseReport reads the fields the link cares about from one report
// payload. Bambu printers send partial updates, so only present fields are
// set. ok is false for payloads that are not JSON objects.
func parseReport(payload []byte) (u update, ok bool) {
	var msg struct {
		Print *struct {
			GcodeState *string          `json:"gcode_state"`
			McPercent  *json.RawMessage `json:"mc_percent"`
			McRemain   *json.RawMessage `json:"mc_remaining_time"`
			Fun        *json.RawMessage `json:"fun"`
		} `json:"print"`
		Info *struct {
			Module []struct {
				Name  string `json:"name"`
				SwVer string `json:"sw_ver"`
			} `json:"module"`
		} `json:"info"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return u, false
	}
	if p := msg.Print; p != nil {
		if p.GcodeState != nil {
			s := strings.ToUpper(strings.TrimSpace(*p.GcodeState))
			if !gcodeStates[s] {
				s = ""
			}
			u.gcodeState = &s
		}
		if n, ok := flexInt(p.McPercent); ok {
			n = min(max(n, 0), 100)
			u.percent = &n
		}
		if n, ok := flexInt(p.McRemain); ok && n >= 0 {
			u.remaining = &n
		}
		if p.Fun != nil {
			u.devKnown = true
			if v, ok := funBits(*p.Fun); ok {
				on := v&devModeOffBit == 0
				u.devMode = &on
			}
		}
	}
	if msg.Info != nil {
		for _, m := range msg.Info.Module {
			if m.Name == "ota" && firmwareOK(m.SwVer) {
				u.firmware = m.SwVer
			}
		}
	}
	return u, true
}

func flexInt(raw *json.RawMessage) (int, bool) {
	if raw == nil {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(*raw, &n); err != nil {
		var s string
		if json.Unmarshal(*raw, &s) != nil {
			return 0, false
		}
		n = json.Number(strings.TrimSpace(s))
	}
	i, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return 0, false
		}
		i = int64(f)
	}
	if i > 1<<31-1 || i < -(1<<31) {
		return 0, false
	}
	return int(i), true
}

// funBits reads print.fun, a hex string such as "3EC1AFFF9CFF". An empty
// value means unknown.
func funBits(raw json.RawMessage) (uint64, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, false
	}
	s = strings.TrimPrefix(strings.TrimSpace(strings.ToLower(s)), "0x")
	if s == "" || len(s) > 16 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func firmwareOK(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r == '.' || r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

package printers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tray is one AMS slot as the printer last reported it.
type Tray struct {
	AmsID  int    // 0..3 for AMS units, 128.. for AMS HT
	SlotID int    // 0..3
	Type   string // "" for an empty slot
	Color  string // "#RRGGBB" or ""
	// Nozzle is the nozzle this AMS feeds on a two nozzle printer: "left",
	// "right", or "" (one nozzle, or not known).
	Nozzle string
	feeds  int // extruder id from the AMS info bits, -1 unknown
	// SPOK-FAIL-1: the RFID tray id ("" when the roll has no Bambu tag), the
	// filament preset id, and remain in percent (nil when the printer doesn't know).
	UUID    string
	InfoIdx string
	Remain  *int
}

// AmsUnit is one AMS unit's humidity, temperature and drying state (SPOK-FAIL-1).
// Each field is nil until the printer reported it.
type AmsUnit struct {
	ID          int
	HumidityRaw *float64 // RH in percent (newer firmware)
	Humidity    *int     // the 1..5 index
	Temp        *float64 // C
	DryTime     *int     // minutes of drying left
	DrySfReason []string // why drying can't run (power, filament at the outlet)
	sfKnown     bool
	// ATS-SP-AMS-DRY-BUILD-1: read-only extras, sent as the printer reports them.
	// InfoRaw is the unit's "info" hex word (its drying status bits are decoded by
	// Spoken Parts once observed). DrySetting is the dry the printer was asked to
	// run (nil when the firmware doesn't send it). TrayExist is the printer's
	// tray_exist_bits hex word (the AMS feeder sensors, one word for all units).
	InfoRaw    string
	DrySetting *DrySetting
	TrayExist  string
	// hasData: the unit reported humidity, temperature or drying at least once.
	// A unit with only its info word is never synced (an all-empty humidity
	// sample would mean nothing to Spoken Parts).
	hasData bool
}

// DrySetting is the AMS dry the printer was asked to run. Each field is nil or
// "" when the firmware doesn't say.
type DrySetting struct {
	TempC    *int   // dry_temperature, C
	Hours    *int   // dry_duration, hours
	Filament string // dry_filament, a material name such as "PLA"
}

// Nozzle is one nozzle as the printer last reported it.
type Nozzle struct {
	ID       int
	Side     string // "left" or "right" on two nozzle printers, else ""
	Diameter string // "0.4"
	Type     string
}

// CommandResult is the printer's answer to a command we sent.
type CommandResult struct {
	Command string
	Result  string
	Reason  string
	ErrCode int
}

// Refused reports whether the printer said no (for example "mqtt message
// verify failed" from a printer that needs Bambu's authorization).
func (c CommandResult) Refused() bool {
	r := strings.ToLower(c.Result)
	return r == "fail" || r == "failed" || c.ErrCode != 0
}

// HMS severity, from the code's upper 16 bits (1 fatal, 2 serious).
const (
	hmsFatal   = 1
	hmsSerious = 2
)

// liveUpdate is the job, storage, AMS and nozzle part of one report.
type liveUpdate struct {
	layer      *int
	total      *int
	subtask    *string
	gcodeFile  *string
	printError *int
	hms        []string
	hmsKnown   bool
	hmsFatal   bool
	storage    *bool
	trays      []trayUpdate
	trayNow    *int
	mapping    []int
	mapKnown   bool
	nozzles    []Nozzle
	command    *CommandResult
	units      []AmsUnit
	trayExist  *string
}

type trayUpdate struct {
	amsID, slotID int
	feeds         int
	typ           *string
	color         *string
	uuid          *string
	infoIdx       *string
	remain        *int
}

func parseLive(payload []byte) (liveUpdate, bool) {
	var msg struct {
		Print *struct {
			LayerNum   *json.RawMessage `json:"layer_num"`
			TotalLayer *json.RawMessage `json:"total_layer_num"`
			Subtask    *string          `json:"subtask_name"`
			GcodeFile  *string          `json:"gcode_file"`
			PrintError *json.RawMessage `json:"print_error"`
			HMS        *[]struct {
				Attr int64 `json:"attr"`
				Code int64 `json:"code"`
			} `json:"hms"`
			SDCard   *bool            `json:"sdcard"`
			HomeFlag *json.RawMessage `json:"home_flag"`
			Mapping  *[]int           `json:"mapping"`
			Command  string           `json:"command"`
			Result   string           `json:"result"`
			Reason   string           `json:"reason"`
			ErrCode  *json.RawMessage `json:"err_code"`
			Nozzle   *json.RawMessage `json:"nozzle_diameter"`
			AMS      *struct {
				AMS []struct {
					ID   json.RawMessage              `json:"id"`
					Info string                       `json:"info"`
					Tray []map[string]json.RawMessage `json:"tray"`
					// SPOK-FAIL-1: humidity and drying, per unit.
					Humidity    *json.RawMessage   `json:"humidity"`
					HumidityRaw *json.RawMessage   `json:"humidity_raw"`
					Temp        *json.RawMessage   `json:"temp"`
					DryTime     *json.RawMessage   `json:"dry_time"`
					DrySfReason *[]json.RawMessage `json:"dry_sf_reason"`
					DrySetting  *struct {
						Temp     *json.RawMessage `json:"dry_temperature"`
						Duration *json.RawMessage `json:"dry_duration"`
						Filament *json.RawMessage `json:"dry_filament"`
					} `json:"dry_setting"`
				} `json:"ams"`
				TrayNow       *json.RawMessage `json:"tray_now"`
				TrayExistBits *json.RawMessage `json:"tray_exist_bits"`
			} `json:"ams"`
			Device *struct {
				Nozzle *struct {
					Info []struct {
						ID       int              `json:"id"`
						Diameter *json.RawMessage `json:"diameter"`
						Type     string           `json:"type"`
					} `json:"info"`
				} `json:"nozzle"`
				Storage *[]struct {
					Name string `json:"name"`
				} `json:"storage"`
			} `json:"device"`
		} `json:"print"`
	}
	var u liveUpdate
	if err := json.Unmarshal(payload, &msg); err != nil || msg.Print == nil {
		return u, err == nil
	}
	p := msg.Print
	if n, ok := flexInt(p.LayerNum); ok && n >= 0 {
		u.layer = &n
	}
	if n, ok := flexInt(p.TotalLayer); ok && n >= 0 {
		u.total = &n
	}
	if p.Subtask != nil {
		s := cleanText(*p.Subtask, 120)
		u.subtask = &s
	}
	if p.GcodeFile != nil {
		s := cleanText(*p.GcodeFile, 200)
		u.gcodeFile = &s
	}
	if n, ok := flexInt(p.PrintError); ok {
		u.printError = &n
	}
	if p.HMS != nil {
		u.hmsKnown = true
		for _, h := range *p.HMS {
			if len(u.hms) >= 20 {
				break
			}
			a, c := uint32(h.Attr), uint32(h.Code)
			u.hms = append(u.hms, fmt.Sprintf("HMS_%04X_%04X_%04X_%04X", a>>16, a&0xFFFF, c>>16, c&0xFFFF))
			if c>>16 == hmsFatal || c>>16 == hmsSerious {
				u.hmsFatal = true
			}
		}
	}
	// Storage: sdcard, else home_flag bits 8-9 (0 none, 1 normal, 2 abnormal,
	// 3 read only), else a "usb" entry in device.storage on newer firmware.
	switch {
	case p.SDCard != nil:
		v := *p.SDCard
		u.storage = &v
	case p.HomeFlag != nil:
		if n, ok := flexInt(p.HomeFlag); ok {
			v := (n>>8)&0x3 == 1
			u.storage = &v
		}
	}
	if p.Device != nil && p.Device.Storage != nil && u.storage == nil {
		v := false
		for _, s := range *p.Device.Storage {
			if strings.EqualFold(s.Name, "usb") || strings.EqualFold(s.Name, "sdcard") {
				v = true
			}
		}
		u.storage = &v
	}
	if p.Mapping != nil {
		u.mapKnown = true
		for i, v := range *p.Mapping {
			if i >= 64 {
				break
			}
			u.mapping = append(u.mapping, v)
		}
	}
	if p.Command != "" && p.Command != "push_status" && (p.Result != "" || p.ErrCode != nil) {
		c := CommandResult{Command: cleanText(p.Command, 40), Result: cleanText(p.Result, 40), Reason: cleanText(p.Reason, 120)}
		if n, ok := flexInt(p.ErrCode); ok {
			c.ErrCode = n
		}
		u.command = &c
	}
	if p.AMS != nil {
		if n, ok := flexInt(p.AMS.TrayNow); ok && n >= 0 && n <= 255 {
			u.trayNow = &n
		}
		if p.AMS.TrayExistBits != nil {
			var s string
			if json.Unmarshal(*p.AMS.TrayExistBits, &s) == nil {
				if h := hexWord(s); h != "" {
					u.trayExist = &h
				}
			}
		}
		for _, unit := range p.AMS.AMS {
			id, ok := flexInt((*json.RawMessage)(&unit.ID))
			if !ok || id < 0 || id > 255 {
				continue
			}
			feeds := -1
			if info, err := strconv.ParseUint(strings.TrimSpace(unit.Info), 16, 32); err == nil {
				feeds = int(info>>8) & 0xF
			}
			au, _ := parseUnit(id, unit.Humidity, unit.HumidityRaw, unit.Temp, unit.DryTime, unit.DrySfReason)
			au.InfoRaw = hexWord(unit.Info)
			if ds := unit.DrySetting; ds != nil {
				au.DrySetting = parseDrySetting(ds.Temp, ds.Duration, ds.Filament)
			}
			au.hasData = unitHasData(au)
			if au.hasData || au.InfoRaw != "" {
				u.units = append(u.units, au)
			}
			for _, tr := range unit.Tray {
				rawID, ok := tr["id"]
				slot, ok2 := flexInt(&rawID)
				if !ok || !ok2 || slot < 0 || slot > 3 {
					continue
				}
				t := trayUpdate{amsID: id, slotID: slot, feeds: feeds}
				if raw, ok := tr["tray_type"]; ok {
					var s string
					_ = json.Unmarshal(raw, &s)
					s = cleanText(s, 20)
					t.typ = &s
				} else if len(tr) == 1 {
					empty := "" // only an id: the slot is empty
					t.typ = &empty
				}
				if raw, ok := tr["tray_color"]; ok {
					var s string
					_ = json.Unmarshal(raw, &s)
					c := trayColor(s)
					t.color = &c
				}
				if raw, ok := tr["tray_uuid"]; ok {
					var s string
					_ = json.Unmarshal(raw, &s)
					s = trayID(s, 40)
					t.uuid = &s
				}
				if raw, ok := tr["tray_info_idx"]; ok {
					var s string
					_ = json.Unmarshal(raw, &s)
					s = trayID(s, 12)
					t.infoIdx = &s
				}
				if raw, ok := tr["remain"]; ok {
					if n, ok := flexInt(&raw); ok {
						if n < 0 || n > 100 {
							n = -1 // the printer doesn't know
						}
						t.remain = &n
					}
				}
				u.trays = append(u.trays, t)
			}
		}
	}
	if p.Device != nil && p.Device.Nozzle != nil {
		for _, n := range p.Device.Nozzle.Info {
			if n.ID < 0 || n.ID > 7 {
				continue
			}
			u.nozzles = append(u.nozzles, Nozzle{ID: n.ID, Diameter: diameter(n.Diameter), Type: cleanText(n.Type, 20)})
		}
	} else if d := diameter(p.Nozzle); d != "" {
		u.nozzles = []Nozzle{{ID: 0, Diameter: d}}
	}
	return u, true
}

// parseUnit reads one AMS unit's humidity fields. ok is false when it has none.
func parseUnit(id int, humidity, raw, temp, dry *json.RawMessage, reasons *[]json.RawMessage) (AmsUnit, bool) {
	u := AmsUnit{ID: id}
	if n, ok := flexInt(humidity); ok && n >= 1 && n <= 5 {
		u.Humidity = &n
	}
	if f, ok := flexFloat(raw); ok && f >= 0 && f <= 100 {
		u.HumidityRaw = &f
	}
	if f, ok := flexFloat(temp); ok && f > -20 && f < 150 {
		u.Temp = &f
	}
	if n, ok := flexInt(dry); ok && n >= 0 && n <= 100000 {
		u.DryTime = &n
	}
	if reasons != nil {
		u.sfKnown = true
		for i, r := range *reasons {
			if i >= 8 {
				break
			}
			var s string
			if json.Unmarshal(r, &s) != nil {
				s = string(r)
			}
			if s = cleanText(s, 40); s != "" {
				u.DrySfReason = append(u.DrySfReason, s)
			}
		}
	}
	ok := u.Humidity != nil || u.HumidityRaw != nil || u.Temp != nil || u.DryTime != nil || u.sfKnown
	return u, ok
}

// unitHasData is true when a unit report carries anything worth syncing.
func unitHasData(u AmsUnit) bool {
	return u.Humidity != nil || u.HumidityRaw != nil || u.Temp != nil || u.DryTime != nil ||
		u.sfKnown || u.DrySetting != nil
}

// parseDrySetting reads an AMS unit's dry_setting. nil when it has nothing usable.
func parseDrySetting(temp, dur, fil *json.RawMessage) *DrySetting {
	var d DrySetting
	if n, ok := flexInt(temp); ok && n > 0 && n <= 150 {
		d.TempC = &n
	}
	if n, ok := flexInt(dur); ok && n > 0 && n <= 96 {
		d.Hours = &n
	}
	if fil != nil {
		var s string
		if json.Unmarshal(*fil, &s) == nil {
			d.Filament = cleanText(s, 24)
		}
	}
	if d.TempC == nil && d.Hours == nil && d.Filament == "" {
		return nil
	}
	return &d
}

// hexWord keeps a hex word as sent ("0002", "1F"), upper case, at most 16 digits.
// Anything that isn't plain hex gives "".
func hexWord(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" || len(s) > 16 {
		return ""
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToUpper(s)
}

// flexFloat reads a number sent as a number or a string ("24", "25.3").
func flexFloat(raw *json.RawMessage) (float64, bool) {
	if raw == nil {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(*raw, &f); err == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(*raw, &s) != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// trayID keeps letters and digits only (an RFID tray uuid or a preset id).
func trayID(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= max {
			break
		}
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// trayColor turns "RRGGBBAA" into "#RRGGBB".
func trayColor(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 && len(s) != 8 {
		return ""
	}
	if _, err := strconv.ParseUint(s, 16, 32); err != nil {
		return ""
	}
	return "#" + strings.ToUpper(s[:6])
}

func diameter(raw *json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var f float64
	if err := json.Unmarshal(*raw, &f); err != nil {
		var s string
		if json.Unmarshal(*raw, &s) != nil {
			return ""
		}
		if f, err = strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
			return ""
		}
	}
	if f <= 0 || f > 5 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// cleanText keeps printable characters only and caps the length.
func cleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// live is what a printer last reported about its job, storage, AMS and nozzles.
type live struct {
	layer, total *int
	subtask      string
	gcodeFile    string
	printError   int
	hms          []string
	hmsFatal     bool
	storage      *bool
	trays        map[[2]int]Tray
	trayNow      *int
	mapping      []int
	nozzles      map[int]Nozzle
	command      *CommandResult
	commandSeq   int
	units        map[int]AmsUnit
	trayExist    string
}

func (l *live) apply(u liveUpdate) {
	if u.layer != nil {
		l.layer = u.layer
	}
	if u.total != nil {
		l.total = u.total
	}
	if u.subtask != nil {
		l.subtask = *u.subtask
	}
	if u.gcodeFile != nil {
		l.gcodeFile = *u.gcodeFile
	}
	if u.printError != nil {
		l.printError = *u.printError
	}
	if u.hmsKnown {
		l.hms, l.hmsFatal = u.hms, u.hmsFatal
	}
	if u.storage != nil {
		l.storage = u.storage
	}
	if u.trayNow != nil {
		l.trayNow = u.trayNow
	}
	if u.mapKnown {
		l.mapping = u.mapping
	}
	if l.trays == nil {
		l.trays = map[[2]int]Tray{}
	}
	for _, t := range u.trays {
		key := [2]int{t.amsID, t.slotID}
		cur, ok := l.trays[key]
		if !ok {
			cur = Tray{AmsID: t.amsID, SlotID: t.slotID}
		}
		cur.feeds = t.feeds
		if t.typ != nil {
			cur.Type = *t.typ
			if cur.Type == "" {
				cur.Color = ""
			}
		}
		if t.color != nil && cur.Type != "" {
			cur.Color = *t.color
		}
		if t.uuid != nil {
			cur.UUID = *t.uuid
		}
		if t.infoIdx != nil {
			cur.InfoIdx = *t.infoIdx
		}
		if t.remain != nil {
			if *t.remain < 0 {
				cur.Remain = nil
			} else {
				n := *t.remain
				cur.Remain = &n
			}
		}
		if t.typ != nil && *t.typ == "" {
			cur.UUID, cur.InfoIdx, cur.Remain = "", "", nil
		}
		l.trays[key] = cur
	}
	if l.units == nil {
		l.units = map[int]AmsUnit{}
	}
	if u.trayExist != nil {
		l.trayExist = *u.trayExist
	}
	for _, nu := range u.units {
		cur := l.units[nu.ID]
		cur.ID = nu.ID
		if nu.Humidity != nil {
			cur.Humidity = nu.Humidity
		}
		if nu.HumidityRaw != nil {
			cur.HumidityRaw = nu.HumidityRaw
		}
		if nu.Temp != nil {
			cur.Temp = nu.Temp
		}
		if nu.DryTime != nil {
			cur.DryTime = nu.DryTime
		}
		if nu.sfKnown {
			cur.DrySfReason = nu.DrySfReason
		}
		if nu.InfoRaw != "" {
			cur.InfoRaw = nu.InfoRaw
		}
		cur.hasData = cur.hasData || nu.hasData
		if nu.DrySetting != nil {
			ds := *nu.DrySetting
			cur.DrySetting = &ds
		}
		l.units[nu.ID] = cur
	}
	if len(u.nozzles) > 0 {
		l.nozzles = map[int]Nozzle{}
		for _, n := range u.nozzles {
			l.nozzles[n.ID] = n
		}
	}
	if u.command != nil {
		c := *u.command
		l.command = &c
		l.commandSeq++
	}
}

// sideOf names a nozzle on a two nozzle printer (id 0 is the right one).
func sideOf(id int, twoNozzles bool) string {
	if !twoNozzles {
		return ""
	}
	switch id {
	case 0:
		return "right"
	case 1:
		return "left"
	}
	return ""
}

// amsUnits is every unit's last humidity report, by unit id.
func (l *live) amsUnits() []AmsUnit {
	var out []AmsUnit
	for _, u := range l.units {
		if !u.hasData {
			continue
		}
		u.DrySfReason = append([]string(nil), u.DrySfReason...)
		if u.DrySetting != nil {
			ds := *u.DrySetting
			u.DrySetting = &ds
		}
		u.TrayExist = l.trayExist
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (l *live) snapshot() (trays []Tray, nozzles []Nozzle) {
	two := len(l.nozzles) == 2
	for _, n := range l.nozzles {
		n.Side = sideOf(n.ID, two)
		nozzles = append(nozzles, n)
	}
	sort.Slice(nozzles, func(i, j int) bool { return nozzles[i].ID < nozzles[j].ID })
	for _, t := range l.trays {
		t.Nozzle = ""
		if two && t.feeds >= 0 {
			t.Nozzle = sideOf(t.feeds, true)
		}
		trays = append(trays, t)
	}
	sort.Slice(trays, func(i, j int) bool {
		if trays[i].AmsID != trays[j].AmsID {
			return trays[i].AmsID < trays[j].AmsID
		}
		return trays[i].SlotID < trays[j].SlotID
	})
	return trays, nozzles
}

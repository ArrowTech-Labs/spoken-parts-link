package printers

import (
	"reflect"
	"testing"
)

// SPOK-FAIL-1: AMS humidity per unit and RFID/remain per tray, as an H2D with an
// AMS 2 Pro (id 0) and an AMS HT (id 128) reports them (strings, as Bambu sends).
const h2dHumidity = `{"print":{"ams":{"ams":[
{"id":"0","info":"0103","humidity":"4","humidity_raw":"21","temp":"26.4","dry_time":0,"dry_sf_reason":[],
 "tray":[{"id":"0","tray_type":"PETG","tray_color":"FFFFFFFF","tray_uuid":"A1B2C3D4E5F60718A1B2C3D4E5F60718","tray_info_idx":"GFG00","remain":82},
         {"id":"1","tray_type":"PLA","tray_color":"000000FF","tray_uuid":"00000000000000000000000000000000","remain":-1}]},
{"id":"128","info":"1003","humidity":"5","humidity_raw":"9","temp":"55.0","dry_time":420,"dry_sf_reason":["filament at outlet"],
 "tray":[{"id":"0","tray_type":"PAHT-CF","tray_color":"000000FF"}]}]}}}`

func TestParseLiveAmsHumidityAndRFID(t *testing.T) {
	u, ok := parseLive([]byte(h2dHumidity))
	if !ok {
		t.Fatal("not parsed")
	}
	var l live
	l.apply(u)
	units := l.amsUnits()
	if len(units) != 2 || units[0].ID != 0 || units[1].ID != 128 {
		t.Fatalf("units %+v", units)
	}
	a, ht := units[0], units[1]
	if *a.HumidityRaw != 21 || *a.Humidity != 4 || *a.Temp != 26.4 || *a.DryTime != 0 || len(a.DrySfReason) != 0 {
		t.Fatalf("ams 0 %+v", a)
	}
	if *ht.HumidityRaw != 9 || *ht.DryTime != 420 || !reflect.DeepEqual(ht.DrySfReason, []string{"filament at outlet"}) {
		t.Fatalf("ht %+v", ht)
	}
	trays, _ := l.snapshot()
	if trays[0].UUID != "A1B2C3D4E5F60718A1B2C3D4E5F60718" || trays[0].InfoIdx != "GFG00" || *trays[0].Remain != 82 {
		t.Fatalf("tray 0 %+v", trays[0])
	}
	if trays[1].Remain != nil {
		t.Fatalf("remain -1 is unknown: %+v", trays[1])
	}
	// A delta with only the HT's humidity keeps everything else; drying ended.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"128","humidity_raw":"11","dry_time":0,"dry_sf_reason":[]}]}}}`))
	l.apply(u)
	units = l.amsUnits()
	if *units[1].HumidityRaw != 11 || *units[1].Temp != 55 || *units[1].DryTime != 0 || len(units[1].DrySfReason) != 0 {
		t.Fatalf("delta %+v", units[1])
	}
	if *units[0].HumidityRaw != 21 {
		t.Fatalf("other unit changed %+v", units[0])
	}
	// The slot emptied: its RFID and remain go with the roll.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"0","info":"0103","tray":[{"id":"0"}]}]}}}`))
	l.apply(u)
	trays, _ = l.snapshot()
	if trays[0].Type != "" || trays[0].UUID != "" || trays[0].Remain != nil {
		t.Fatalf("emptied %+v", trays[0])
	}
	// Bad values are dropped, not passed on.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"1","humidity":"9","humidity_raw":"300","temp":"x"}]}}}`))
	if len(u.units) != 0 {
		t.Fatalf("bad values kept %+v", u.units)
	}
}

func TestTrayIDKeepsAlnumOnly(t *testing.T) {
	if got := trayID(" ab-12 CD\x00ef ", 40); got != "ab12CDef" {
		t.Fatalf("got %q", got)
	}
	if got := trayID("ABCDEFGH", 4); got != "ABCD" {
		t.Fatalf("cap %q", got)
	}
}

// ATS-SP-AMS-DRY-BUILD-1: the unit's raw info word, its dry setting and the
// printer's tray_exist_bits ride along, read only and as reported.
const x2dDrying = `{"print":{"ams":{"tray_exist_bits":"1000F","ams":[
{"id":"0","info":"0x2104","humidity_raw":"18","temp":"60.5","dry_time":610,
 "dry_setting":{"dry_temperature":65,"dry_duration":"12","dry_filament":"PETG"},"tray":[]},
{"id":"128","info":"1003","humidity_raw":"9","temp":"84.0","dry_time":300,
 "dry_setting":{"dry_temperature":"85","dry_duration":12,"dry_filament":"PAHT-CF"},"tray":[]},
{"id":"1","info":"0001","tray":[]}]}}}`

func TestParseLiveDryExtras(t *testing.T) {
	u, ok := parseLive([]byte(x2dDrying))
	if !ok {
		t.Fatal("not parsed")
	}
	var l live
	l.apply(u)
	units := l.amsUnits()
	// Unit 1 sent only its info word: never synced on its own.
	if len(units) != 2 || units[0].ID != 0 || units[1].ID != 128 {
		t.Fatalf("units %+v", units)
	}
	a, ht := units[0], units[1]
	if a.InfoRaw != "2104" || ht.InfoRaw != "1003" {
		t.Fatalf("info %q %q", a.InfoRaw, ht.InfoRaw)
	}
	if a.TrayExist != "1000F" || ht.TrayExist != "1000F" {
		t.Fatalf("tray_exist %q %q", a.TrayExist, ht.TrayExist)
	}
	if a.DrySetting == nil || *a.DrySetting.TempC != 65 || *a.DrySetting.Hours != 12 || a.DrySetting.Filament != "PETG" {
		t.Fatalf("ams 0 setting %+v", a.DrySetting)
	}
	if ht.DrySetting == nil || *ht.DrySetting.TempC != 85 || *ht.DrySetting.Hours != 12 || ht.DrySetting.Filament != "PAHT-CF" {
		t.Fatalf("ht setting %+v", ht.DrySetting)
	}
	// A delta with only tray_exist_bits keeps the units and updates the word.
	u, _ = parseLive([]byte(`{"print":{"ams":{"tray_exist_bits":"F"}}}`))
	l.apply(u)
	units = l.amsUnits()
	if len(units) != 2 || units[0].TrayExist != "F" || units[1].DrySetting == nil {
		t.Fatalf("delta %+v", units)
	}
	// A unit that later reports humidity is synced, with the info word it sent before.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"1","humidity_raw":"30"}]}}}`))
	l.apply(u)
	units = l.amsUnits()
	if len(units) != 3 || units[1].ID != 1 || units[1].InfoRaw != "0001" {
		t.Fatalf("unit 1 later %+v", units)
	}
	// Garbage is dropped: non-hex words, absurd settings.
	u, _ = parseLive([]byte(`{"print":{"ams":{"tray_exist_bits":"zz","ams":[{"id":"2","info":"12G4","humidity_raw":"5",
		"dry_setting":{"dry_temperature":900,"dry_duration":-1,"dry_filament":""}}]}}}`))
	if u.trayExist != nil || len(u.units) != 1 || u.units[0].InfoRaw != "" || u.units[0].DrySetting != nil {
		t.Fatalf("garbage kept %+v %+v", u.trayExist, u.units)
	}
	// The copy handed out is not shared with the live state.
	units[0].DrySetting.Filament = "changed"
	if l.amsUnits()[0].DrySetting.Filament != "PETG" {
		t.Fatal("dry setting shared")
	}
}

func TestHexWord(t *testing.T) {
	for in, want := range map[string]string{"0x1f": "1F", " 0103 ": "0103", "": "", "12345678901234567": "", "G1": ""} {
		if got := hexWord(in); got != want {
			t.Errorf("hexWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// Review fixes: a dry that ends clears the setting; a partial setting merges;
// a wrongly typed new field never drops the rest of the report; an all-zero
// tray_uuid (no RFID tag) is not passed on as an id.
func TestDrySettingEndsMergesAndLenient(t *testing.T) {
	var l live
	u, _ := parseLive([]byte(x2dDrying))
	l.apply(u)
	// Partial: only the hours change.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"0","dry_setting":{"dry_duration":11}}]}}}`))
	l.apply(u)
	d := l.amsUnits()[0].DrySetting
	if d == nil || *d.TempC != 65 || *d.Hours != 11 || d.Filament != "PETG" {
		t.Fatalf("partial %+v", d)
	}
	// The dry ended: the printer sends an idle setting.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"0","dry_time":0,
		"dry_setting":{"dry_temperature":-1,"dry_duration":-1,"dry_filament":""}}]}}}`))
	l.apply(u)
	if got := l.amsUnits()[0].DrySetting; got != nil {
		t.Fatalf("idle setting kept %+v", got)
	}
	// Wrongly typed new fields: the rest of the message still counts.
	u, ok := parseLive([]byte(`{"print":{"layer_num":7,"ams":{"tray_now":"1","ams":[{"id":"128","humidity_raw":"12",
		"dry_sf_reason":0,"dry_setting":"x"}]}}}`))
	if !ok || u.layer == nil || *u.layer != 7 || u.trayNow == nil || len(u.units) != 1 {
		t.Fatalf("bad types dropped the report: ok=%v %+v", ok, u)
	}
	if u.units[0].sfKnown || u.units[0].dryKnown || *u.units[0].HumidityRaw != 12 {
		t.Fatalf("bad types %+v", u.units[0])
	}
	// No RFID tag.
	l2 := live{}
	u, _ = parseLive([]byte(h2dHumidity))
	l2.apply(u)
	trays, _ := l2.snapshot()
	if trays[1].UUID != "" {
		t.Fatalf("zero uuid passed on: %q", trays[1].UUID)
	}
}

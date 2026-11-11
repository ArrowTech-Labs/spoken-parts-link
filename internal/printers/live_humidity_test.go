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

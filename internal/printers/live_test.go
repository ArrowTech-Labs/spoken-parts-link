package printers

import (
	"reflect"
	"testing"
)

// An H2D style full report: two AMS 2 Pro (A feeds the left nozzle, B the
// right), two nozzles, storage, a job with layers, an HMS and the mapping.
const h2dReport = `{"print":{"command":"push_status","gcode_state":"RUNNING","layer_num":20,"total_layer_num":160,
"subtask_name":"SP2-SP1052-1-A1B2","gcode_file":"/data/Metadata/plate_1.gcode","print_error":0,"sdcard":true,
"hms":[{"attr":50331904,"code":65543},{"attr":50331904,"code":196615}],"mapping":[0,256],
"ams":{"tray_now":"0","ams":[{"id":"0","info":"0103","tray":[{"id":"0","tray_type":"PLA","tray_color":"000000FF"},{"id":"1"},{"id":"2","tray_type":"PETG","tray_color":"FFFFFFFF"},{"id":"3","tray_type":""}]},
{"id":"1","info":"0003","tray":[{"id":"0","tray_type":"PLA","tray_color":"0000FFFF"}]}]},
"device":{"nozzle":{"info":[{"id":0,"diameter":0.4,"type":"HS01"},{"id":1,"diameter":0.6,"type":"HS01"}]}}}}`

func TestParseLiveH2D(t *testing.T) {
	u, ok := parseLive([]byte(h2dReport))
	if !ok {
		t.Fatal("not parsed")
	}
	var l live
	l.apply(u)
	if *l.layer != 20 || *l.total != 160 || l.subtask != "SP2-SP1052-1-A1B2" || l.gcodeFile != "/data/Metadata/plate_1.gcode" {
		t.Fatalf("job %+v", l)
	}
	if !reflect.DeepEqual(l.hms, []string{"HMS_0300_0100_0001_0007", "HMS_0300_0100_0003_0007"}) || !l.hmsFatal {
		t.Fatalf("hms %v %v", l.hms, l.hmsFatal)
	}
	if l.storage == nil || !*l.storage || !reflect.DeepEqual(l.mapping, []int{0, 256}) || *l.trayNow != 0 {
		t.Fatalf("storage/mapping %+v", l)
	}
	trays, nozzles := l.snapshot()
	want := []Tray{
		{AmsID: 0, SlotID: 0, Type: "PLA", Color: "#000000", Nozzle: "left", feeds: 1},
		{AmsID: 0, SlotID: 1, Nozzle: "left", feeds: 1},
		{AmsID: 0, SlotID: 2, Type: "PETG", Color: "#FFFFFF", Nozzle: "left", feeds: 1},
		{AmsID: 0, SlotID: 3, Nozzle: "left", feeds: 1},
		{AmsID: 1, SlotID: 0, Type: "PLA", Color: "#0000FF", Nozzle: "right", feeds: 0},
	}
	if !reflect.DeepEqual(trays, want) {
		t.Fatalf("trays\n got %+v\nwant %+v", trays, want)
	}
	if !reflect.DeepEqual(nozzles, []Nozzle{{0, "right", "0.4", "HS01"}, {1, "left", "0.6", "HS01"}}) {
		t.Fatalf("nozzles %+v", nozzles)
	}
	// A delta that changes one tray keeps the others; one with only a remain
	// field keeps the type.
	u, _ = parseLive([]byte(`{"print":{"ams":{"ams":[{"id":"0","info":"0103","tray":[{"id":"1","tray_type":"PLA","tray_color":"FF0000FF"},{"id":"2","remain":40}]}]}}}`))
	l.apply(u)
	trays, _ = l.snapshot()
	if trays[1].Type != "PLA" || trays[1].Color != "#FF0000" || trays[2].Type != "PETG" || len(trays) != 5 {
		t.Fatalf("delta %+v", trays)
	}
}

func TestParseLiveStorageAndCommandAnswers(t *testing.T) {
	cases := map[string]*bool{
		`{"print":{"sdcard":false}}`:                        boolPtr(false),
		`{"print":{"home_flag":256}}`:                       boolPtr(true),
		`{"print":{"home_flag":512}}`:                       boolPtr(false),
		`{"print":{"device":{"storage":[{"name":"usb"}]}}}`: boolPtr(true),
		`{"print":{"device":{"storage":[]}}}`:               boolPtr(false),
		`{"print":{"gcode_state":"IDLE"}}`:                  nil,
	}
	for in, want := range cases {
		u, _ := parseLive([]byte(in))
		if (u.storage == nil) != (want == nil) || (want != nil && *u.storage != *want) {
			t.Errorf("%s: storage %v", in, u.storage)
		}
	}
	u, _ := parseLive([]byte(`{"print":{"command":"project_file","result":"failed","reason":"mqtt message verify failed","err_code":84033543}}`))
	if u.command == nil || !u.command.Refused() || u.command.ErrCode != 84033543 {
		t.Fatalf("refusal %+v", u.command)
	}
	u, _ = parseLive([]byte(`{"print":{"command":"push_status","result":"success"}}`))
	if u.command != nil {
		t.Fatal("push_status is not a command answer")
	}
	// One nozzle printers: no sides.
	var l live
	u, _ = parseLive([]byte(`{"print":{"nozzle_diameter":"0.4","ams":{"ams":[{"id":"0","info":"0001","tray":[{"id":"0","tray_type":"PLA","tray_color":"000000FF"}]}]}}}`))
	l.apply(u)
	trays, nozzles := l.snapshot()
	if trays[0].Nozzle != "" || len(nozzles) != 1 || nozzles[0].Side != "" || nozzles[0].Diameter != "0.4" {
		t.Fatalf("one nozzle %+v %+v", trays, nozzles)
	}
}

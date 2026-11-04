package printers

import "testing"

func TestParseReport(t *testing.T) {
	u, ok := parseReport([]byte(`{"print":{"gcode_state":"RUNNING","mc_percent":42,"mc_remaining_time":70,"fun":"3EC18FFF9CFF"}}`))
	if !ok || *u.gcodeState != "RUNNING" || *u.percent != 42 || *u.remaining != 70 || u.devMode == nil || !*u.devMode {
		t.Fatalf("%+v", u)
	}
	u, _ = parseReport([]byte(`{"print":{"fun":"3EC1AFFF9CFF"}}`))
	if u.devMode == nil || *u.devMode {
		t.Fatal("signature bit set must mean dev mode off")
	}
	u, _ = parseReport([]byte(`{"print":{"fun":""}}`))
	if !u.devKnown || u.devMode != nil {
		t.Fatal("empty fun must mean unknown")
	}
	u, _ = parseReport([]byte(`{"print":{"mc_percent":"150","gcode_state":"WEIRD"}}`))
	if *u.percent != 100 || *u.gcodeState != "" {
		t.Fatalf("clamp or state: %+v", u)
	}
	u, _ = parseReport([]byte(`{"print":{"mc_percent":-3}}`))
	if *u.percent != 0 {
		t.Fatal("negative percent")
	}
	u, _ = parseReport([]byte(`{"info":{"module":[{"name":"ota","sw_ver":"01.04.20.00"},{"name":"ams","sw_ver":"9"}]}}`))
	if u.firmware != "01.04.20.00" {
		t.Fatalf("firmware %q", u.firmware)
	}
	u, _ = parseReport([]byte(`{"print":{"command":"push_status"}}`))
	if u.gcodeState != nil || u.percent != nil || u.devKnown {
		t.Fatal("absent fields must stay unset")
	}
	if _, ok := parseReport([]byte(`not json`)); ok {
		t.Fatal("bad json accepted")
	}
}

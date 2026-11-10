package sends

import (
	"reflect"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
)

func TestAmsMapping(t *testing.T) {
	m, m2 := amsMapping([]api.MapRow{{Filament: 1, AmsID: 0, SlotID: 2, Tray: 2}, {Filament: 3, AmsID: 128, SlotID: 0, Tray: 128}})
	if !reflect.DeepEqual(m, []int{2, -1, 128}) {
		t.Fatalf("ams_mapping %v", m)
	}
	if !reflect.DeepEqual(m2, []mqtt.AmsSlot{{AmsID: 0, SlotID: 2}, {AmsID: 255, SlotID: 255}, {AmsID: 128, SlotID: 0}}) {
		t.Fatalf("ams_mapping2 %v", m2)
	}
}

func TestWrongSlot(t *testing.T) {
	plan := []api.MapRow{{Filament: 1, AmsID: 0, SlotID: 0}, {Filament: 2, AmsID: 1, SlotID: 3}}
	if got := wrongSlot(plan, []int{0, 0x0103}); got != "" {
		t.Fatalf("right slots flagged: %q", got)
	}
	if got := wrongSlot(plan, []int{0, 0xFFFF}); got != "" {
		t.Fatalf("unused flagged: %q", got)
	}
	if got := wrongSlot(plan, []int{0x0002, 0x0103}); got != "Filament 1 prints from AMS A slot 3, not AMS A slot 1." {
		t.Fatalf("%q", got)
	}
	if got := wrongSlot(plan, []int{0, 0xFE00}); got != "Filament 2 prints from the external spool, not AMS B slot 4." {
		t.Fatalf("%q", got)
	}
}

func TestMatches(t *testing.T) {
	rec := &record{Subtask: "SP-0a1b2c3d-SP1052-P1"}
	for _, s := range []printers.Snapshot{
		{Subtask: "SP-0a1b2c3d-SP1052-P1"},
		{Subtask: "sp-0a1b2c3d-sp1052-p1.gcode.3mf"},
		{GcodeFile: "/media/usb0/SP-0a1b2c3d-SP1052-P1.gcode.3mf"},
	} {
		if !matches(rec, s) {
			t.Errorf("%+v not matched", s)
		}
	}
	for _, s := range []printers.Snapshot{{Subtask: "benchy"}, {Subtask: "SP-0a1b2c3e-SP1052-P1"}, {GcodeFile: "/data/Metadata/plate_1.gcode"}} {
		if matches(rec, s) {
			t.Errorf("%+v matched", s)
		}
	}
}

func TestRemovableNeverTheRunningPrintOrAForeignFile(t *testing.T) {
	r := &Runner{files: map[string][]ManifestEntry{"S1": {{FileName: "SP-11111111-A-P1.gcode.3mf", Size: 10}}}}
	idle := printers.Snapshot{GcodeState: "FINISH"}
	if !r.removable("S1", idle)("SP-11111111-A-P1.gcode.3mf", 10) {
		t.Fatal("our idle file not removable")
	}
	for _, c := range []struct {
		name string
		size int64
	}{{"SP-11111111-A-P1.gcode.3mf", 11}, {"SP-22222222-A-P1.gcode.3mf", 10}, {"benchy.gcode.3mf", 10}} {
		if r.removable("S1", idle)(c.name, c.size) {
			t.Errorf("%s (%d) removable", c.name, c.size)
		}
	}
	running := printers.Snapshot{GcodeState: "RUNNING", Subtask: "SP-11111111-A-P1"}
	if r.removable("S1", running)("SP-11111111-A-P1.gcode.3mf", 10) {
		t.Fatal("the running print's file is removable")
	}
	if r.removable("S2", idle)("SP-11111111-A-P1.gcode.3mf", 10) {
		t.Fatal("another printer's manifest was used")
	}
}

package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
)

func TestSyncRankKeepsConfiguredPrinters(t *testing.T) {
	now := time.Now()
	junk := printers.Snapshot{Access: printers.AccessMissing, LastSeen: now}
	coded := printers.Snapshot{Access: printers.AccessOK, LastSeen: now.Add(-time.Hour)}
	known := printers.Snapshot{Access: printers.AccessMissing, Known: true}
	if !(syncRank(coded) > syncRank(known) && syncRank(known) > syncRank(junk)) {
		t.Fatalf("ranks %d %d %d", syncRank(coded), syncRank(known), syncRank(junk))
	}
}

// ATS-SP-AMS-DRY-BUILD-1: the read-only drying extras reach the sync body under
// the agreed optional keys, and are left out when the printer didn't send them.
func TestSyncUnitsDryExtrasJSON(t *testing.T) {
	rh, temp, dry, setC, setH := 9.0, 84.0, 300, 85, 12
	got, err := json.Marshal(syncUnits([]printers.AmsUnit{
		{ID: 128, HumidityRaw: &rh, Temp: &temp, DryTime: &dry, InfoRaw: "1003", TrayExist: "1000F",
			DrySetting: &printers.DrySetting{TempC: &setC, Hours: &setH, Filament: "PAHT-CF"}},
		{ID: 0, HumidityRaw: &rh},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"ams_id":128,"humidity_raw":9,"temp":84,"dry_time":300,"info_raw":"1003",` +
		`"dry_setting":{"dry_temperature":85,"dry_duration":12,"dry_filament":"PAHT-CF"},"tray_exist":"1000F"},` +
		`{"ams_id":0,"humidity_raw":9}]`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

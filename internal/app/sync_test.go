package app

import (
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

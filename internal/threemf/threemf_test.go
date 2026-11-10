package threemf

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

func write(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plate.gcode.3mf")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheck(t *testing.T) {
	f := testutil.Sliced(1, testutil.SlicedFilament{ID: 1, Type: "PLA", Color: "#000000", Map: "1"}, testutil.SlicedFilament{ID: 2, Type: "PETG", Color: "#ffffffff", Map: "2"})
	info, err := Check(write(t, f.Data), 1)
	if err != nil {
		t.Fatal(err)
	}
	if info.GcodeMD5 != f.MD5 || len(info.Filaments) != 2 {
		t.Fatalf("%+v", info)
	}
	if info.Filaments[0] != (Filament{1, "PLA", "#000000", "left"}) || info.Filaments[1] != (Filament{2, "PETG", "#FFFFFF", "right"}) {
		t.Fatalf("%+v", info.Filaments)
	}
	if _, err := Check(write(t, f.Data), 2); !errors.Is(err, ErrNoGcode) {
		t.Fatalf("plate 2: %v", err)
	}
	if _, err := Check(write(t, []byte("not a zip")), 1); !errors.Is(err, ErrNotZip) {
		t.Fatalf("not zip: %v", err)
	}
	bad := testutil.Sliced(1)
	bad.Data = testutil.SlicedWithMD5(1, "00000000000000000000000000000000")
	if _, err := Check(write(t, bad.Data), 1); !errors.Is(err, ErrBadMD5) {
		t.Fatalf("bad md5: %v", err)
	}
	if _, err := Check(write(t, testutil.SlicedWithMD5(1, "")), 1); !errors.Is(err, ErrNoMD5) {
		t.Fatalf("no md5: %v", err)
	}
}

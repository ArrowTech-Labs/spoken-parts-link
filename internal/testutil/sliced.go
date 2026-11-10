package testutil

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SlicedFilament is one filament in a test .gcode.3mf. Map is the plate's
// filament_maps entry ("1" left nozzle, "2" right, "" none).
type SlicedFilament struct {
	ID    int
	Type  string
	Color string
	Map   string
}

// SlicedFile is a small, valid sliced Bambu file for tests.
type SlicedFile struct {
	Data   []byte
	MD5    string // of the plate G-code
	SHA256 string // of the whole file
}

func gcode(plate int) []byte {
	var b strings.Builder
	b.WriteString("; HEADER_BLOCK_START\n; total layer number: 3\n; HEADER_BLOCK_END\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "; CHANGE_LAYER\nG1 X%d Y%d E0.5\n", 10+i, 10+plate)
	}
	return []byte(b.String())
}

// Sliced builds a file with Metadata/plate_<plate>.gcode, its md5 and a
// slice_info.config listing the filaments.
func Sliced(plate int, fils ...SlicedFilament) SlicedFile {
	g := gcode(plate)
	sum := md5.Sum(g)
	data := build(plate, g, hex.EncodeToString(sum[:]), fils)
	s := sha256.Sum256(data)
	return SlicedFile{Data: data, MD5: hex.EncodeToString(sum[:]), SHA256: hex.EncodeToString(s[:])}
}

// SlicedWithMD5 builds a file whose md5 member says md5 ("" for none).
func SlicedWithMD5(plate int, md5hex string) []byte {
	return build(plate, gcode(plate), md5hex, nil)
}

func build(plate int, g []byte, md5hex string, fils []SlicedFilament) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(fmt.Sprintf("Metadata/plate_%d.gcode", plate))
	_, _ = w.Write(g)
	if md5hex != "" {
		w, _ = zw.Create(fmt.Sprintf("Metadata/plate_%d.gcode.md5", plate))
		_, _ = w.Write([]byte(md5hex))
	}
	var maps []string
	var rows strings.Builder
	for _, f := range fils {
		m := f.Map
		if m == "" {
			m = "1"
		}
		maps = append(maps, m)
		fmt.Fprintf(&rows, `<filament id="%d" type="%s" color="%s" used_g="3.1"/>`, f.ID, f.Type, f.Color)
	}
	w, _ = zw.Create("Metadata/slice_info.config")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><config><plate><metadata key="index" value="%d"/><metadata key="filament_maps" value="%s"/>%s</plate></config>`, plate, strings.Join(maps, " "), rows.String())
	_ = zw.Close()
	return buf.Bytes()
}

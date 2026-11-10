// Package threemf checks a sliced Bambu Studio file (.gcode.3mf) before it is
// uploaded to a printer: it must be a zip with the plate's G-code, and that
// G-code's md5 must match the md5 Studio wrote next to it. It also reads the
// plate's filaments (type, color, nozzle) from Metadata/slice_info.config.
package threemf

import (
	"archive/zip"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Limits for what is read out of the zip.
const (
	maxMeta  = 4 << 20
	maxGcode = 2 << 30
)

// Filament is one filament the plate lists.
type Filament struct {
	ID     int    // 1 based, as in Studio
	Type   string // "PLA"
	Color  string // "#RRGGBB"
	Nozzle string // "left", "right" or "" (one nozzle printers)
}

// Info is what the check found.
type Info struct {
	Plate     int
	GcodeMD5  string // lower case hex of Metadata/plate_<n>.gcode
	GcodeSize int64
	Filaments []Filament
}

// Errors.
var (
	ErrNotZip   = errors.New("the file is not a sliced 3MF (zip)")
	ErrNoGcode  = errors.New("the file has no G-code for this plate")
	ErrNoMD5    = errors.New("the file has no md5 for the plate G-code")
	ErrBadMD5   = errors.New("the plate G-code does not match its md5")
	ErrBadPlate = errors.New("bad plate number")
)

// Check opens the file at path and checks plate n.
func Check(path string, n int) (*Info, error) {
	if n < 1 || n > 99 {
		return nil, ErrBadPlate
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, ErrNotZip
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	name := fmt.Sprintf("Metadata/plate_%d.gcode", n)
	g := files[name]
	if g == nil || g.UncompressedSize64 == 0 || g.UncompressedSize64 > maxGcode {
		return nil, ErrNoGcode
	}
	want, err := readSmall(files[name+".md5"])
	if err != nil {
		return nil, ErrNoMD5
	}
	rc, err := g.Open()
	if err != nil {
		return nil, ErrNoGcode
	}
	h := md5.New()
	size, err := io.Copy(h, io.LimitReader(rc, maxGcode+1))
	rc.Close()
	if err != nil {
		return nil, ErrNoGcode
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return nil, ErrBadMD5
	}
	info := &Info{Plate: n, GcodeMD5: got, GcodeSize: size}
	if raw, err := readSmall(files["Metadata/slice_info.config"]); err == nil {
		info.Filaments = filaments(raw, n)
	}
	return info, nil
}

func readSmall(f *zip.File) ([]byte, error) {
	if f == nil || f.UncompressedSize64 > maxMeta {
		return nil, errors.New("missing")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxMeta))
}

type sliceInfo struct {
	Plates []struct {
		Meta []struct {
			Key   string `xml:"key,attr"`
			Value string `xml:"value,attr"`
		} `xml:"metadata"`
		Filaments []struct {
			ID    string `xml:"id,attr"`
			Type  string `xml:"type,attr"`
			Color string `xml:"color,attr"`
		} `xml:"filament"`
	} `xml:"plate"`
}

func filaments(raw []byte, n int) []Filament {
	var si sliceInfo
	if xml.Unmarshal(raw, &si) != nil {
		return nil
	}
	for _, p := range si.Plates {
		meta := map[string]string{}
		for _, m := range p.Meta {
			meta[m.Key] = m.Value
		}
		if meta["index"] != strconv.Itoa(n) {
			continue
		}
		maps := strings.Fields(meta["filament_maps"])
		var out []Filament
		for _, f := range p.Filaments {
			id, err := strconv.Atoi(f.ID)
			if err != nil || id < 1 || id > 64 {
				continue
			}
			fl := Filament{ID: id, Type: strings.TrimSpace(f.Type), Color: color(f.Color)}
			if id <= len(maps) {
				switch maps[id-1] {
				case "1":
					fl.Nozzle = "left"
				case "2":
					fl.Nozzle = "right"
				}
			}
			out = append(out, fl)
		}
		return out
	}
	return nil
}

func color(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) < 6 {
		return ""
	}
	if _, err := strconv.ParseUint(s[:6], 16, 32); err != nil {
		return ""
	}
	return "#" + strings.ToUpper(s[:6])
}

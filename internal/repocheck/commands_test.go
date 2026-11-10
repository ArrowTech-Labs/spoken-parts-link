package repocheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Bambu printer commands the link must never send: pause, stop, motion,
// temperature, fan, AMS, calibration, firmware, camera and lights.
var forbiddenCommands = []string{
	"pause", "resume", "stop", "gcode_line", "ams_change_filament", "ams_control",
	"ams_filament_setting", "ams_user_setting", "extrusion_cali", "extrusion_cali_set",
	"extrusion_cali_sel", "flowrate_cali", "print_speed", "calibration", "ledctrl",
	"ipcam_record_set", "ipcam_timelapse", "ipcam_resolution_set", "upgrade_confirm",
	"upgrade_consistency_confirm", "start_upgrade", "xcam_control_set", "unload_filament",
	"load_filament", "set_temp", "set_ctt", "skip_objects", "auto_home", "fan",
	"get_access_code", "set_accessories", "print_option", "set_airduct",
}

// TestPrinterCommandAllowList: the only commands the link can send are the
// constant mqtt.AllowedCommands (pushall and project_file). No Go source file
// names any other printer command, and only the mqtt package builds command
// JSON.
func TestPrinterCommandAllowList(t *testing.T) {
	src := read(t, "internal/mqtt/mqtt.go")
	if !strings.Contains(src, "var AllowedCommands = [...]string{\"pushall\", \"project_file\"}\n") {
		t.Fatal("mqtt.AllowedCommands must be exactly pushall and project_file")
	}
	forbidden := map[string]bool{}
	for _, c := range forbiddenCommands {
		forbidden[c] = true
	}
	var files []string
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	files = append(files, filepath.Join(root, "cmd/spoken-parts-link/main.go"))
	if len(files) < 20 {
		t.Fatalf("only %d files", len(files))
	}
	fset := token.NewFileSet()
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		tags := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if fl, ok := n.(*ast.Field); ok && fl.Tag != nil {
				tags[fl.Tag] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || tags[lit] {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if forbidden[v] {
				t.Errorf("%s: names the printer command %q", rel, v)
			}
			if strings.Contains(v, `"command"`) || v == "command" {
				switch rel {
				case "internal/mqtt/mqtt.go", "internal/testutil/testutil.go":
				default:
					t.Errorf("%s: builds printer command JSON (%q); only internal/mqtt may", rel, v)
				}
			}
			return true
		})
	}
}

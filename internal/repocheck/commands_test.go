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

// TestHomeAssistantAPIUse (0.5): the homeassistant_api permission gives the
// app a token that can do anything Home Assistant's REST API can. Only
// internal/haapi may use it, only for the two Spoken Parts Printers
// services, and the label printer code never touches a 3D printer path.
func TestHomeAssistantAPIUse(t *testing.T) {
	src := read(t, "internal/haapi/haapi.go")
	for _, want := range []string{
		"var AllowedServices = [...]string{\"list_printers\", \"print_raster\"}\n",
		"const Domain = \"spoken_parts_printers\"\n",
		"const DefaultBase = \"http://supervisor/core/api\"\n",
		"if !allowed(domain, service) {\n\t\treturn nil, ErrNotAllowed\n\t}",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/haapi/haapi.go lacks %q", want)
		}
	}
	var files []string
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	files = append(files, filepath.Join(root, "cmd/spoken-parts-link/main.go"))
	fset := token.NewFileSet()
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if rel != "internal/haapi/haapi.go" && (strings.Contains(v, "SUPERVISOR_TOKEN") || strings.Contains(strings.ToLower(v), "supervisor/") ||
				strings.Contains(v, "/services/") || strings.Contains(v, "/core/api")) {
				t.Errorf("%s: %q; only internal/haapi talks to the Supervisor", rel, v)
			}
			return true
		})
		if strings.HasPrefix(rel, "internal/labels/") || strings.HasPrefix(rel, "internal/haapi/") {
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range []string{"/internal/mqtt", "/internal/ftps", "/internal/printers", "/internal/sends", "/internal/camera", "/internal/rtsp"} {
					if strings.HasSuffix(path, bad) {
						t.Errorf("%s imports %s: label jobs never touch a 3D printer", rel, path)
					}
				}
			}
		}
	}
}

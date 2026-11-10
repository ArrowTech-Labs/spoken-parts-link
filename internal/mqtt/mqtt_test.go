package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const (
	serial = "01P00A000000001"
	code   = "AbCd1234"
)

func dial(t *testing.T, ca *testutil.CA, addr string) *tls.Conn {
	t.Helper()
	// Like real printers, the test leaf has only a common name, so Go's
	// hostname check cannot be used. Certificate checks are the printers
	// package's job; here we only need a TLS pipe.
	_ = ca
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: serial, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSessionPublishesOnlyOnePushAll(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	report := []byte(`{"print":{"gcode_state":"RUNNING","mc_percent":42}}`)
	b := testutil.NewBroker(t, ca.Leaf(t, serial), serial, code, report)
	c, err := Connect(dial(t, ca, b.Addr), serial, Options{Username: "bblp", Password: code, KeepAlive: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequestPushAll(); err != nil {
		t.Fatal(err)
	}
	if err := c.RequestPushAll(); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, func(p []byte) { got <- string(p) }) }()
	select {
	case p := <-got:
		if p != string(report) {
			t.Fatalf("report %q", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no report")
	}
	time.Sleep(200 * time.Millisecond) // several keepalive pings
	cancel()
	<-done
	pubs := b.Publishes()
	if len(pubs) != 1 {
		t.Fatalf("want exactly 1 publish, got %d: %+v", len(pubs), pubs)
	}
	if pubs[0].Topic != "device/"+serial+"/request" || pubs[0].Payload != PushAllPayload {
		t.Fatalf("publish %+v", pubs[0])
	}
	cn := b.Connects()
	if len(cn) != 1 || cn[0].Username != "bblp" || cn[0].Password != code {
		t.Fatalf("connect %+v", cn)
	}
}

func TestPushAllPayloadIsTheDocumentedOne(t *testing.T) {
	if PushAllPayload != `{"pushing":{"sequence_id":"0","command":"pushall","version":1,"push_target":1}}` {
		t.Fatal("pushall payload changed")
	}
}

func TestWrongCode(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	b := testutil.NewBroker(t, ca.Leaf(t, serial), serial, code)
	_, err := Connect(dial(t, ca, b.Addr), serial, Options{Username: "bblp", Password: "Wrong999"})
	var ce *ConnackError
	if !errors.As(err, &ce) || !ce.BadCredentials() {
		t.Fatalf("want bad credentials, got %v", err)
	}
	if strings.Contains(err.Error(), "Wrong999") {
		t.Fatal("password in error")
	}
	if len(b.Publishes()) != 0 {
		t.Fatal("published after a refused connect")
	}
}

func TestWatchdog(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	b := testutil.NewBroker(t, ca.Leaf(t, serial), serial, code)
	c, err := Connect(dial(t, ca, b.Addr), serial, Options{Username: "bblp", Password: code, KeepAlive: time.Hour, Watchdog: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = c.Run(context.Background(), nil)
	if !errors.Is(err, ErrWatchdog) || time.Since(start) > 2*time.Second {
		t.Fatalf("want watchdog, got %v after %v", err, time.Since(start))
	}
}

func TestBadSerialRefused(t *testing.T) {
	if _, err := Connect(nil, "../../#", Options{}); err == nil {
		t.Fatal("bad serial accepted")
	}
}

func TestPacketEncoding(t *testing.T) {
	p := pushAllPacket(serial)
	if p[0] != 0x30 {
		t.Fatalf("not a QoS 0 publish: %x", p[0])
	}
	if got := appendRemaining(nil, 321); len(got) != 2 || got[0] != 0xC1 || got[1] != 0x02 {
		t.Fatalf("remaining length %x", got)
	}
}

// TestPublishSurface reads this package's source and fails if anything
// other than the constant pushall request could be published.
func TestPublishSurface(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	exported := map[string]bool{}
	publishBuilders := 0
	publishCalls := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "0x30") {
			t.Errorf("%s: raw PUBLISH header byte 0x30", name)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				n := d.Name.Name
				if d.Recv != nil {
					n = recvName(d.Recv) + "." + n
				}
				if ast.IsExported(d.Name.Name) {
					exported[n] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.IsExported() {
								exported[id.Name] = true
							}
						}
					}
				}
			}
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "publishPacket" {
						publishCalls["publishPacket"] = append(publishCalls["publishPacket"], fd.Name.Name)
					}
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "typePublish" {
					publishBuilders++
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || len(n.Args) != 1 {
					return true
				}
				switch sel.Sel.Name {
				case "Write":
					if !allowedWriteArg(n.Args[0]) {
						t.Errorf("%s: unexpected Write argument at %v", name, fset.Position(n.Pos()))
					}
				case "write":
					if !isFixedPacket(n.Args[0], "typePingreq", "typePuback") {
						t.Errorf("%s: write of something other than PINGREQ or PUBACK at %v", name, fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
	// typePublish: its declaration, the PUBLISH case in Run, and the one
	// header byte in publishPacket.
	if publishBuilders != 3 {
		t.Errorf("typePublish used %d times, want 3 (decl, read switch, publishPacket)", publishBuilders)
	}
	for callee, callers := range publishCalls {
		_ = callee
		sort.Strings(callers)
		if got := strings.Join(callers, ","); got != "projectFilePacket,pushAllPacket" {
			t.Errorf("publishPacket called from %s, want only pushAllPacket and projectFilePacket", got)
		}
	}
	if len(publishCalls) != 1 {
		t.Error("publishPacket is not called the expected way")
	}
	var names []string
	for n := range exported {
		names = append(names, n)
	}
	sort.Strings(names)
	want := "AllowedCommands|AmsSlot|Client|Client.Close|Client.RequestPushAll|Client.Run|Client.StartProjectFile|ConnackError|ConnackError.BadCredentials|ConnackError.Error|Connect|DefaultMaxPacket|ErrAlreadyStarted|ErrBadCommand|ErrWatchdog|Options|ProjectFile|PushAllPayload"
	if got := strings.Join(names, "|"); got != want {
		t.Errorf("exported API changed:\n got %s\nwant %s", got, want)
	}
}

func recvName(fl *ast.FieldList) string {
	switch t := fl.List[0].Type.(type) {
	case *ast.StarExpr:
		return t.X.(*ast.Ident).Name
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// allowedWriteArg: conn.Write may only send the connect packet, the
// subscribe packet, the pushall packet, DISCONNECT, or (inside write) p.
func allowedWriteArg(e ast.Expr) bool {
	switch a := e.(type) {
	case *ast.Ident:
		return a.Name == "pkt" || a.Name == "p"
	case *ast.CallExpr:
		if id, ok := a.Fun.(*ast.Ident); ok {
			return id.Name == "subscribePacket" || id.Name == "pushAllPacket" || id.Name == "projectFilePacket"
		}
	case *ast.CompositeLit:
		return isFixedPacket(a, "typeDisconnect")
	}
	return false
}

func isFixedPacket(e ast.Expr, types ...string) bool {
	cl, ok := e.(*ast.CompositeLit)
	if !ok || len(cl.Elts) == 0 {
		return false
	}
	be, ok := cl.Elts[0].(*ast.BinaryExpr)
	if !ok {
		return false
	}
	id, ok := be.X.(*ast.Ident)
	if !ok {
		return false
	}
	for _, t := range types {
		if id.Name == t {
			return true
		}
	}
	return false
}

func TestAllowedCommandsAreExactlyPushallAndProjectFile(t *testing.T) {
	if got := strings.Join(AllowedCommands[:], ","); got != "pushall,project_file" {
		t.Fatalf("AllowedCommands = %s", got)
	}
}

func goodProjectFile() ProjectFile {
	return ProjectFile{
		Plate:        1,
		URL:          "ftp:///SP-0a1b2c3d-SP1052-P1.gcode.3mf",
		MD5:          "0123456789abcdef0123456789abcdef",
		SubtaskName:  "SP-0a1b2c3d-SP1052-P1",
		UseAMS:       true,
		AmsMapping:   []int{1, -1},
		AmsMapping2:  []AmsSlot{{AmsID: 0, SlotID: 1}, {AmsID: 255, SlotID: 255}},
		BedLeveling:  true,
		LayerInspect: true,
	}
}

func TestProjectFilePayloadAndChecks(t *testing.T) {
	b, err := goodProjectFile().payload("7")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"print":{"ams_mapping":[1,-1],"ams_mapping2":[{"ams_id":0,"slot_id":1},{"ams_id":255,"slot_id":255}],"bed_leveling":true,"bed_type":"auto","command":"project_file","flow_cali":false,"layer_inspect":true,"md5":"0123456789abcdef0123456789abcdef","param":"Metadata/plate_1.gcode","profile_id":"0","project_id":"0","sequence_id":"7","subtask_id":"0","subtask_name":"SP-0a1b2c3d-SP1052-P1","task_id":"0","timelapse":false,"url":"ftp:///SP-0a1b2c3d-SP1052-P1.gcode.3mf","use_ams":true,"vibration_cali":false}}`
	if string(b) != want {
		t.Fatalf("payload\n got %s\nwant %s", b, want)
	}
	bad := []func(*ProjectFile){
		func(p *ProjectFile) { p.Plate = 0 },
		func(p *ProjectFile) { p.URL = "ftp:///../../etc/passwd" },
		func(p *ProjectFile) { p.URL = "http://evil/SP2-X.gcode.3mf" },
		func(p *ProjectFile) { p.MD5 = "nope" },
		func(p *ProjectFile) { p.SubtaskName = `x","command":"stop` },
		func(p *ProjectFile) { p.AmsMapping2 = nil },
		func(p *ProjectFile) { p.AmsMapping = []int{300, 1} },
	}
	for i, f := range bad {
		p := goodProjectFile()
		f(&p)
		if _, err := p.payload("1"); !errors.Is(err, ErrBadCommand) {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestStartProjectFileOncePerSession(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	b := testutil.NewBroker(t, ca.Leaf(t, serial), serial, code)
	c, err := Connect(dial(t, ca, b.Addr), serial, Options{Username: "bblp", Password: code})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.StartProjectFile(goodProjectFile(), 1); err != nil {
		t.Fatal(err)
	}
	if err := c.StartProjectFile(goodProjectFile(), 2); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second start: %v", err)
	}
	p := goodProjectFile()
	p.MD5 = "x"
	waitPublishes(t, b, 1)
	pubs := b.Publishes()
	if len(pubs) != 1 || !strings.Contains(pubs[0].Payload, `"command":"project_file"`) {
		t.Fatalf("publishes %+v", pubs)
	}
}

func waitPublishes(t *testing.T, b *testutil.Broker, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.Publishes()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d publishes, got %d", n, len(b.Publishes()))
}

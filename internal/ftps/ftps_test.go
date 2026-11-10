package ftps

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const (
	serial = "01P00A000000001"
	code   = "Qz7Lp2Xw"
)

func config() *tls.Config {
	return &tls.Config{
		ServerName:         serial,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		InsecureSkipVerify: true, // the printers package checks the certificate
		VerifyConnection:   func(tls.ConnectionState) error { return nil },
	}
}

func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func login(t *testing.T, s *testutil.FTPServer, pass string) (*Client, error) {
	t.Helper()
	c, err := Dial(context.Background(), s.Addr, config(), dial, 0)
	if err != nil {
		t.Fatal(err)
	}
	return c, c.Login("bblp", pass)
}

func TestUploadResumesTheSessionAndChecksTheSize(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	c, err := login(t, s, code)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("G1 X1 Y1\n"), 50000)
	var last int64
	if err := c.Store(context.Background(), "/SP-0a1b2c3d-ABC-P1.gcode.3mf", bytes.NewReader(data), int64(len(data)), func(n int64) { last = n }); err != nil {
		t.Fatal(err)
	}
	if last != int64(len(data)) {
		t.Fatalf("progress %d", last)
	}
	if got := s.Files()["SP-0a1b2c3d-ABC-P1.gcode.3mf"]; !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes", len(got))
	}
	if r := s.Resumed(); len(r) != 1 || !r[0] {
		t.Fatalf("data connection resumed: %v", r)
	}
	if err := c.Delete("/SP-0a1b2c3d-ABC-P1.gcode.3mf"); err != nil {
		t.Fatal(err)
	}
	if len(s.Files()) != 0 {
		t.Fatal("not deleted")
	}
	c.Quit()
	for _, cmd := range s.Commands() {
		if strings.Contains(cmd, code) {
			t.Fatalf("code in a logged command: %q", cmd)
		}
	}
}

func TestNoStorageIs553(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	s.SetNoStorage(true)
	c, err := login(t, s, code)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	err = c.Store(context.Background(), "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil)
	if Code(err) != 553 || !strings.Contains(err.Error(), "Could not create file") {
		t.Fatalf("want 553, got %v", err)
	}
}

func TestBenign426IsCheckedBySize(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	s.SetReply426(true)
	c, err := login(t, s, code)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Store(context.Background(), "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abcdef"), 6, nil); err != nil {
		t.Fatalf("426 with a complete file: %v", err)
	}
}

func TestWrongPasswordIs530AndNeverInTheError(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	_, err := login(t, s, "Wrong999")
	if Code(err) != 530 || strings.Contains(err.Error(), "Wrong999") {
		t.Fatalf("got %v", err)
	}
}

func TestNoSessionReuseIsRefusedLikeVsftpd(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	cfg := config()
	cfg.ClientSessionCache = nil
	if _, err := Dial(context.Background(), s.Addr, cfg, dial, 0); err == nil {
		t.Fatal("a config without a session cache was accepted")
	}
	cfg = config()
	cfg.SessionTicketsDisabled = true // no tickets: the data connection can't resume
	c, err := Dial(context.Background(), s.Addr, cfg, dial, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("bblp", code); err != nil {
		t.Fatal(err)
	}
	err = c.Store(context.Background(), "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil)
	if Code(err) != 522 {
		t.Fatalf("want 522, got %v", err)
	}
}

func TestBadNamesAreRefusedBeforeSending(t *testing.T) {
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	c, err := login(t, s, code)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	for _, n := range []string{"../etc/passwd", "a b", "x\r\nDELE y", ""} {
		if err := c.Store(context.Background(), n, strings.NewReader("x"), 1, nil); err == nil {
			t.Errorf("%q accepted", n)
		}
		if err := c.Delete(n); err == nil {
			t.Errorf("delete %q accepted", n)
		}
	}
}

func TestListReadsVsftpdLines(t *testing.T) {
	e, ok := parseListLine("-rwxr-xr-x    1 103      107       2834512 Nov 10 02:41 SP-0a1b2c3d-X2D-P1.gcode.3mf")
	if !ok || e.Name != "SP-0a1b2c3d-X2D-P1.gcode.3mf" || e.Size != 2834512 || e.Dir {
		t.Fatalf("%+v %v", e, ok)
	}
	if e, ok := parseListLine("drwxr-xr-x    2 103      107          4096 Nov 10 02:41 my prints"); !ok || !e.Dir || e.Name != "my prints" {
		t.Fatalf("%+v", e)
	}
	ca := testutil.NewCA(t, "Test Printer CA")
	s := testutil.NewFTPServer(t, ca.Leaf(t, serial), code)
	s.Put("benchy.gcode.3mf", []byte("12345"))
	c, err := login(t, s, code)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	list, err := c.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != "benchy.gcode.3mf" || list[0].Size != 5 {
		t.Fatalf("%+v %v", list, err)
	}
}

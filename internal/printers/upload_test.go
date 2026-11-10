package printers

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

// uploadRig: one printer whose MQTT checks out, and an FTPS server with the
// given certificate on the same address.
func uploadRig(t *testing.T, ftpLeaf *testutil.CA, ftpCN string, report string, autoStart bool) (*Manager, *testutil.FTPServer, *testutil.Broker) {
	t.Helper()
	ca := testutil.NewCA(t, "Test Printer CA")
	if ftpLeaf == nil {
		ftpLeaf = ca
	}
	broker := testutil.NewBroker(t, ca.Leaf(t, serial1), serial1, goodCode, []byte(report))
	ftp := testutil.NewFTPServer(t, ftpLeaf.Leaf(t, ftpCN), goodCode)
	b, err := BundleFromPEM(ca.PEM)
	if err != nil {
		t.Fatal(err)
	}
	red := redact.New()
	red.Add(goodCode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	codes := &fakeCodes{m: map[string]string{serial1: goodCode}}
	m, err := New(ctx, Config{
		Bundle:   b,
		Codes:    codes,
		MetaPath: filepath.Join(t.TempDir(), "printers.json"),
		Log:      logx.New(&syncWriter{b: &bytes.Buffer{}}, red, logx.Debug),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(addr)
			var d net.Dialer
			switch port {
			case "8883":
				return d.DialContext(ctx, network, broker.Addr)
			case "990":
				return d.DialContext(ctx, network, ftp.Addr)
			}
			return d.DialContext(ctx, network, "127.0.0.1:"+port)
		},
		RetryMin:  10 * time.Millisecond,
		AutoStart: autoStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.SetEnabled(true)
	m.Observe(ssdp.Announcement{Serial: serial1, IP: netip.MustParseAddr("192.168.1.40"), ModelCode: "O1D"})
	waitFor(t, "connected", func() bool { s, _ := m.Get(serial1); return s.Connected && s.Access == AccessOK && s.GcodeState != "" })
	return m, ftp, broker
}

const cloudReport = `{"print":{"gcode_state":"FINISH","fun":"3EC1AFFF9CFF","sdcard":true}}`

func TestUploadChecksTheFTPSCertificateBeforeTheCode(t *testing.T) {
	evil := testutil.NewCA(t, "Not Bambu")
	m, ftp, _ := uploadRig(t, evil, serial1, cloudReport, false)
	err := m.Upload(context.Background(), serial1, "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil)
	if !errors.Is(err, ErrCertificate) {
		t.Fatalf("want ErrCertificate, got %v", err)
	}
	if len(ftp.Logins()) != 0 {
		t.Fatal("the access code went to a server whose certificate did not check out")
	}
}

func TestUploadChecksTheSerialInTheFTPSCertificate(t *testing.T) {
	m, ftp, _ := uploadRig(t, nil, "01P00A999999999", cloudReport, false)
	err := m.Upload(context.Background(), serial1, "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil)
	if !errors.Is(err, ErrCertificate) || len(ftp.Logins()) != 0 {
		t.Fatalf("another printer's certificate was accepted: %v", err)
	}
}

func TestUploadAndDelete(t *testing.T) {
	m, ftp, broker := uploadRig(t, nil, serial1, cloudReport, false)
	if err := m.Upload(context.Background(), serial1, "../evil.gcode.3mf", strings.NewReader("abc"), 3, nil); err == nil {
		t.Fatal("bad name accepted")
	}
	if err := m.Upload(context.Background(), serial1, "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil); err != nil {
		t.Fatal(err)
	}
	if string(ftp.Files()["SP-0a1b2c3d-X-P1.gcode.3mf"]) != "abc" {
		t.Fatal("not stored")
	}
	if err := m.Delete(context.Background(), serial1, "SP-0a1b2c3d-X-P1.gcode.3mf"); err != nil || len(ftp.Files()) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := m.Delete(context.Background(), serial1, "model.3mf"); err == nil {
		t.Fatal("deleted a file the link did not upload")
	}
	// No storage in the report: nothing is even tried.
	broker.Push([]byte(`{"print":{"sdcard":false}}`))
	waitFor(t, "no storage", func() bool { s, _ := m.Get(serial1); return s.Storage != nil && !*s.Storage })
	n := len(ftp.Logins())
	if err := m.Upload(context.Background(), serial1, "SP-0a1b2c3d-X-P1.gcode.3mf", strings.NewReader("abc"), 3, nil); !errors.Is(err, ErrNoStorage) || len(ftp.Logins()) != n {
		t.Fatalf("no storage: %v", err)
	}
}

func TestProjectFileOnlyWithAutoStartAndDeveloperMode(t *testing.T) {
	pf := mqtt.ProjectFile{Plate: 1, URL: "ftp:///SP-0a1b2c3d-X-P1.gcode.3mf", MD5: strings.Repeat("a", 32), SubtaskName: "SP-0a1b2c3d-X-P1"}
	m, _, broker := uploadRig(t, nil, serial1, cloudReport, true)
	if err := m.StartProjectFile(serial1, pf); !errors.Is(err, ErrAutoStart) {
		t.Fatalf("Cloud mode printer: %v", err)
	}
	m2, _, broker2 := uploadRig(t, nil, serial1, `{"print":{"gcode_state":"FINISH","fun":"3EC18FFF9CFF"}}`, false)
	if err := m2.StartProjectFile(serial1, pf); !errors.Is(err, ErrAutoStart) {
		t.Fatalf("auto start off: %v", err)
	}
	for _, b := range []*testutil.Broker{broker, broker2} {
		for _, p := range b.Publishes() {
			if p.Payload != mqtt.PushAllPayload {
				t.Fatalf("sent %q", p.Payload)
			}
		}
	}
}

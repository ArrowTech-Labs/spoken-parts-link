package printers

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

// cameraRig routes port 322 to a fake camera with its own certificate.
func cameraRig(t *testing.T, camCA *testutil.CA) (*rig, *testutil.RTSPServer) {
	t.Helper()
	bundle := testutil.NewCA(t, "Test Printer CA")
	r := newRig(t, bundle, bundle)
	if camCA == nil {
		camCA = bundle
	}
	cam := testutil.NewRTSPServer(t, camCA.Leaf(t, serial1), testutil.RTSPOptions{Code: goodCode})
	dial := r.m.cfg.Dial
	r.m.cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasSuffix(addr, ":322") {
			var d net.Dialer
			return d.DialContext(ctx, network, cam.Addr)
		}
		return dial(ctx, network, addr)
	}
	r.codes.set(serial1, goodCode)
	r.m.SetEnabled(true)
	r.m.Observe(announce("192.168.1.40"))
	waitFor(t, "connected", func() bool { s := r.snap(t); return s.Connected && s.Access == AccessOK })
	return r, cam
}

func TestWithCameraChecksTheCertificateFirst(t *testing.T) {
	r, cam := cameraRig(t, testutil.NewCA(t, "Rogue CA"))
	called := false
	err := r.m.WithCamera(context.Background(), serial1, func(net.Conn, string, string) error { called = true; return nil })
	if !errors.Is(err, ErrCameraCert) || called {
		t.Fatalf("err %v called %v", err, called)
	}
	time.Sleep(50 * time.Millisecond)
	if len(cam.Requests()) != 0 {
		t.Fatal("something was sent to a camera that did not check out")
	}
}

func TestWithCameraGivesTheCodeOnlyToF(t *testing.T) {
	r, _ := cameraRig(t, nil)
	var gotHost, gotCode string
	err := r.m.WithCamera(context.Background(), serial1, func(c net.Conn, host, code string) error {
		gotHost, gotCode = host, code
		return nil
	})
	if err != nil || gotHost != "192.168.1.40:322" || gotCode != goodCode {
		t.Fatalf("err %v host %q", err, gotHost)
	}
	if strings.Contains(r.log.String(), goodCode) {
		t.Fatal("code in the log")
	}
	if err := r.m.WithCamera(context.Background(), "UNKNOWN0001", func(net.Conn, string, string) error { return nil }); !errors.Is(err, ErrNoCamera) {
		t.Fatalf("unknown printer: %v", err)
	}
	feeds := r.m.Feeds()
	if len(feeds) != 1 || feeds[0].Serial != serial1 || !feeds[0].Connected || string(feeds[0].Report["gcode_state"]) != `"RUNNING"` {
		t.Fatalf("feeds %+v", feeds)
	}
}

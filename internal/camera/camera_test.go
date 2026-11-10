package camera

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"image/jpeg"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const (
	serial = "0948AB510700222"
	code   = "Vb51Nq7K"
)

func camServer(t *testing.T, opt testutil.RTSPOptions) *testutil.RTSPServer {
	t.Helper()
	ca := testutil.NewCA(t, "Test Printer CA")
	opt.Code = code
	return testutil.NewRTSPServer(t, ca.Leaf(t, serial), opt)
}

// dialer returns a DialFunc to the fake camera. Certificate checks are the
// printers package's job and are tested there and in package rtsp.
func dialer(addr string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		d := tls.Dialer{Config: &tls.Config{
			ServerName:         serial,
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // test only: see package rtsp's tests for the CA check
		}}
		return d.DialContext(ctx, "tcp", addr)
	}
}

// seg is one JPEG marker segment.
func seg(marker byte, body string) []byte {
	n := len(body) + 2
	return append([]byte{0xFF, marker, byte(n >> 8), byte(n)}, body...)
}

func craftedJPEG() []byte {
	var b []byte
	b = append(b, 0xFF, 0xD8)
	b = append(b, seg(0xE0, "JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")...)
	b = append(b, seg(0xE1, "Exif\x00\x00GPS 33.4N 112.0W serial "+serial)...)
	b = append(b, seg(0xE0, "JFXX\x00thumbnail")...)
	b = append(b, seg(0xFE, "comment "+code)...)
	b = append(b, seg(0xED, "Photoshop 3.0\x00IPTC")...)
	b = append(b, seg(0xE2, "ICC_PROFILE\x00")...)
	b = append(b, seg(0xDB, "\x00dqt-table")...)
	b = append(b, seg(0xC0, "\x08\x00\x10\x00\x10\x01\x01\x11\x00")...)
	b = append(b, seg(0xC4, "\x00huff")...)
	b = append(b, seg(0xDA, "\x01\x01\x00\x00\x3f\x00")...)
	b = append(b, 0x12, 0xFF, 0x00, 0x34, 0xFF, 0xD0, 0x56) // scan data with stuffing and RST0
	b = append(b, seg(0xFE, "late comment")...)             // between scans
	b = append(b, seg(0xDA, "\x01\x01\x00\x00\x3f\x00")...)
	b = append(b, 0x78, 0x9A)
	b = append(b, 0xFF, 0xD9)
	return b
}

func TestStripMetadata(t *testing.T) {
	out, err := StripMetadata(craftedJPEG())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Exif", "GPS", serial, "JFXX", "comment", code, "Photoshop", "ICC_PROFILE"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("%q survived", s)
		}
	}
	for _, s := range []string{"JFIF\x00", "dqt-table", "huff", "\x12\xFF\x00\x34\xFF\xD0\x56", "\x78\x9A\xFF\xD9"} {
		if !bytes.Contains(out, []byte(s)) {
			t.Errorf("%q lost", s)
		}
	}
	if !bytes.HasPrefix(out, []byte{0xFF, 0xD8, 0xFF, 0xE0}) {
		t.Fatalf("starts % x", out[:4])
	}
	again, err := StripMetadata(out)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatal("not idempotent")
	}
	full := craftedJPEG()
	for name, b := range map[string][]byte{
		"empty":       nil,
		"no SOI":      full[2:],
		"no EOI":      full[:len(full)-2],
		"bad length":  append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, 0xFF, 0xD9),
		"no scan":     append(append([]byte{0xFF, 0xD8}, seg(0xDB, "x")...), 0xFF, 0xD9),
		"not markers": {0xFF, 0xD8, 0x00, 0x00, 0xFF, 0xD9},
	} {
		if _, err := StripMetadata(b); !errors.Is(err, ErrJPEG) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// fakeFFmpeg writes a shell script that records its argv and environment,
// reads stdin and prints the crafted JPEG. It runs without a real ffmpeg.
func fakeFFmpeg(t *testing.T, extra string) (path, argvFile, envFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	jp := filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(jp, craftedJPEG(), 0o600); err != nil {
		t.Fatal(err)
	}
	argvFile, envFile, stdinFile = filepath.Join(dir, "argv"), filepath.Join(dir, "env"), filepath.Join(dir, "stdin")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argvFile + "'\n" +
		"/usr/bin/env > '" + envFile + "'\n" +
		"/bin/cat > '" + stdinFile + "'\n" + extra +
		"/bin/cat '" + jp + "'\n"
	path = filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return
}

func TestGrabChildSeesNoSecret(t *testing.T) {
	t.Setenv("SPL_TEST_ACCESS_CODE", code) // must not be inherited
	ff, argvFile, envFile, stdinFile := fakeFFmpeg(t, "")
	s := camServer(t, testutil.RTSPOptions{})
	var cmdArgs, cmdEnv []string
	g := &Grabber{FFmpeg: ff, Width: 640, Run: func(c *exec.Cmd) error {
		cmdArgs, cmdEnv = append([]string(nil), c.Args...), c.Env
		return c.Run()
	}}
	out, err := g.GrabDial(context.Background(), serial, s.Addr, code, dialer(s.Addr))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("Exif")) || !bytes.HasPrefix(out, []byte{0xFF, 0xD8}) {
		t.Fatal("output not stripped")
	}
	want := append([]string{ff}, Args(640)...)
	if strings.Join(cmdArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv %q", cmdArgs)
	}
	if cmdEnv == nil || len(cmdEnv) != 0 {
		t.Fatalf("env %q (must be empty, not inherited)", cmdEnv)
	}
	argv, _ := os.ReadFile(argvFile)
	env, _ := os.ReadFile(envFile)
	for _, b := range [][]byte{argv, env} {
		for _, bad := range []string{code, "rtsp", "://", "@", "bblp", s.Addr} {
			if bytes.Contains(b, []byte(bad)) {
				t.Fatalf("child saw %q in %q", bad, b)
			}
		}
	}
	for _, v := range []string{"SPL_TEST_ACCESS_CODE", "HOME=", "PATH=/"} {
		if bytes.Contains(env, []byte(v)) {
			t.Fatalf("inherited %q: %q", v, env)
		}
	}
	in, _ := os.ReadFile(stdinFile)
	if !bytes.HasPrefix(in, []byte{0, 0, 0, 1, 0x67}) || bytes.Contains(in, []byte(code)) {
		t.Fatalf("stdin % x", in[:min(8, len(in))])
	}
	if !strings.Contains(string(argv), "pipe:0") || !strings.Contains(string(argv), "scale=640:-2") {
		t.Fatalf("argv %q", argv)
	}
}

func TestToJPEGWithFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	for _, c := range []struct {
		clip  []byte
		width int
	}{{testutil.TestClip(), 320}, {testutil.H264Clip(t), 1024}} {
		s := camServer(t, testutil.RTSPOptions{Clip: c.clip})
		var argv, env []string
		g := &Grabber{FFmpeg: ff, Width: c.width, Run: func(cmd *exec.Cmd) error {
			argv, env = cmd.Args, cmd.Env
			return cmd.Run()
		}}
		start := time.Now()
		out, err := g.GrabDial(context.Background(), serial, s.Addr, code, dialer(s.Addr))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != c.width || cfg.Height != c.width*3/4 {
			t.Fatalf("got %dx%d, want width %d", cfg.Width, cfg.Height, c.width)
		}
		if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("Lavc")) {
			t.Fatal("metadata left in the JPEG")
		}
		for _, a := range append(argv, env...) {
			if strings.Contains(a, code) || strings.Contains(a, "://") {
				t.Fatalf("argv/env %q", a)
			}
		}
		if len(env) != 0 {
			t.Fatalf("env %q", env)
		}
		t.Logf("%d px: %d bytes in %v", c.width, len(out), time.Since(start))
	}
}

func TestToJPEGErrors(t *testing.T) {
	au := append([]byte{0, 0, 0, 1}, 0x67, 1, 2, 3)
	if _, err := ToJPEG(context.Background(), "ffmpeg", []byte("not h264"), 0); err == nil {
		t.Fatal("accepted junk input")
	}
	if _, err := ToJPEG(context.Background(), "ffmpeg", au, 99999); err == nil {
		t.Fatal("accepted width")
	}
	// No output.
	ff, _, _, _ := fakeFFmpeg(t, "exit 0\n")
	if _, err := ToJPEG(context.Background(), ff, au, 0); !errors.Is(err, ErrJPEG) {
		t.Fatalf("empty output: %v", err)
	}
	// Failure with stderr.
	ff, _, _, _ = fakeFFmpeg(t, "echo 'Invalid data found when processing input' >&2; exit 1\n")
	_, err := ToJPEG(context.Background(), ff, au, 0)
	if err == nil || !strings.Contains(err.Error(), "exit status 1: Invalid data found") {
		t.Fatalf("got %v", err)
	}
	// Timeout: killed.
	old := ffmpegTimeout
	ffmpegTimeout = 300 * time.Millisecond
	defer func() { ffmpegTimeout = old }()
	ff, _, _, _ = fakeFFmpeg(t, "exec /bin/sleep 30\n")
	start := time.Now()
	_, err = ToJPEG(context.Background(), ff, au, 0)
	if err == nil || !strings.Contains(err.Error(), "took longer") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	// Missing binary.
	if _, err := ToJPEG(context.Background(), filepath.Join(t.TempDir(), "nope"), au, 0); err == nil || strings.Contains(err.Error(), code) {
		t.Fatalf("got %v", err)
	}
}

func TestGrabErrorsHaveNoCode(t *testing.T) {
	ff, _, _, _ := fakeFFmpeg(t, "")
	for _, o := range []testutil.RTSPOptions{{OffStatus: 454}, {Basic: true}} {
		s := camServer(t, o)
		g := &Grabber{FFmpeg: ff}
		wrong := code
		if o.Basic {
			wrong = "Wr0ngC0de"
		}
		_, err := g.GrabDial(context.Background(), serial, s.Addr, wrong, dialer(s.Addr))
		if err == nil || strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), code) {
			t.Fatalf("%+v: %v", o, err)
		}
	}
}

func TestSameSerialIsBusy(t *testing.T) {
	g := &Grabber{FFmpeg: "/nonexistent"}
	release := make(chan struct{})
	entered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = g.GrabDial(context.Background(), serial, "127.0.0.1:1", code, func(ctx context.Context) (net.Conn, error) {
			close(entered)
			<-release
			return nil, errors.New("dial refused")
		})
	}()
	<-entered
	start := time.Now()
	if _, err := g.GrabDial(context.Background(), serial, "127.0.0.1:1", code, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if _, err := g.Grab(context.Background(), serial, c1, "127.0.0.1:1", code); !errors.Is(err, ErrBusy) {
		t.Fatalf("Grab: got %v", err)
	}
	if time.Since(start) > 100*time.Millisecond || !g.Busy(serial) {
		t.Fatal("ErrBusy was not immediate")
	}
	// Another printer is not blocked.
	_, err := g.GrabDial(context.Background(), "OTHER0000000001", "127.0.0.1:1", code, func(ctx context.Context) (net.Conn, error) {
		return nil, errors.New("dial refused")
	})
	if err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("other printer: %v", err)
	}
	close(release)
	wg.Wait()
	if g.Busy(serial) {
		t.Fatal("still busy")
	}
}

func TestFFmpegPath(t *testing.T) {
	if _, err := FFmpegPath("/nonexistent/ffmpeg"); err != nil && !errors.Is(err, ErrNoFFmpeg) {
		t.Fatal(err)
	}
	ff, _, _, _ := fakeFFmpeg(t, "")
	if p, err := FFmpegPath(ff); err != nil || p != ff {
		t.Fatalf("%q %v", p, err)
	}
}

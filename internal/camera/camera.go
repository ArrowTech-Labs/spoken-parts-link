// Package camera grabs one still from a printer camera: an H.264 key frame
// over RTSPS (package rtsp, in-process, so the access code stays in this
// process) turned into a JPEG by ffmpeg.
//
// ffmpeg reads the Annex-B bytes on stdin and writes the JPEG on stdout. Its
// argv is fixed (no URL, no secret) and its environment is empty. The JPEG
// is checked and stripped of every APPn segment but APP0 JFIF, and of
// comments, so no EXIF or other metadata leaves the device.
package camera

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/rtsp"
)

// Defaults.
const (
	DefaultFFmpeg  = "/usr/bin/ffmpeg" // Alpine's, allowed by the AppArmor profile
	DefaultWidth   = 1024
	DefaultUser    = "bblp"
	DefaultPath    = "/streaming/live/1"
	DefaultPort    = 322
	FFmpegTimeout  = 15 * time.Second
	MaxJPEG        = 2 << 20
	maxStderr      = 4 << 10
	minWidth       = 16
	maxWidth       = 4096
	stderrInErrMax = 200
)

// Errors.
var (
	ErrBusy     = errors.New("camera: a grab for this printer is already running")
	ErrNoFFmpeg = errors.New("camera: ffmpeg not found")
	ErrJPEG     = errors.New("camera: ffmpeg did not make a valid JPEG")
)

var ffmpegTimeout = FFmpegTimeout // tests shorten it

// FFmpegPath returns configured if it is an executable file, else ffmpeg
// from PATH (for tests and development).
func FFmpegPath(configured string) (string, error) {
	if configured != "" {
		if st, err := os.Stat(configured); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return configured, nil
		}
	}
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", ErrNoFFmpeg
	}
	return p, nil
}

// Args is ffmpeg's argv after the program name: input on stdin, one JPEG
// frame width pixels wide on stdout.
func Args(width int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "h264", "-i", "pipe:0",
		"-frames:v", "1",
		"-vf", "scale=" + strconv.Itoa(width) + ":-2",
		"-q:v", "5",
		"-f", "mjpeg", "pipe:1",
	}
}

// RunFunc runs a prepared command; tests use it to see argv and env.
type RunFunc func(*exec.Cmd) error

// ToJPEG decodes the Annex-B access unit with ffmpeg and returns a JPEG
// width pixels wide (0 means DefaultWidth), metadata stripped.
func ToJPEG(ctx context.Context, ffmpeg string, annexB []byte, width int) ([]byte, error) {
	return toJPEG(ctx, ffmpeg, annexB, width, nil)
}

// limited keeps the first max bytes and notes overflow; it never fails a
// write, so the child never blocks on a full pipe.
type limited struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room < len(p) {
		l.over = true
		if room > 0 {
			l.buf.Write(p[:room])
		}
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func toJPEG(ctx context.Context, ffmpeg string, annexB []byte, width int, run RunFunc) ([]byte, error) {
	if width == 0 {
		width = DefaultWidth
	}
	if width < minWidth || width > maxWidth {
		return nil, fmt.Errorf("camera: width %d out of range", width)
	}
	if len(annexB) < 5 || len(annexB) > rtsp.MaxFrame || !(bytes.HasPrefix(annexB, []byte{0, 0, 0, 1}) || bytes.HasPrefix(annexB, []byte{0, 0, 1})) {
		return nil, errors.New("camera: not an H.264 Annex-B frame")
	}
	if ffmpeg == "" {
		return nil, ErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, Args(width)...)
	cmd.Env = []string{} // nothing inherited
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(annexB)
	out := &limited{max: MaxJPEG}
	errb := &limited{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errb
	cmd.WaitDelay = 2 * time.Second
	if run == nil {
		run = (*exec.Cmd).Run
	}
	err := run(cmd)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("camera: ffmpeg took longer than %s", ffmpegTimeout)
		}
		return nil, fmt.Errorf("camera: ffmpeg: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("camera: ffmpeg failed: %v%s", exitText(err), stderrText(errb.buf.Bytes()))
	}
	if out.over {
		return nil, fmt.Errorf("%w: over %d bytes", ErrJPEG, MaxJPEG)
	}
	return StripMetadata(out.buf.Bytes())
}

func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return "exit status " + strconv.Itoa(ee.ExitCode())
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return "cannot run ffmpeg (" + pe.Err.Error() + ")"
	}
	return "cannot run ffmpeg"
}

// stderrText is the first line of ffmpeg's stderr, printable ASCII only.
func stderrText(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	out := make([]byte, 0, stderrInErrMax)
	for _, c := range b {
		if len(out) >= stderrInErrMax {
			break
		}
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return ": " + string(out)
}

// StripMetadata checks that b is a whole JPEG (SOI ... EOI) and returns a
// copy without APP1-APP15 and COM segments, and without APP0 segments other
// than JFIF (so no EXIF, XMP, ICC, thumbnails or comments).
func StripMetadata(b []byte) ([]byte, error) {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrJPEG, what) }
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, bad("no start marker")
	}
	if b[len(b)-2] != 0xFF || b[len(b)-1] != 0xD9 {
		return nil, bad("no end marker")
	}
	out := make([]byte, 0, len(b))
	out = append(out, 0xFF, 0xD8)
	i := 2
	sawSOS := false
	for {
		if i+1 >= len(b) || b[i] != 0xFF {
			return nil, bad("broken marker")
		}
		for i+1 < len(b) && b[i+1] == 0xFF { // fill bytes
			i++
		}
		if i+1 >= len(b) {
			return nil, bad("broken marker")
		}
		m := b[i+1]
		switch {
		case m == 0xD9:
			if !sawSOS {
				return nil, bad("no image data")
			}
			return append(out, 0xFF, 0xD9), nil
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7) || m == 0xD8 || m == 0x00:
			return nil, bad("unexpected marker")
		}
		if i+4 > len(b) {
			return nil, bad("short segment")
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if n < 2 || i+2+n > len(b) {
			return nil, bad("segment length")
		}
		seg := b[i : i+2+n]
		i += 2 + n
		drop := m == 0xFE || (m >= 0xE1 && m <= 0xEF) ||
			(m == 0xE0 && !(n >= 7 && bytes.Equal(seg[4:9], []byte("JFIF\x00"))))
		if !drop {
			out = append(out, seg...)
		}
		if m != 0xDA {
			continue
		}
		// Entropy-coded data runs to the next marker that is not a byte
		// stuffing (FF 00) or restart (FF D0-D7).
		sawSOS = true
		j := i
		for {
			if j+1 >= len(b) {
				return nil, bad("no end marker")
			}
			if b[j] == 0xFF && b[j+1] != 0x00 && !(b[j+1] >= 0xD0 && b[j+1] <= 0xD7) && b[j+1] != 0xFF {
				break
			}
			j++
		}
		out = append(out, b[i:j]...)
		i = j
	}
}

// DialFunc opens a verified TLS connection to the printer's camera port
// (the printers package does the dial with the Bambu CA check).
type DialFunc func(ctx context.Context) (net.Conn, error)

// Grabber grabs stills. Grabs for one printer never overlap: a second grab
// for a serial already being grabbed returns ErrBusy at once.
type Grabber struct {
	FFmpeg  string        // default DefaultFFmpeg, else ffmpeg on PATH
	Width   int           // default DefaultWidth
	Timeout time.Duration // the RTSP part; default rtsp.DefaultTimeout
	User    string        // default DefaultUser
	Path    string        // default DefaultPath
	Run     RunFunc       // test hook; nil runs the command

	mu   sync.Mutex
	busy map[string]bool
}

func (g *Grabber) lock(serial string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy == nil {
		g.busy = map[string]bool{}
	}
	if g.busy[serial] {
		return false
	}
	g.busy[serial] = true
	return true
}

func (g *Grabber) unlock(serial string) {
	g.mu.Lock()
	delete(g.busy, serial)
	g.mu.Unlock()
}

// Busy reports whether a grab for serial is running.
func (g *Grabber) Busy(serial string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy[serial]
}

// Grab pulls one key frame from the camera on conn (already verified;
// not closed here) and returns it as a JPEG. host is ip:port as used in the
// RTSP URL, code the printer's access code.
func (g *Grabber) Grab(ctx context.Context, serial string, conn net.Conn, host, code string) ([]byte, error) {
	if !g.lock(serial) {
		return nil, ErrBusy
	}
	defer g.unlock(serial)
	return g.grab(ctx, ctx, conn, host, code)
}

// GrabDial is Grab that takes the per-printer lock before dialing, and
// closes the connection it opened. g.Timeout bounds the dial and the RTSP
// session together; ffmpeg then has up to FFmpegTimeout.
func (g *Grabber) GrabDial(ctx context.Context, serial, host, code string, dial DialFunc) ([]byte, error) {
	if !g.lock(serial) {
		return nil, ErrBusy
	}
	defer g.unlock(serial)
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = rtsp.DefaultTimeout
	}
	// One deadline covers the dial and the RTSP session; ffmpeg has its own.
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial(rctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return g.grab(ctx, rctx, conn, host, code)
}

// grab runs the RTSP part under rctx and ffmpeg under ctx.
func (g *Grabber) grab(ctx, rctx context.Context, conn net.Conn, host, code string) ([]byte, error) {
	ff := g.FFmpeg
	if ff == "" {
		ff = DefaultFFmpeg
	}
	ff, err := FFmpegPath(ff)
	if err != nil {
		return nil, err
	}
	user, path := g.User, g.Path
	if user == "" {
		user = DefaultUser
	}
	if path == "" {
		path = DefaultPath
	}
	pw := []byte(code)
	au, err := rtsp.GrabIDR(rctx, conn, host, path, user, pw, g.Timeout)
	clear(pw)
	if err != nil {
		return nil, err
	}
	return toJPEG(ctx, ff, au, g.Width, g.Run)
}

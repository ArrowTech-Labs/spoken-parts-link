// Package rtsp is a minimal RTSP 1.0 client that pulls one H.264 key frame
// from a camera, such as a Bambu Lab printer's LAN live view
// (rtsps://<ip>:322/streaming/live/1, user bblp, password = access code).
//
// It runs over a connection the caller has already opened and verified (TLS
// with the printer CA check, as for MQTT): this package never dials. The
// session is OPTIONS, DESCRIBE, SETUP (RTP over the RTSP connection,
// interleaved channels), PLAY, then RTP H.264 depacketizing (RFC 6184) until
// the first complete IDR access unit, and a best effort TEARDOWN.
//
// The password is only used in-process: it goes into a Digest hash or, on
// rtsps only, a Basic header written straight to the connection. Request
// URLs never carry userinfo, and no error from this package contains the
// password, a header sent or text the camera answered (only status codes).
// Every read is bounded in size and the whole grab in time.
package rtsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Limits.
const (
	DefaultTimeout   = 15 * time.Second
	MaxFrame         = 2 << 20  // largest access unit returned (Annex-B bytes)
	DefaultMaxPacket = 65535    // largest interleaved frame accepted
	DefaultMaxBytes  = 64 << 20 // most bytes read from the camera in one grab
	maxHeaderBytes   = 16 << 10
	maxHeaderLines   = 64
	maxBody          = 64 << 10
	maxSkipped       = 32 // RTSP messages skipped while waiting for an answer
	maxNALsPerAU     = 256
)

// Errors. Every error returned wraps one of these or is a *StatusError.
var (
	ErrAuth = errors.New("rtsp: camera answered 401 after login")
	// ErrAuthMethod: the camera asks for a login this client does not do
	// (only Digest MD5/SHA-256 and, on rtsps only, Basic are supported).
	ErrAuthMethod = errors.New("rtsp: camera asks for a login this client does not support")
	ErrStreamOff  = errors.New("rtsp: camera stream is off")
	ErrTimeout    = errors.New("rtsp: timed out")
	ErrTooLarge   = errors.New("rtsp: camera sent more than the limit")
	ErrProtocol   = errors.New("rtsp: bad answer from the camera")
	ErrClosed     = errors.New("rtsp: camera closed the connection")
	ErrConfig     = errors.New("rtsp: bad request settings")
)

// StatusError is an RTSP answer other than 2xx (and not 401, 404 or 454).
type StatusError struct {
	Method string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rtsp: camera answered %d to %s", e.Code, e.Method)
}

// Config is one grab.
type Config struct {
	// URL is rtsp:// or rtsps:// without userinfo, for example
	// "rtsps://192.168.1.20:322/streaming/live/1".
	URL      string
	User     string
	Password []byte // not modified; wipe it yourself after the call
	Timeout  time.Duration
	// Limits; zero means the default.
	MaxFrame  int
	MaxPacket int
	MaxBytes  int64
}

// GrabIDR is Grab for host (ip:port) and path over rtsps.
func GrabIDR(ctx context.Context, conn net.Conn, host, path, user string, password []byte, timeout time.Duration) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return Grab(ctx, conn, Config{URL: "rtsps://" + host + path, User: user, Password: password, Timeout: timeout})
}

// Grab runs one RTSP session on conn and returns the first complete IDR
// access unit in Annex-B form: SPS, PPS and the IDR slice NAL units, each
// after a 00 00 00 01 start code. conn is not closed (the caller closes it)
// and its deadline is left set. ctx and cfg.Timeout bound the whole grab.
func Grab(ctx context.Context, conn net.Conn, cfg Config) ([]byte, error) {
	base, err := checkConfig(&cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })

	cr := &countReader{r: conn, left: cfg.MaxBytes}
	s := &session{
		conn: conn, cr: cr, br: bufio.NewReaderSize(cr, 8<<10), cfg: cfg,
		url: base, tls: strings.EqualFold(mustScheme(base), "rtsps"),
		pkt: make([]byte, cfg.MaxPacket),
	}
	defer s.wipe()
	au, err := s.run()
	stop()
	if s.session != "" {
		// Best effort: tell the camera we are done. Never read the answer.
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = s.send("TEARDOWN", s.playURL, nil)
	}
	if err != nil {
		return nil, s.mapErr(ctx, err)
	}
	return au, nil
}

func mustScheme(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme
}

func checkConfig(cfg *Config) (string, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxFrame <= 0 || cfg.MaxFrame > MaxFrame {
		cfg.MaxFrame = MaxFrame
	}
	if cfg.MaxPacket <= 0 || cfg.MaxPacket > DefaultMaxPacket {
		cfg.MaxPacket = DefaultMaxPacket
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if !safeText(cfg.User) || strings.ContainsAny(cfg.User, `:"\`) {
		return "", fmt.Errorf("%w: user name", ErrConfig)
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		// url.Parse errors quote the input; it has no userinfo by contract
		// but say nothing about it anyway.
		return "", fmt.Errorf("%w: camera URL does not parse", ErrConfig)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: camera URL must not have a user or password", ErrConfig)
	}
	if (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Host == "" || u.Opaque != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: camera URL must be rtsp:// or rtsps://host/path", ErrConfig)
	}
	s := u.String()
	if !safeText(s) || strings.Contains(s, " ") {
		return "", fmt.Errorf("%w: camera URL has bad characters", ErrConfig)
	}
	return s, nil
}

// safeText: printable ASCII, no CR, LF or other control characters.
func safeText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return len(s) <= 2048
}

type countReader struct {
	r    io.Reader
	left int64
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("%w: total bytes", ErrTooLarge)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

type session struct {
	conn       net.Conn
	cr         *countReader
	br         *bufio.Reader
	cfg        Config
	url        string // the request URL
	tls        bool
	cseq       int
	auth       *authState
	session    string
	playURL    string
	staleRetry bool
	phase      string
	rtpCh      int
	asm        *assembler // set from PLAY on
	done       []byte     // the access unit, once complete
	pkt        []byte
	skipped    int
}

func (s *session) wipe() {
	if s.auth != nil {
		s.auth.wipe()
	}
}

func (s *session) run() ([]byte, error) {
	s.phase = "OPTIONS"
	if _, err := s.do("OPTIONS", s.url, nil); err != nil {
		var se *StatusError
		if !errors.As(err, &se) {
			return nil, err
		}
		// Some cameras refuse OPTIONS; DESCRIBE decides.
	}

	s.phase = "DESCRIBE"
	resp, err := s.do("DESCRIBE", s.url, []string{"Accept: application/sdp"})
	if err != nil {
		return nil, err
	}
	if ct := resp.get("content-type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/sdp") {
		return nil, fmt.Errorf("%w: DESCRIBE is not SDP", ErrProtocol)
	}
	sd, err := parseSDP(resp.body)
	if err != nil {
		return nil, err
	}
	track, err := sd.h264()
	if err != nil {
		return nil, err
	}
	base := s.url
	for _, h := range []string{"content-base", "content-location"} {
		if v := resp.get(h); v != "" {
			if b, err := resolve(s.url, v, false); err == nil {
				base = b
				break
			}
		}
	}
	trackURL, err := resolve(base, track.control, true)
	if err != nil {
		return nil, err
	}
	s.playURL = base
	if sd.control != "" && sd.control != "*" {
		if p, err := resolve(base, sd.control, true); err == nil {
			s.playURL = p
		}
	}

	s.phase = "SETUP"
	resp, err = s.do("SETUP", trackURL, []string{"Transport: RTP/AVP/TCP;unicast;interleaved=0-1"})
	if err != nil {
		return nil, err
	}
	id, _, _ := strings.Cut(resp.get("session"), ";")
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 || !isToken(id) {
		return nil, fmt.Errorf("%w: SETUP has no usable Session", ErrProtocol)
	}
	s.session = id
	s.rtpCh = 0
	if ch, ok := interleaved(resp.get("transport")); ok {
		s.rtpCh = ch
	}

	s.asm = newAssembler(track.pt, s.cfg.MaxFrame)
	for _, ps := range track.sprop {
		s.asm.param(ps)
	}
	s.phase = "PLAY"
	if _, err := s.do("PLAY", s.playURL, []string{"Range: npt=0.000-"}); err != nil {
		return nil, err
	}
	s.phase = "stream"
	for s.done == nil {
		if err := s.next(); err != nil {
			return nil, err
		}
	}
	return s.done, nil
}

func (s *session) mapErr(ctx context.Context, err error) error {
	for _, known := range []error{ErrAuth, ErrAuthMethod, ErrStreamOff, ErrTooLarge, ErrProtocol, ErrConfig} {
		if errors.Is(err, known) {
			return err
		}
	}
	var se *StatusError
	if errors.As(err, &se) {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("rtsp: canceled: %w", context.Canceled)
	}
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		if s.phase == "stream" {
			return fmt.Errorf("%w: no IDR frame within %s", ErrTimeout, s.cfg.Timeout)
		}
		return fmt.Errorf("%w: camera did not answer %s within %s", ErrTimeout, s.phase, s.cfg.Timeout)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w (%s)", ErrClosed, s.phase)
	}
	// Errors from the connection itself (TLS alerts, resets). They carry no
	// request text.
	return fmt.Errorf("rtsp: %s: connection error: %w", s.phase, err)
}

// do sends a request, logging in when the camera answers 401.
func (s *session) do(method, uri string, extra []string) (*message, error) {
	for attempt := 0; ; attempt++ {
		if err := s.send(method, uri, extra); err != nil {
			return nil, err
		}
		resp, err := s.response()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.code == 401:
			ch, ok := pickChallenge(resp.all("www-authenticate"), s.tls)
			switch {
			case !ok && s.auth != nil:
				return nil, ErrAuth
			case !ok:
				return nil, ErrAuthMethod
			case s.auth == nil:
			case ch.stale && s.auth.digest && !s.staleRetry && attempt == 0:
				// The nonce expired: log in again once with the new one.
				s.staleRetry = true
				s.auth.wipe()
			default:
				return nil, ErrAuth
			}
			s.auth = newAuth(ch, s.cfg.User, s.cfg.Password)
			continue
		case resp.code == 404 || resp.code == 454:
			if method == "DESCRIBE" || method == "PLAY" || resp.code == 454 {
				return nil, ErrStreamOff
			}
			return nil, &StatusError{Method: method, Code: resp.code}
		case resp.code < 200 || resp.code > 299:
			return nil, &StatusError{Method: method, Code: resp.code}
		}
		return resp, nil
	}
}

// send writes one request. Only the Authorization header is secret; the
// buffer is wiped after the write.
func (s *session) send(method, uri string, extra []string) error {
	s.cseq++
	b := make([]byte, 0, 2048) // big enough not to grow, so one wipe covers it
	b = append(b, method...)
	b = append(b, ' ')
	b = append(b, uri...)
	b = append(b, " RTSP/1.0\r\nCSeq: "...)
	b = strconv.AppendInt(b, int64(s.cseq), 10)
	b = append(b, "\r\nUser-Agent: SpokenPartsLink\r\n"...)
	if s.session != "" {
		b = append(b, "Session: "...)
		b = append(b, s.session...)
		b = append(b, "\r\n"...)
	}
	if s.auth != nil {
		b = append(b, "Authorization: "...)
		b = s.auth.appendHeader(b, method, uri)
		b = append(b, "\r\n"...)
	}
	for _, h := range extra {
		b = append(b, h...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "\r\n"...)
	_, err := s.conn.Write(b)
	clear(b[:cap(b)])
	return err
}

// response reads until the answer with the current CSeq. Interleaved frames
// that arrive first are handled.
func (s *session) response() (*message, error) {
	for {
		m, err := s.message()
		if err != nil {
			return nil, err
		}
		if m == nil { // an interleaved frame
			continue
		}
		// Some servers leave CSeq out of answers: take those as ours.
		if cs := m.get("cseq"); m.code != 0 && (cs == strconv.Itoa(s.cseq) || cs == "") {
			return m, nil
		}
		s.skipped++
		if s.skipped > maxSkipped {
			return nil, fmt.Errorf("%w: too many unexpected messages", ErrProtocol)
		}
	}
}

// next handles one item from the connection while streaming.
func (s *session) next() error {
	m, err := s.message()
	if err != nil || m == nil {
		return err
	}
	// A request or stray answer from the camera: ignore it.
	s.skipped++
	if s.skipped > maxSkipped {
		return fmt.Errorf("%w: too many unexpected messages", ErrProtocol)
	}
	return nil
}

// message reads one interleaved frame (and returns nil) or one RTSP message.
func (s *session) message() (*message, error) {
	b, err := s.br.Peek(1)
	if err != nil {
		return nil, err
	}
	if b[0] == '$' {
		return nil, s.frame()
	}
	return readMessage(s.br)
}

func (s *session) frame() error {
	var h [4]byte
	if _, err := io.ReadFull(s.br, h[:]); err != nil {
		return err
	}
	n := int(h[2])<<8 | int(h[3])
	if n > s.cfg.MaxPacket {
		return fmt.Errorf("%w: RTP packet of %d bytes", ErrTooLarge, n)
	}
	p := s.pkt[:n]
	if _, err := io.ReadFull(s.br, p); err != nil {
		return err
	}
	if s.asm == nil || int(h[1]) != s.rtpCh || s.done != nil {
		return nil // RTCP, other channels, or before SETUP
	}
	au, err := s.asm.push(p)
	if err != nil {
		return err
	}
	if au != nil {
		s.done = au
	}
	return nil
}

func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			// RFC 2326 session ids may contain $-_.+ and alphanumerics.
			return false
		}
	}
	return true
}

// interleaved returns the RTP channel from a Transport header.
func interleaved(t string) (int, bool) {
	for _, p := range strings.Split(t, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(k, "interleaved") {
			continue
		}
		a, _, _ := strings.Cut(v, "-")
		n, err := strconv.Atoi(strings.TrimSpace(a))
		if err != nil || n < 0 || n > 255 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// message is one RTSP request or response. Header names are lower case.
type message struct {
	code    int    // responses
	method  string // requests
	headers []header
	body    []byte
}

type header struct{ k, v string }

func (m *message) get(k string) string {
	for _, h := range m.headers {
		if h.k == k {
			return h.v
		}
	}
	return ""
}

func (m *message) all(k string) []string {
	var out []string
	for _, h := range m.headers {
		if h.k == k {
			out = append(out, h.v)
		}
	}
	return out
}

func readLine(br *bufio.Reader, left *int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > *left {
			return "", fmt.Errorf("%w: header", ErrTooLarge)
		}
		*left -= len(chunk)
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func readMessage(br *bufio.Reader) (*message, error) {
	left := maxHeaderBytes
	var first string
	for blank := 0; first == ""; blank++ {
		if blank > 4 {
			return nil, fmt.Errorf("%w: empty lines", ErrProtocol)
		}
		l, err := readLine(br, &left)
		if err != nil {
			return nil, err
		}
		first = l
	}
	m := &message{}
	if strings.HasPrefix(first, "RTSP/") {
		parts := strings.SplitN(first, " ", 3)
		if len(parts) < 2 || len(parts[1]) != 3 {
			return nil, fmt.Errorf("%w: status line", ErrProtocol)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil || code < 100 || code > 999 {
			return nil, fmt.Errorf("%w: status line", ErrProtocol)
		}
		m.code = code
	} else {
		parts := strings.Fields(first)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "RTSP/") {
			return nil, fmt.Errorf("%w: not RTSP", ErrProtocol)
		}
		m.method = parts[0]
	}
	for n := 0; ; n++ {
		if n > maxHeaderLines {
			return nil, fmt.Errorf("%w: header lines", ErrTooLarge)
		}
		l, err := readLine(br, &left)
		if err != nil {
			return nil, err
		}
		if l == "" {
			break
		}
		if (l[0] == ' ' || l[0] == '\t') && len(m.headers) > 0 {
			m.headers[len(m.headers)-1].v += " " + strings.TrimSpace(l)
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return nil, fmt.Errorf("%w: header line", ErrProtocol)
		}
		m.headers = append(m.headers, header{strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)})
	}
	if cl := m.get("content-length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: Content-Length", ErrProtocol)
		}
		if n > maxBody {
			return nil, fmt.Errorf("%w: body of %d bytes", ErrTooLarge, n)
		}
		m.body = make([]byte, n)
		if _, err := io.ReadFull(br, m.body); err != nil {
			return nil, err
		}
	}
	return m, nil
}

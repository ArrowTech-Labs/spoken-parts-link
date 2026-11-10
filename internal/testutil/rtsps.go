package testutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clipH264 is a tiny H.264 Annex-B clip (160x120, 5 fps, 2 s, GOP 5, so two
// IDR frames with SPS/PPS in-band, SEI removed; about 8 KB). Made once with
//
//	ffmpeg -f lavfi -i testsrc=size=160x120:rate=5 -t 2 -c:v libx264 \
//	  -profile:v baseline -preset veryfast -g 5 -bf 0 -pix_fmt yuv420p \
//	  -bsf:v filter_units=remove_types=6 -f h264 clip.h264
//
// (ffmpeg 8.1.1, libx264). It is test video only, no secret.
//
//go:embed testdata/clip.h264
var clipH264 []byte

// TestClip returns the checked-in clip.
func TestClip() []byte { return append([]byte(nil), clipH264...) }

var (
	clipOnce sync.Once
	clipGen  []byte
)

// H264Clip returns a short Annex-B clip with at least two IDR frames: made
// now with ffmpeg (320x240) when ffmpeg and an H.264 encoder are available,
// else the checked-in 160x120 clip.
func H264Clip(t testing.TB) []byte {
	t.Helper()
	clipOnce.Do(func() { clipGen = generateClip() })
	if clipGen != nil {
		return append([]byte(nil), clipGen...)
	}
	return TestClip()
}

func generateClip() []byte {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	for _, enc := range []string{"libx264", "libopenh264"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=5", "-t", "2",
			"-c:v", enc, "-g", "5", "-bf", "0", "-pix_fmt", "yuv420p", "-f", "h264", "-")
		cmd.Env = []string{}
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		cancel()
		if err != nil {
			continue
		}
		var idr, sps, pps int
		for _, n := range SplitAnnexB(out.Bytes()) {
			switch n[0] & 0x1f {
			case 5:
				idr++
			case 7:
				sps++
			case 8:
				pps++
			}
		}
		if idr >= 2 && sps >= 1 && pps >= 1 {
			return out.Bytes()
		}
	}
	return nil
}

// SplitAnnexB splits an Annex-B byte stream into NAL units (without start
// codes or trailing zero bytes).
func SplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, bytes.TrimRight(b[start:], "\x00"))
	}
	var nals [][]byte
	for _, n := range out {
		if len(n) > 0 {
			nals = append(nals, n)
		}
	}
	return nals
}

// RTSPOptions configures a fake camera.
type RTSPOptions struct {
	Code  string // the access code (user bblp)
	Clip  []byte // Annex-B; nil means TestClip
	Basic bool   // Basic auth only (else Digest)
	Qop   bool   // Digest with qop=auth (else RFC 2069 style, like live555)
	// OffStatus, when set, is the answer to DESCRIBE (404 or 454: stream off).
	OffStatus int
	Stall     bool // never send an IDR frame (nor SPS/PPS)
	// CloseAfter closes the connection after this many RTP packets.
	CloseAfter int
	// Control is how the track URL is given: "relative" (default, with a
	// Content-Base), "relative-nobase", "absolute" or "star".
	Control  string
	NoSprop  bool // no sprop-parameter-sets in the SDP
	NoInband bool // strip SPS/PPS from the stream (needs the SDP's)
	// BigPacket, when set, sends one interleaved frame of this many bytes
	// right after PLAY.
	BigPacket int
	// GiantNAL, when set, sends an IDR NAL of this many bytes first.
	GiantNAL int
	Pace     time.Duration // between frames (default 5 ms)
	Path     string        // default /streaming/live/1
}

// RTSPRequest is one request the fake camera received.
type RTSPRequest struct {
	Method string
	URI    string
	Auth   string // "", "Digest" or "Basic"
	Raw    string // the whole request head
}

// RTSPServer is a fake Bambu printer camera (RTSPS, port 322 on a printer).
type RTSPServer struct {
	Addr string
	URL  string // rtsps://127.0.0.1:<port>/streaming/live/1, no userinfo

	opt   RTSPOptions
	ln    net.Listener
	nonce string

	mu    sync.Mutex
	reqs  []RTSPRequest
	conns []net.Conn
	sent  int
	wg    sync.WaitGroup
}

const fakeSession = "6A2F9C01"

// NewRTSPServer listens on 127.0.0.1 with cert (TLS 1.2, like the printer).
func NewRTSPServer(t testing.TB, cert tls.Certificate, opt RTSPOptions) *RTSPServer {
	t.Helper()
	if opt.Clip == nil {
		opt.Clip = TestClip()
	}
	if opt.Path == "" {
		opt.Path = "/streaming/live/1"
	}
	if opt.Pace == 0 {
		opt.Pace = 5 * time.Millisecond
	}
	var nb [16]byte
	_, _ = rand.Read(nb[:])
	s := &RTSPServer{opt: opt, nonce: hex.EncodeToString(nb[:])}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.ln, s.Addr = ln, ln.Addr().String()
	s.URL = "rtsps://" + s.Addr + opt.Path
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// Close stops the server.
func (s *RTSPServer) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Requests returns every request received.
func (s *RTSPServer) Requests() []RTSPRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RTSPRequest(nil), s.reqs...)
}

// Methods returns the method of every request received, in order.
func (s *RTSPServer) Methods() []string {
	var out []string
	for _, r := range s.Requests() {
		out = append(out, r.Method)
	}
	return out
}

// PacketsSent counts RTP packets sent (all connections).
func (s *RTSPServer) PacketsSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// TrackURL is the URL the client must SETUP.
func (s *RTSPServer) TrackURL() string {
	switch s.opt.Control {
	case "absolute":
		return s.URL + "/track1"
	case "star":
		return s.URL + "/"
	}
	return s.URL + "/trackID=1"
}

func (s *RTSPServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			s.serve(c)
		}()
	}
}

type rtspReq struct {
	method, uri string
	headers     map[string]string
	raw         string
}

func readRTSPRequest(r *bufio.Reader) (*rtspReq, error) {
	var raw strings.Builder
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	raw.WriteString(line)
	f := strings.Fields(line)
	if len(f) != 3 {
		return nil, fmt.Errorf("bad request line")
	}
	q := &rtspReq{method: f[0], uri: f[1], headers: map[string]string{}}
	for i := 0; i < 64; i++ {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		raw.WriteString(l)
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			q.raw = raw.String()
			if n, _ := strconv.Atoi(q.headers["content-length"]); n > 0 && n < 1<<16 {
				_, err := io.CopyN(io.Discard, r, int64(n))
				return q, err
			}
			return q, nil
		}
		k, v, _ := strings.Cut(l, ":")
		q.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return nil, fmt.Errorf("too many headers")
}

func digestParams(v string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			m[strings.ToLower(k)] = strings.Trim(val, `"`)
		}
	}
	return m
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

const fakeRealm = "LIVE555 Streaming Media"

func (s *RTSPServer) authorized(q *rtspReq) bool {
	a := q.headers["authorization"]
	if s.opt.Basic {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bblp:"+s.opt.Code))
		return a == want
	}
	scheme, rest, _ := strings.Cut(a, " ")
	if scheme != "Digest" {
		return false
	}
	p := digestParams(rest)
	if p["username"] != "bblp" || p["realm"] != fakeRealm || p["nonce"] != s.nonce || p["uri"] != q.uri {
		return false
	}
	ha1 := md5hex("bblp:" + fakeRealm + ":" + s.opt.Code)
	ha2 := md5hex(q.method + ":" + q.uri)
	want := md5hex(ha1 + ":" + s.nonce + ":" + ha2)
	if s.opt.Qop {
		if p["qop"] != "auth" || p["nc"] == "" || p["cnonce"] == "" {
			return false
		}
		want = md5hex(ha1 + ":" + s.nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	}
	return p["response"] == want
}

func (s *RTSPServer) challenge() string {
	if s.opt.Basic {
		return `Basic realm="` + fakeRealm + `"`
	}
	qop := ""
	if s.opt.Qop {
		qop = `, qop="auth"`
	}
	return `Digest realm="` + fakeRealm + `", nonce="` + s.nonce + `"` + qop
}

func (s *RTSPServer) sdp() string {
	var sps, pps []byte
	for _, n := range SplitAnnexB(s.opt.Clip) {
		switch n[0] & 0x1f {
		case 7:
			if sps == nil {
				sps = n
			}
		case 8:
			if pps == nil {
				pps = n
			}
		}
	}
	fmtp := "a=fmtp:96 packetization-mode=1"
	if !s.opt.NoSprop && sps != nil && pps != nil {
		fmtp += ";sprop-parameter-sets=" + base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)
	}
	control := "trackID=1"
	switch s.opt.Control {
	case "absolute":
		control = s.URL + "/track1"
	case "star":
		control = "*"
	}
	return "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=Bambu live\r\nt=0 0\r\na=control:*\r\na=range:npt=0-\r\n" +
		"m=audio 0 RTP/AVP 0\r\na=control:trackID=9\r\n" +
		"m=video 0 RTP/AVP 96\r\nc=IN IP4 0.0.0.0\r\nb=AS:500\r\na=rtpmap:96 H264/90000\r\n" + fmtp + "\r\na=control:" + control + "\r\n"
}

func (s *RTSPServer) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)
	var wmu sync.Mutex
	write := func(p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := c.Write(p)
		return err
	}
	done := make(chan struct{})
	defer close(done)
	reply := func(q *rtspReq, code int, reason string, headers []string, body string) {
		var b strings.Builder
		fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\nCSeq: %s\r\n", code, reason, q.headers["cseq"])
		for _, h := range headers {
			b.WriteString(h + "\r\n")
		}
		if body != "" {
			fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
		}
		b.WriteString("\r\n" + body)
		_ = write([]byte(b.String()))
	}
	for {
		if b, err := r.Peek(1); err == nil && b[0] == '$' {
			var h [4]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, r, int64(h[2])<<8|int64(h[3])); err != nil {
				return
			}
			continue
		}
		q, err := readRTSPRequest(r)
		if err != nil {
			return
		}
		auth, _, _ := strings.Cut(q.headers["authorization"], " ")
		s.mu.Lock()
		s.reqs = append(s.reqs, RTSPRequest{Method: q.method, URI: q.uri, Auth: auth, Raw: q.raw})
		s.mu.Unlock()
		if strings.Contains(q.uri, "@") {
			reply(q, 400, "Bad Request", nil, "")
			continue
		}
		if q.method != "OPTIONS" && !s.authorized(q) {
			reply(q, 401, "Unauthorized", []string{"WWW-Authenticate: " + s.challenge()}, "")
			continue
		}
		switch q.method {
		case "OPTIONS":
			reply(q, 200, "OK", []string{"Public: OPTIONS, DESCRIBE, SETUP, TEARDOWN, PLAY, PAUSE, GET_PARAMETER, SET_PARAMETER"}, "")
		case "DESCRIBE":
			if pathOf(q.uri) != pathOf(s.URL) {
				reply(q, 404, "Not Found", nil, "")
				continue
			}
			if s.opt.OffStatus != 0 {
				reply(q, s.opt.OffStatus, "Stream Off", nil, "")
				continue
			}
			var hs []string
			if s.opt.Control != "relative-nobase" {
				hs = append(hs, "Content-Base: "+s.URL+"/")
			}
			hs = append(hs, "Content-Type: application/sdp")
			reply(q, 200, "OK", hs, s.sdp())
		case "SETUP":
			if pathOf(q.uri) != pathOf(s.TrackURL()) {
				reply(q, 404, "Not Found", nil, "")
				continue
			}
			if !strings.Contains(q.headers["transport"], "RTP/AVP/TCP") || !strings.Contains(q.headers["transport"], "interleaved=0-1") {
				reply(q, 461, "Unsupported Transport", nil, "")
				continue
			}
			reply(q, 200, "OK", []string{"Transport: RTP/AVP/TCP;unicast;interleaved=0-1;ssrc=1A2B3C4D", "Session: " + fakeSession + ";timeout=65"}, "")
		case "PLAY":
			if q.headers["session"] != fakeSession {
				reply(q, 454, "Session Not Found", nil, "")
				continue
			}
			reply(q, 200, "OK", []string{"Session: " + fakeSession, "Range: npt=0.000-", "RTP-Info: url=" + s.TrackURL() + ";seq=1000;rtptime=0"}, "")
			_ = c.SetDeadline(time.Time{})
			go s.stream(c, write, done)
		case "TEARDOWN":
			reply(q, 200, "OK", nil, "")
			return
		default:
			reply(q, 405, "Method Not Allowed", nil, "")
		}
	}
}

type accessUnit struct{ nals [][]byte }

func (s *RTSPServer) units() []accessUnit {
	var aus []accessUnit
	var cur accessUnit
	for _, n := range SplitAnnexB(s.opt.Clip) {
		t := n[0] & 0x1f
		if (t == 7 || t == 8) && s.opt.NoInband {
			continue
		}
		cur.nals = append(cur.nals, n)
		if t == 1 || t == 5 {
			aus = append(aus, cur)
			cur = accessUnit{}
		}
	}
	if s.opt.Stall {
		var p []accessUnit
		for _, au := range aus {
			keepAU := true
			for _, n := range au.nals {
				if t := n[0] & 0x1f; t == 5 || t == 7 || t == 8 {
					keepAU = false
				}
			}
			if keepAU {
				p = append(p, au)
			}
		}
		aus = p
	}
	return aus
}

const fakeMTU = 1200

func (s *RTSPServer) stream(c net.Conn, write func([]byte) error, done chan struct{}) {
	var seq uint16 = 1000
	var ts uint32
	count := 0
	send := func(ch byte, pkt []byte) bool {
		frame := append([]byte{'$', ch, byte(len(pkt) >> 8), byte(len(pkt))}, pkt...)
		if write(frame) != nil {
			return false
		}
		if ch == 0 {
			s.mu.Lock()
			s.sent++
			s.mu.Unlock()
			count++
			if s.opt.CloseAfter > 0 && count >= s.opt.CloseAfter {
				_ = c.Close()
				return false
			}
		}
		return true
	}
	rtp := func(marker bool, payload []byte) bool {
		h := []byte{0x80, 96, byte(seq >> 8), byte(seq), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts), 0x1A, 0x2B, 0x3C, 0x4D}
		if marker {
			h[1] |= 0x80
		}
		seq++
		return send(0, append(h, payload...))
	}
	sendAU := func(nals [][]byte) bool {
		// Small NAL units go together in STAP-A, big ones in FU-A.
		var stap [][]byte
		flush := func(last bool) bool {
			if len(stap) == 0 {
				return true
			}
			var pl []byte
			if len(stap) == 1 {
				pl = stap[0]
			} else {
				pl = []byte{stap[0][0]&0x60 | 24}
				for _, n := range stap {
					pl = append(pl, byte(len(n)>>8), byte(len(n)))
					pl = append(pl, n...)
				}
			}
			stap = nil
			return rtp(last, pl)
		}
		size := 0
		for i, n := range nals {
			last := i == len(nals)-1
			if len(n) <= fakeMTU/2 && size+len(n)+3 <= fakeMTU {
				stap = append(stap, n)
				size += len(n) + 2
				if last && !flush(true) {
					return false
				}
				continue
			}
			if !flush(false) {
				return false
			}
			size = 0
			if len(n) <= fakeMTU {
				if !rtp(last, n) {
					return false
				}
				continue
			}
			hdr, body := n[0], n[1:]
			for off := 0; off < len(body); off += fakeMTU {
				end := min(off+fakeMTU, len(body))
				fu := []byte{hdr&0xe0 | 28, hdr & 0x1f}
				if off == 0 {
					fu[1] |= 0x80
				}
				if end == len(body) {
					fu[1] |= 0x40
				}
				if !rtp(last && end == len(body), append(fu, body[off:end]...)) {
					return false
				}
			}
		}
		return true
	}
	if s.opt.BigPacket > 0 {
		if !send(0, append([]byte{0x80, 96, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, make([]byte, s.opt.BigPacket-12)...)) {
			return
		}
	}
	if s.opt.GiantNAL > 0 {
		giant := make([]byte, s.opt.GiantNAL)
		giant[0], giant[1] = 0x65, 0x88
		if !sendAU([][]byte{giant}) {
			return
		}
		ts += 18000
	}
	aus := s.units()
	if len(aus) == 0 {
		return
	}
	// Start just after the first frame, so the client waits for an IDR.
	for i := 1; ; i++ {
		select {
		case <-done:
			return
		case <-time.After(s.opt.Pace):
		}
		if !sendAU(aus[i%len(aus)].nals) {
			return
		}
		ts += 90000 / 5
		// An RTCP sender report on channel 1, which the client ignores.
		sr := make([]byte, 28)
		sr[0], sr[1], sr[3] = 0x80, 200, 6
		if !send(1, sr) {
			return
		}
	}
}

// pathOf drops the scheme and host: like a printer, the fake camera answers
// for whatever address the client used to reach it.
func pathOf(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Host != "" {
		return u.Path
	}
	return uri
}

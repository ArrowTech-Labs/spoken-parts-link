package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
)

const (
	serial = "0948AB510700111"
	code   = "Kq83Zr0T"
)

// dial opens a TLS connection the way the link does: TLS 1.2 and the
// certificate checked against the (test) printer CA with CN = serial.
func dial(t *testing.T, ca *testutil.CA, addr string) net.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	cfg := &tls.Config{
		ServerName:         serial,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
				return err
			}
			if leaf.Subject.CommonName != serial {
				return errors.New("wrong name")
			}
			return nil
		},
	}
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func server(t *testing.T, opt testutil.RTSPOptions) (*testutil.RTSPServer, *testutil.CA) {
	t.Helper()
	ca := testutil.NewCA(t, "Test Printer CA")
	if opt.Code == "" {
		opt.Code = code
	}
	return testutil.NewRTSPServer(t, ca.Leaf(t, serial), opt), ca
}

func grab(t *testing.T, s *testutil.RTSPServer, ca *testutil.CA, pass string, cfg Config) ([]byte, error) {
	t.Helper()
	conn := dial(t, ca, s.Addr)
	if cfg.URL == "" {
		cfg.URL = s.URL
	}
	cfg.User = "bblp"
	cfg.Password = []byte(pass)
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	au, err := Grab(context.Background(), conn, cfg)
	if err != nil && strings.Contains(err.Error(), pass) {
		t.Fatalf("the password is in the error: %v", err)
	}
	return au, err
}

// wantAU is SPS, PPS and the second IDR of the clip (the server starts just
// after the first frame).
func wantAU(clip []byte, inbandSPS bool) []byte {
	var sps, pps, idr []byte
	idrs := 0
	for _, n := range testutil.SplitAnnexB(clip) {
		switch n[0] & 0x1f {
		case 7:
			if sps == nil || (inbandSPS && idrs == 1) {
				sps = n
			}
		case 8:
			if pps == nil || (inbandSPS && idrs == 1) {
				pps = n
			}
		case 5:
			idrs++
			if idrs == 2 {
				idr = n
			}
		}
	}
	var out []byte
	for _, n := range [][]byte{sps, pps, idr} {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func checkAnnexB(t *testing.T, au []byte) {
	t.Helper()
	nals := testutil.SplitAnnexB(au)
	if len(nals) < 3 || !bytes.HasPrefix(au, []byte{0, 0, 0, 1}) {
		t.Fatalf("%d NAL units", len(nals))
	}
	if nals[0][0]&0x1f != 7 || nals[1][0]&0x1f != 8 {
		t.Fatalf("starts with NAL types %d, %d", nals[0][0]&0x1f, nals[1][0]&0x1f)
	}
	for _, n := range nals[2:] {
		if n[0]&0x1f != 5 {
			t.Fatalf("NAL type %d in the frame", n[0]&0x1f)
		}
	}
}

func waitTeardown(t *testing.T, s *testutil.RTSPServer) {
	t.Helper()
	for i := 0; i < 200; i++ {
		m := s.Methods()
		if len(m) > 0 && m[len(m)-1] == "TEARDOWN" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no TEARDOWN: %v", s.Methods())
}

func checkRequests(t *testing.T, s *testutil.RTSPServer) {
	t.Helper()
	for _, r := range s.Requests() {
		if strings.Contains(r.URI, "@") || strings.Contains(r.URI, code) {
			t.Fatalf("request URI %q", r.URI)
		}
		if r.Auth != "Basic" && strings.Contains(r.Raw, code) {
			t.Fatalf("the code is in a request: %q", r.Raw)
		}
	}
}

func TestGrabDigest(t *testing.T) {
	for _, qop := range []bool{false, true} {
		s, ca := server(t, testutil.RTSPOptions{Qop: qop})
		au, err := grab(t, s, ca, code, Config{})
		if err != nil {
			t.Fatal(err)
		}
		checkAnnexB(t, au)
		if want := wantAU(testutil.TestClip(), true); !bytes.Equal(au, want) {
			t.Fatalf("qop %v: got %d bytes, want the clip's second IDR (%d bytes)", qop, len(au), len(want))
		}
		waitTeardown(t, s)
		got := strings.Join(s.Methods(), " ")
		if got != "OPTIONS DESCRIBE DESCRIBE SETUP PLAY TEARDOWN" {
			t.Fatalf("requests %s", got)
		}
		reqs := s.Requests()
		if reqs[1].Auth != "" || reqs[2].Auth != "Digest" || reqs[3].Auth != "Digest" || reqs[5].Auth != "Digest" {
			t.Fatalf("auth %+v", reqs)
		}
		if reqs[3].URI != s.TrackURL() || !strings.Contains(reqs[3].Raw, "interleaved=0-1") {
			t.Fatalf("SETUP %q", reqs[3].Raw)
		}
		if !strings.Contains(reqs[4].Raw, "Session: 6A2F9C01\r\n") {
			t.Fatalf("PLAY without the session id (no ;timeout): %q", reqs[4].Raw)
		}
		checkRequests(t, s)
	}
}

func TestGrabBasic(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Basic: true})
	au, err := grab(t, s, ca, code, Config{})
	if err != nil {
		t.Fatal(err)
	}
	checkAnnexB(t, au)
	waitTeardown(t, s)
	checkRequests(t, s)
}

func TestGrabIDRHelper(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{})
	conn := dial(t, ca, s.Addr)
	au, err := GrabIDR(context.Background(), conn, s.Addr, "/streaming/live/1", "bblp", []byte(code), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkAnnexB(t, au)
}

func TestBasicNeverOverPlainRTSP(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Basic: true})
	_, err := grab(t, s, ca, code, Config{URL: strings.Replace(s.URL, "rtsps://", "rtsp://", 1)})
	if !errors.Is(err, ErrAuthMethod) {
		t.Fatalf("got %v", err)
	}
	for _, r := range s.Requests() {
		if r.Auth != "" {
			t.Fatalf("sent %s auth", r.Auth)
		}
	}
}

func TestWrongCode(t *testing.T) {
	for _, basic := range []bool{false, true} {
		s, ca := server(t, testutil.RTSPOptions{Basic: basic})
		wrong := "Zz99Yy88"
		_, err := grab(t, s, ca, wrong, Config{})
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("basic %v: got %v", basic, err)
		}
		if strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), code) {
			t.Fatalf("code in %q", err)
		}
		if got := strings.Join(s.Methods(), " "); got != "OPTIONS DESCRIBE DESCRIBE" {
			t.Fatalf("basic %v: requests %s (must give up after one failed login)", basic, got)
		}
	}
}

func TestStreamOff(t *testing.T) {
	for _, st := range []int{404, 454} {
		s, ca := server(t, testutil.RTSPOptions{OffStatus: st})
		_, err := grab(t, s, ca, code, Config{})
		if !errors.Is(err, ErrStreamOff) {
			t.Fatalf("%d: got %v", st, err)
		}
	}
}

func TestStallTimesOut(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Stall: true})
	start := time.Now()
	_, err := grab(t, s, ca, code, Config{Timeout: 700 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "no IDR frame within") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	if s.PacketsSent() == 0 {
		t.Fatal("the server sent nothing")
	}
}

func TestContextCancel(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Stall: true})
	conn := dial(t, ca, s.Addr)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Grab(ctx, conn, Config{URL: s.URL, User: "bblp", Password: []byte(code), Timeout: 10 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestContextDeadline(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Stall: true})
	conn := dial(t, ca, s.Addr)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Grab(ctx, conn, Config{URL: s.URL, User: "bblp", Password: []byte(code)})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseMidStream(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{CloseAfter: 3})
	_, err := grab(t, s, ca, code, Config{})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedPacket(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{BigPacket: 3000})
	_, err := grab(t, s, ca, code, Config{MaxPacket: 2000})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedNAL(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{GiantNAL: MaxFrame + 1000})
	_, err := grab(t, s, ca, code, Config{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestTotalBytesLimit(t *testing.T) {
	s, ca := server(t, testutil.RTSPOptions{Stall: true})
	_, err := grab(t, s, ca, code, Config{MaxBytes: 20000})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestControlURLs(t *testing.T) {
	for _, c := range []string{"relative", "relative-nobase", "absolute", "star"} {
		s, ca := server(t, testutil.RTSPOptions{Control: c})
		au, err := grab(t, s, ca, code, Config{})
		if err != nil {
			t.Fatalf("%s: %v (requests %v)", c, err, s.Methods())
		}
		checkAnnexB(t, au)
	}
}

func TestParameterSets(t *testing.T) {
	for _, o := range []testutil.RTSPOptions{{NoSprop: true}, {NoInband: true}} {
		s, ca := server(t, o)
		au, err := grab(t, s, ca, code, Config{})
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		checkAnnexB(t, au)
	}
}

func TestGeneratedClip(t *testing.T) {
	clip := testutil.H264Clip(t)
	s, ca := server(t, testutil.RTSPOptions{Clip: clip})
	au, err := grab(t, s, ca, code, Config{})
	if err != nil {
		t.Fatal(err)
	}
	checkAnnexB(t, au)
	if want := wantAU(clip, true); !bytes.Equal(au, want) {
		t.Fatalf("got %d bytes, want %d", len(au), len(want))
	}
}

func TestConfigRefusesUserinfo(t *testing.T) {
	for _, u := range []string{
		"rtsps://bblp:" + code + "@127.0.0.1:322/streaming/live/1",
		"http://127.0.0.1/x",
		"rtsps:///x",
		"rtsps://127.0.0.1/a\r\nX: " + code,
	} {
		_, err := Grab(context.Background(), nil, Config{URL: u, User: "bblp", Password: []byte(code)})
		if !errors.Is(err, ErrConfig) || strings.Contains(err.Error(), code) {
			t.Fatalf("%q: %v", u, err)
		}
	}
}

func TestResolve(t *testing.T) {
	for _, c := range []struct{ base, ref, want string }{
		{"rtsps://h:322/streaming/live/1", "trackID=1", "rtsps://h:322/streaming/live/1/trackID=1"},
		{"rtsps://h:322/streaming/live/1/", "trackID=1", "rtsps://h:322/streaming/live/1/trackID=1"},
		{"rtsps://h:322/streaming/live/1/", "*", "rtsps://h:322/streaming/live/1/"},
		{"rtsps://h:322/streaming/live/1", "", "rtsps://h:322/streaming/live/1"},
		{"rtsps://h:322/streaming/live/1", "rtsp://other:554/x/track1", "rtsp://other:554/x/track1"},
		{"rtsps://h:322/streaming/live/1", "rtsps://u:secret@h:322/x", "rtsps://h:322/x"},
		{"rtsps://h:322/streaming/live/1", "/abs/track", "rtsps://h:322/abs/track"},
		{"rtsps://h:322/a/b", "track?ctype=video", "rtsps://h:322/a/b/track?ctype=video"},
	} {
		got, err := resolve(c.base, c.ref, true)
		if err != nil || got != c.want {
			t.Errorf("resolve(%q, %q) = %q, %v; want %q", c.base, c.ref, got, err, c.want)
		}
	}
	for _, ref := range []string{"http://h/x", "javascript:alert(1)", "a b"} {
		if _, err := resolve("rtsps://h/x", ref, true); err == nil {
			t.Errorf("%q resolved", ref)
		}
	}
}

func TestChallenges(t *testing.T) {
	cs := parseChallenges([]string{
		`Digest realm="R, x", nonce="abc", qop="auth,auth-int", algorithm=MD5, Basic realm="B"`,
		`Digest realm="S", nonce="n2", algorithm=SHA-256, stale=TRUE`,
	})
	if len(cs) != 3 || cs[0].realm != "R, x" || cs[0].nonce != "abc" || len(cs[0].qop) != 2 ||
		cs[1].scheme != "basic" || cs[1].realm != "B" || !cs[2].stale || cs[2].algorithm != "SHA-256" {
		t.Fatalf("%+v", cs)
	}
	c, ok := pickChallenge([]string{`Basic realm="x"`, `Digest realm="r", nonce="n"`}, true)
	if !ok || c.scheme != "digest" {
		t.Fatalf("%+v", c)
	}
	if _, ok := pickChallenge([]string{`Basic realm="x"`}, false); ok {
		t.Fatal("Basic picked without TLS")
	}
	if _, ok := pickChallenge([]string{`Digest realm="r", nonce="n", qop="auth-int"`}, true); ok {
		t.Fatal("auth-int only picked")
	}
	if _, ok := pickChallenge([]string{`Digest realm="r", nonce="n", algorithm=SHA-512-256`}, true); ok {
		t.Fatal("unknown algorithm picked")
	}
}

// RFC 2617 section 3.5 example.
func TestDigestVector(t *testing.T) {
	ch := challenge{scheme: "digest", realm: "testrealm@host.com", nonce: "dcd98b7102dd2f0e8b11d0f600bfb0c093", qop: []string{"auth"}, opaque: "5ccc069c403ebaf9f0171e9517f40e41"}
	a := newAuth(ch, "Mufasa", []byte("Circle Of Life"))
	a.cnonce = "0a4f113b"
	h := string(a.appendHeader(nil, "GET", "/dir/index.html"))
	if !strings.Contains(h, `response="6629fae49393a05397450978507c4ef1"`) || !strings.Contains(h, "nc=00000001") {
		t.Fatal(h)
	}
	if strings.Contains(h, "Circle Of Life") {
		t.Fatal("password in header")
	}
	a.wipe()
	if !bytes.Equal(a.ha1, make([]byte, len(a.ha1))) {
		t.Fatal("not wiped")
	}
}

func TestReadMessageLimits(t *testing.T) {
	long := "RTSP/1.0 200 OK\r\nX: " + strings.Repeat("a", maxHeaderBytes) + "\r\n\r\n"
	if _, err := readMessage(bufio.NewReader(strings.NewReader(long))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("long header: %v", err)
	}
	many := "RTSP/1.0 200 OK\r\n" + strings.Repeat("X: y\r\n", maxHeaderLines+2) + "\r\n"
	if _, err := readMessage(bufio.NewReader(strings.NewReader(many))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("many headers: %v", err)
	}
	body := "RTSP/1.0 200 OK\r\nContent-Length: 999999\r\n\r\n"
	if _, err := readMessage(bufio.NewReader(strings.NewReader(body))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("body: %v", err)
	}
}

func TestAssemblerSkipsBrokenFrames(t *testing.T) {
	nals := testutil.SplitAnnexB(testutil.TestClip())
	sps, pps, idr := nals[0], nals[1], nals[2]
	pkt := func(seq uint16, ts uint32, marker bool, pl []byte) []byte {
		h := []byte{0x80, 96, byte(seq >> 8), byte(seq), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts), 0, 0, 0, 1}
		if marker {
			h[1] |= 0x80
		}
		return append(h, pl...)
	}
	fu := func(start, end bool, part []byte) []byte {
		b := []byte{idr[0]&0xe0 | 28, idr[0] & 0x1f}
		if start {
			b[1] |= 0x80
		}
		if end {
			b[1] |= 0x40
		}
		return append(b, part...)
	}
	a := newAssembler(96, MaxFrame)
	a.param(sps)
	a.param(pps)
	body := idr[1:]
	half := len(body) / 2
	// Frame 1 loses its middle packet (seq 2): no frame.
	for _, p := range [][]byte{
		pkt(1, 100, false, fu(true, false, body[:half/2])),
		pkt(3, 100, true, fu(false, true, body[half:])),
	} {
		if au, err := a.push(p); err != nil || au != nil {
			t.Fatalf("broken frame returned: %v %d", err, len(au))
		}
	}
	// Frame 2 is whole.
	var au []byte
	for _, p := range [][]byte{
		pkt(4, 200, false, fu(true, false, body[:half])),
		pkt(5, 200, true, fu(false, true, body[half:])),
	} {
		var err error
		if au, err = a.push(p); err != nil {
			t.Fatal(err)
		}
	}
	if au == nil || !bytes.HasSuffix(au, idr) {
		t.Fatalf("no frame: %d", len(au))
	}
	// Wrong payload type and non-RTP are ignored.
	b := newAssembler(96, MaxFrame)
	if au, err := b.push(pkt(1, 1, true, idr)[:5]); au != nil || err != nil {
		t.Fatal("short packet")
	}
	p := pkt(1, 1, true, idr)
	p[1] = 0x80 | 97
	if au, err := b.push(p); au != nil || err != nil {
		t.Fatal("other payload type")
	}
}

// FuzzStream feeds arbitrary bytes to the interleaved reader, RTSP message
// parser and H.264 depacketizer: no panic, limits hold.
func FuzzStream(f *testing.F) {
	nals := testutil.SplitAnnexB(testutil.TestClip())
	rtp := func(seq byte, marker bool, pl []byte) []byte {
		h := []byte{0x80, 96, 0, seq, 0, 0, 0, 9, 0, 0, 0, 1}
		if marker {
			h[1] |= 0x80
		}
		p := append(h, pl...)
		return append([]byte{'$', 0, byte(len(p) >> 8), byte(len(p))}, p...)
	}
	stap := []byte{24, 0, byte(len(nals[0])), 0, 0}
	stap = append(stap[:3], nals[0]...)
	stap = append(stap, 0, byte(len(nals[1])))
	stap = append(stap, nals[1]...)
	idr := nals[2]
	seed := rtp(1, false, stap)
	seed = append(seed, rtp(2, false, append([]byte{idr[0]&0xe0 | 28, 0x80 | 5}, idr[1:100]...))...)
	seed = append(seed, rtp(3, true, append([]byte{idr[0]&0xe0 | 28, 0x40 | 5}, idr[100:]...))...)
	f.Add(seed)
	f.Add(append([]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 3\r\n\r\nabc"), seed...))
	f.Add([]byte("$\x01\x00\x04abcd$\x00\x00\x0c\x80\x60\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Add(rtp(1, true, []byte{28, 0xc5}))
	f.Fuzz(func(t *testing.T, data []byte) {
		cr := &countReader{r: bytes.NewReader(data), left: 1 << 20}
		s := &session{cr: cr, br: bufio.NewReader(cr), cfg: Config{MaxPacket: DefaultMaxPacket}, pkt: make([]byte, DefaultMaxPacket)}
		s.asm = newAssembler(96, 64<<10)
		for i := 0; i < 10000 && s.done == nil; i++ {
			if err := s.next(); err != nil {
				break
			}
		}
		if s.done != nil {
			if len(s.done) > 64<<10 || !bytes.HasPrefix(s.done, []byte{0, 0, 0, 1}) {
				t.Fatalf("bad frame of %d bytes", len(s.done))
			}
		}
	})
}

func FuzzSDPAndAuth(f *testing.F) {
	f.Add([]byte("v=0\r\na=control:*\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 sprop-parameter-sets=Z0LACg==,aM4PyA==\r\na=control:trackID=1\r\n"), `Digest realm="r", nonce="n", qop="auth"`)
	f.Fuzz(func(t *testing.T, body []byte, wa string) {
		if d, err := parseSDP(body); err == nil {
			if m, err := d.h264(); err == nil {
				_, _ = resolve("rtsps://h:322/streaming/live/1", m.control, true)
			}
		}
		if c, ok := pickChallenge([]string{wa}, true); ok {
			a := newAuth(c, "bblp", []byte(code))
			h := a.appendHeader(nil, "DESCRIBE", "rtsps://h/x")
			if bytes.Contains(h, []byte(code)) {
				t.Fatal("password in header")
			}
			if bytes.ContainsAny(h, "\r\n") {
				t.Fatal("CR or LF in header")
			}
		}
	})
}

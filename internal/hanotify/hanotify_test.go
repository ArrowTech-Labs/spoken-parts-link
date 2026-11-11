package hanotify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

const testID = "spokenparts-phone-0123456789abcdefABCDEF"

func newNotifier(t *testing.T, base string) (*Notifier, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets", "ha.enc")
	n := New(base, store.NewCodes(path), redact.New(), nil)
	key := make([]byte, 32)
	key[0] = 7
	if err := n.Unlock(key); err != nil {
		t.Fatal(err)
	}
	return n, path
}

func TestNotifyPostsToTheWebhook(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n, path := newNotifier(t, srv.URL)
	if ok, _ := n.Ready(); ok {
		t.Fatal("ready before set up")
	}
	if err := n.Set("short", monitor.Warning); err != ErrBadID {
		t.Fatalf("short id: %v", err)
	}
	if err := n.Set(testID, monitor.Critical); err != nil {
		t.Fatal(err)
	}
	if ok, min := n.Ready(); !ok || min != monitor.Critical {
		t.Fatalf("ready %v %v", ok, min)
	}
	err := n.Notify(context.Background(), monitor.Notice{Title: "Spoken Parts: H2D", Message: "Possible spaghetti", Severity: "critical", Key: "hms:x", Printer: "H2D", ImageURL: "https://spokenparts.com/f/1"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if gotPath != "/api/webhook/"+testID || got.Message != "Possible spaghetti" || got.ImageURL != "https://spokenparts.com/f/1" {
		t.Errorf("path %q payload %+v", gotPath, got)
	}
	mu.Unlock()
	// Stored encrypted: the id is not in the file.
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), testID) || len(b) == 0 {
		t.Fatal("webhook id stored in the clear")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := n.Clear(); err != nil || n.Configured() {
		t.Fatalf("clear %v", err)
	}
}

func TestNotifyErrorsDoNotNameTheID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	n, _ := newNotifier(t, srv.URL)
	_ = n.Set(testID, monitor.Warning)
	err := n.Notify(context.Background(), monitor.Notice{Message: "x"})
	if err == nil || strings.Contains(err.Error(), testID) {
		t.Fatalf("err %v", err)
	}
	srv.Close()
	err = n.Notify(context.Background(), monitor.Notice{Message: "x"})
	if err == nil || strings.Contains(err.Error(), testID) || strings.Contains(err.Error(), "webhook") {
		t.Fatalf("err %v", err)
	}
}

func TestRedactsTheID(t *testing.T) {
	red := redact.New()
	n := New("http://127.0.0.1:1", store.NewCodes(filepath.Join(t.TempDir(), "ha.enc")), red, nil)
	_ = n.Unlock(make([]byte, 32))
	_ = n.Set(testID, monitor.Warning)
	if out := red.Redact("url /api/webhook/" + testID); strings.Contains(out, testID) {
		t.Fatalf("not redacted: %s", out)
	}
}

// hits counts webhook requests that reached the handler.
type hits struct {
	mu sync.Mutex
	n  int
}

func (h *hits) handler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if r.URL.Path == "/api/webhook/"+testID {
			h.n++
		}
		h.mu.Unlock()
		w.WriteHeader(code)
	})
}

func (h *hits) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// sniffListener acts like Home Assistant's Python ssl on its HTTPS port: a
// connection that does not start with a TLS handshake gets a TLS alert and is
// closed. Handshakes go on to the TLS server.
type sniffListener struct {
	net.Listener
	plain *atomic.Int32
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (l sniffListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		br := bufio.NewReader(c)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, err := br.Peek(1)
		_ = c.SetReadDeadline(time.Time{})
		if err == nil && b[0] == 0x16 {
			return &peekedConn{c, br}, nil
		}
		l.plain.Add(1)
		_, _ = c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x46})
		_ = c.Close()
	}
}

// httpsOnly is a Home Assistant that serves only HTTPS, with a certificate
// that does not chain to any trusted root (like a self-made or Let's Encrypt
// certificate for another name).
func httpsOnly(t *testing.T, h *hits) (srv *httptest.Server, plain *atomic.Int32) {
	t.Helper()
	plain = &atomic.Int32{}
	srv = httptest.NewUnstartedServer(h.handler(200))
	srv.Listener = sniffListener{srv.Listener, plain}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, plain
}

func hostOf(u string) string { return u[strings.Index(u, "://")+3:] }

// autoNotifier is a notifier with no ha_url, pointed at host instead of
// 127.0.0.1:8123, logging to a buffer through a redactor that does not know
// the id (so the test sees what the code itself would write).
func autoNotifier(t *testing.T, host string) (*Notifier, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	n := New("", store.NewCodes(filepath.Join(t.TempDir(), "ha.enc")), redact.New(), logx.New(&buf, redact.New(), logx.Debug))
	n.autoHost = host
	if err := n.Unlock(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := n.Set(testID, monitor.Warning); err != nil {
		t.Fatal(err)
	}
	return n, &buf
}

func noSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, testID) || strings.Contains(s, "/api/webhook") {
		t.Fatalf("%s names the webhook: %s", what, s)
	}
}

var note = monitor.Notice{Title: "Spoken Parts", Message: "x", Severity: "info", Key: "test"}

func TestAutoUsesHTTPWhenItWorks(t *testing.T) {
	h := &hits{}
	srv := httptest.NewServer(h.handler(200))
	defer srv.Close()
	n, logs := autoNotifier(t, hostOf(srv.URL))
	for i := 0; i < 2; i++ {
		if err := n.Notify(context.Background(), note); err != nil {
			t.Fatal(err)
		}
	}
	if h.count() != 2 || n.learned != srv.URL {
		t.Fatalf("hits %d learned %q", h.count(), n.learned)
	}
	if !strings.Contains(logs.String(), "Home Assistant reached url="+srv.URL+` cert_check="none (http)"`) || strings.Count(logs.String(), "reached") != 1 {
		t.Fatalf("log: %s", logs)
	}
	noSecret(t, "log", logs.String())
}

func TestAutoFallsBackToHTTPSOnLoopback(t *testing.T) {
	h := &hits{}
	srv, plain := httpsOnly(t, h)
	n, logs := autoNotifier(t, hostOf(srv.URL))
	if err := n.Notify(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	// One plain attempt (refused with a TLS alert), then one push over HTTPS.
	if plain.Load() != 1 || h.count() != 1 || n.learned != srv.URL {
		t.Fatalf("plain %d hits %d learned %q", plain.Load(), h.count(), n.learned)
	}
	// Remembered: the next one goes straight to HTTPS.
	if err := n.Notify(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if plain.Load() != 1 || h.count() != 2 {
		t.Fatalf("plain %d hits %d", plain.Load(), h.count())
	}
	if !strings.Contains(logs.String(), "Home Assistant reached url="+srv.URL+` cert_check="off (loopback)"`) {
		t.Fatalf("log: %s", logs)
	}
	noSecret(t, "log", logs.String())
}

func TestAutoFallsBackWhenHTTPGets400(t *testing.T) {
	// Go's own TLS server answers plain HTTP with "400 Bad Request".
	h := &hits{}
	srv := httptest.NewTLSServer(h.handler(200))
	defer srv.Close()
	n, _ := autoNotifier(t, hostOf(srv.URL))
	if err := n.Notify(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if h.count() != 1 || n.learned != srv.URL {
		t.Fatalf("hits %d learned %q", h.count(), n.learned)
	}
}

func TestAutoBothFailNamesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host := hostOf(srv.URL)
	srv.Close() // nothing listens there now
	n, logs := autoNotifier(t, host)
	err := n.Notify(context.Background(), note)
	if err == nil || !strings.HasPrefix(err.Error(), "could not reach Home Assistant (http://"+host+": ") || !strings.Contains(err.Error(), "; https://"+host+": ") {
		t.Fatalf("err %v", err)
	}
	noSecret(t, "error", err.Error())
	noSecret(t, "log", logs.String())
	if n.learned != "" {
		t.Fatalf("learned %q", n.learned)
	}
}

func TestOnlyLoopbackSkipsTheCertificateCheck(t *testing.T) {
	nn := New("", store.NewCodes(filepath.Join(t.TempDir(), "ha.enc")), redact.New(), nil)
	if nn.verify.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("the normal client must check certificates")
	}
	for base, want := range map[string]bool{
		"https://127.0.0.1:8123":        true,
		"https://127.10.20.30:8123":     true,
		"https://[::1]:8123":            true,
		"https://[::ffff:127.0.0.1]:81": true,
		"http://127.0.0.1:8123":         false,
		"https://localhost:8123":        false, // names never count
		"https://192.168.2.75:8123":     false,
		"https://10.0.0.1":              false,
		"https://0.0.0.0:8123":          false,
		"https://[::]:8123":             false,
		"https://homeassistant.local":   false,
		"https://127.0.0.1.nip.io:8123": false,
	} {
		if got := skipsVerify(base); got != want {
			t.Errorf("skipsVerify(%s) = %v", base, got)
		}
		if (nn.client(base) == nn.loopback) != want {
			t.Errorf("client(%s) skips the check: %v", base, !want)
		}
	}
	// The client that skips the check connects only to loopback addresses,
	// whatever the URL said.
	for addr, ok := range map[string]bool{"127.0.0.1:8123": true, "[::1]:8123": true, "192.168.2.75:8123": false, "[fe80::1]:8123": false, "10.1.2.3:443": false} {
		if err := loopbackOnly("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("loopbackOnly(%s) = %v", addr, err)
		}
	}
	// A name for this host is checked like any other host: the test server's
	// certificate chains to no trusted root, so the push must fail.
	h := &hits{}
	srv := httptest.NewTLSServer(h.handler(200))
	defer srv.Close()
	n, _ := newNotifier(t, "https://localhost:"+srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	_ = n.Set(testID, monitor.Warning)
	err := n.Notify(context.Background(), note)
	if err == nil || !strings.Contains(err.Error(), "certificate") || h.count() != 0 {
		t.Fatalf("err %v hits %d", err, h.count())
	}
	noSecret(t, "error", err.Error())
}

func TestHAURLOverride(t *testing.T) {
	// https on a loopback address: certificate not checked, works.
	h := &hits{}
	tlsSrv, plain := httpsOnly(t, h)
	n, _ := newNotifier(t, tlsSrv.URL+"/")
	_ = n.Set(testID, monitor.Warning)
	if n.fixed != tlsSrv.URL || len(n.bases()) != 1 {
		t.Fatalf("fixed %q bases %v", n.fixed, n.bases())
	}
	if err := n.Notify(context.Background(), note); err != nil || h.count() != 1 || plain.Load() != 0 {
		t.Fatalf("err %v hits %d plain %d", err, h.count(), plain.Load())
	}
	// http:// set by the owner is used as is: no https try when it fails.
	n, _ = newNotifier(t, "http://"+hostOf(tlsSrv.URL))
	_ = n.Set(testID, monitor.Warning)
	err := n.Notify(context.Background(), note)
	if err == nil || h.count() != 1 || plain.Load() != 1 {
		t.Fatalf("err %v hits %d plain %d", err, h.count(), plain.Load())
	}
	noSecret(t, "error", err.Error())
}

func TestParseBase(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"   ":                          "",
		" https://192.168.2.75:8123/ ": "https://192.168.2.75:8123",
		"HTTP://127.0.0.1:8123":        "http://127.0.0.1:8123",
		"https://ha.example.com":       "https://ha.example.com",
		"https://[::1]:8123":           "https://[::1]:8123",
	} {
		if got, err := ParseBase(in); err != nil || got != want {
			t.Errorf("ParseBase(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{
		"127.0.0.1:8123", "ftp://h", "https://", "https://u:p@h:8123", "https://h:8123?x=1", "https://h#f", "https://h/ha",
		"http://127.0.0.1:8123/api/webhook/" + testID,
	} {
		got, err := ParseBase(in)
		if err != ErrBadURL || got != "" {
			t.Errorf("ParseBase(%q) = %q, %v", in, got, err)
		}
		noSecret(t, "error", err.Error())
	}
	// A pasted webhook address is refused and not logged; automatic is used.
	var buf bytes.Buffer
	n := New("http://127.0.0.1:8123/api/webhook/"+testID, store.NewCodes(filepath.Join(t.TempDir(), "ha.enc")), redact.New(), logx.New(&buf, redact.New(), logx.Debug))
	if n.fixed != "" || len(n.bases()) != 2 || !strings.Contains(buf.String(), "ha_url option ignored") {
		t.Fatalf("fixed %q log %s", n.fixed, buf.String())
	}
	noSecret(t, "log", buf.String())
}

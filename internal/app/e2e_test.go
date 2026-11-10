package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/identity"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/signer"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

const (
	serial     = "01P00A000000001"
	accessCode = "Qz7Lp2Xw" // made up for this test
	pairCode   = "K7P4QX9M"
	ingressIP  = "172.30.32.2"
)

// ---- fake backend ----

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type fakeLink struct {
	pub    ed25519.PublicKey
	status string // pending | active | revoked
}

type backend struct {
	t       *testing.T
	mu      sync.Mutex
	links   map[string]*fakeLink
	nonces  map[string]bool
	dataKey []byte
	reqs    []recorded
	syncs   []api.SyncRequest
	// badCheck makes POST pair answer a wrong check number.
	badCheck bool
	// unknown makes every signed call answer 401 unknown_link, before the
	// signature check, like the real backend for a link it does not know.
	unknown bool
	// throttlePair makes POST pair answer 429 with Retry-After.
	throttlePair bool
	pairCalls    int
	// extra answers routes the base fake does not know (sends, events).
	// It runs with b.mu held, after the signature check.
	extra func(w http.ResponseWriter, r *http.Request, body []byte, link *fakeLink) bool
}

func newBackend(t *testing.T) *backend {
	dk := make([]byte, 32)
	_, _ = rand.Read(dk)
	return &backend{t: t, links: map[string]*fakeLink{}, nonces: map[string]bool{}, dataKey: dk}
}

func (b *backend) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"detail":"%s","code":"%s"}`, code, code)
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, recorded{r.Method, r.URL.Path, r.Header.Clone(), body})
	if r.URL.Path == "/api/link/v1/pair" {
		b.pairCalls++
		if b.throttlePair {
			w.Header().Set("Retry-After", "120")
			b.fail(w, 429, "throttled")
			return
		}
	} else if b.unknown {
		b.fail(w, 401, "unknown_link")
		return
	}

	// Verify the signature exactly as the contract says.
	ts, err := strconv.ParseInt(r.Header.Get(signer.HeaderTimestamp), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 120*time.Second {
		b.t.Errorf("%s: bad timestamp %q", r.URL.Path, r.Header.Get(signer.HeaderTimestamp))
		b.fail(w, 401, "bad_timestamp")
		return
	}
	nonce := r.Header.Get(signer.HeaderNonce)
	if b.nonces[nonce] {
		b.t.Errorf("nonce reused: %s", nonce)
	}
	b.nonces[nonce] = true
	var pub ed25519.PublicKey
	var link *fakeLink
	linkID := r.Header.Get(signer.HeaderLink)
	if r.URL.Path == "/api/link/v1/pair" {
		if linkID != "" {
			b.t.Errorf("pair sent X-SP-Link")
		}
		var pr api.PairRequest
		if err := json.Unmarshal(body, &pr); err != nil {
			b.fail(w, 400, "invalid")
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(pr.PublicKey)
		pub = raw
	} else {
		link = b.links[linkID]
		if link == nil {
			b.t.Errorf("%s: unknown link %q", r.URL.Path, linkID)
			b.fail(w, 401, "unknown_link")
			return
		}
		pub = link.pub
	}
	if err := signer.Verify(pub, r.Method, r.URL.RequestURI(), r.Header.Get(signer.HeaderTimestamp), nonce, body, r.Header.Get(signer.HeaderSignature)); err != nil {
		b.t.Errorf("%s %s: signature: %v", r.Method, r.URL.Path, err)
		b.fail(w, 401, "bad_signature")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "POST /api/link/v1/pair":
		var pr api.PairRequest
		_ = json.Unmarshal(body, &pr)
		if pr.Code != pairCode || pr.Platform != "ha_addon" || pr.Name != "Spoken Parts Link" {
			b.fail(w, 400, "bad_code")
			return
		}
		id := newUUID()
		b.links[id] = &fakeLink{pub: pub, status: "pending"}
		check := identity.CheckNumber(pub)
		if b.badCheck {
			check = "9999"
			if identity.CheckNumber(pub) == check {
				check = "0000"
			}
		}
		w.WriteHeader(201)
		_, _ = fmt.Fprintf(w, `{"link_id":"%s","status":"pending","check_number":"%s","expires_at":"2026-11-04T09:15:00Z"}`, id, check)
	case "GET /api/link/v1/pair/status":
		switch link.status {
		case "revoked":
			b.fail(w, 401, "link_revoked")
		case "pending":
			_, _ = fmt.Fprintf(w, `{"status":"pending","check_number":"%s","site":null,"company":null}`, identity.CheckNumber(pub))
		default:
			_, _ = fmt.Fprintf(w, `{"status":"active","check_number":"%s","site":{"id":4,"name":"Gilbert"},"company":{"name":"Spoken Parts"}}`, identity.CheckNumber(pub))
		}
	case "POST /api/link/v1/data-key":
		switch link.status {
		case "revoked":
			b.fail(w, 401, "link_revoked")
		case "pending":
			b.fail(w, 403, "link_pending")
		default:
			_, _ = fmt.Fprintf(w, `{"data_key":"%s"}`, base64.StdEncoding.EncodeToString(b.dataKey))
		}
	case "POST /api/link/v1/sync":
		switch link.status {
		case "revoked":
			b.fail(w, 401, "link_revoked")
		case "pending":
			b.fail(w, 403, "link_pending")
		default:
			var sr api.SyncRequest
			if err := json.Unmarshal(body, &sr); err != nil {
				b.t.Errorf("sync body: %v", err)
			}
			b.syncs = append(b.syncs, sr)
			_, _ = w.Write([]byte(`{"next_sync_s":5,"paused":false,"status":"active","site":{"id":4,"name":"Gilbert"},"company":{"name":"Spoken Parts"},"sends":[],"cancel":[],"commands":[]}`))
		}
	default:
		if b.extra != nil && link != nil && b.extra(w, r, body, link) {
			return
		}
		b.fail(w, 404, "not_found")
	}
}

func newUUID() string {
	u := make([]byte, 16)
	_, _ = rand.Read(u)
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (b *backend) setAll(status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.links {
		if l.status != "revoked" {
			l.status = status
		}
	}
}

func (b *backend) lastSync() *api.SyncRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.syncs) == 0 {
		return nil
	}
	s := b.syncs[len(b.syncs)-1]
	return &s
}

func (b *backend) requests() []recorded {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recorded(nil), b.reqs...)
}

// ---- harness ----

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	t       *testing.T
	dir     string
	be      *backend
	srv     *httptest.Server
	broker  *testutil.Broker
	ca      *testutil.CA
	logs    *lockedBuf
	app     *App
	handler http.Handler
	cancel  context.CancelFunc
	done    chan struct{}
	ssdp    *ssdp.Listener
	stopped bool
}

func newHarness(t *testing.T, dir string, be *backend, srv *httptest.Server, broker *testutil.Broker, ca *testutil.CA, logs *lockedBuf, mods ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, be: be, srv: srv, broker: broker, ca: ca, logs: logs, done: make(chan struct{})}
	red := redact.New()
	lg := logx.New(logs, red, logx.Debug)
	bundle, err := printers.BundleFromPEM(ca.PEM)
	if err != nil {
		t.Fatal(err)
	}
	u, err := api.ParseServer(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	cfg := Config{
		DataDir:        dir,
		Version:        "0.1.0",
		Server:         u,
		Log:            lg,
		Bundle:         bundle,
		PollEvery:      20 * time.Millisecond,
		PendingTimeout: time.Minute,
		SyncMin:        20 * time.Millisecond,
		SyncMax:        50 * time.Millisecond,
		RetryMin:       10 * time.Millisecond,
		RetryMax:       50 * time.Millisecond,
		PrinterRetry:   10 * time.Millisecond,
		PrinterDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "192.168.1.40:8883" {
				t.Errorf("dialed %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, broker.Addr)
		},
	}
	for _, m := range mods {
		m(&cfg)
	}
	h.app, err = New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := web.New(h.app, web.Options{Allow: []netip.Addr{netip.MustParseAddr(ingressIP)}, Log: lg, Red: red})
	if err != nil {
		t.Fatal(err)
	}
	h.handler = ws.Handler()
	conn, err := ssdp.Listen("127.0.0.1:0", false, lg)
	if err != nil {
		t.Fatal(err)
	}
	h.ssdp = ssdp.NewListener(conn, lg, h.app.Observe)
	go func() { _ = h.ssdp.Run(ctx) }()
	go func() { h.app.Run(ctx); close(h.done) }()
	return h
}

func (h *harness) stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.cancel()
	<-h.done
	h.app.Close()
}

func (h *harness) get(path string) string {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = ingressIP + ":50000"
	r.Header.Set("X-Ingress-Path", "/api/hassio_ingress/testtoken")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		h.t.Fatalf("GET %s: %d", path, w.Code)
	}
	return w.Body.String()
}

func (h *harness) post(path string, form url.Values) string {
	page := h.get("/")
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		h.t.Fatal("no csrf token")
	}
	form.Set("csrf", m[1])
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.RemoteAddr = ingressIP + ":50000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Ingress-Path", "/api/hassio_ingress/testtoken")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		h.t.Fatalf("POST %s: %d", path, w.Code)
	}
	return w.Header().Get("Location")
}

// sendSSDP delivers a printer announcement as if it came from the
// printer's own LAN address (the listener only accepts a Location equal to
// a private sender address).
func (h *harness) sendSSDP() {
	h.ssdp.Handle(netip.MustParseAddr("192.168.1.40"), testutil.SSDPPacket(serial, "192.168.1.40", "O1D", "H2D"))
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newWorld(t *testing.T) (string, *backend, *httptest.Server, *testutil.Broker, *testutil.CA, *lockedBuf) {
	dir := t.TempDir()
	be := newBackend(t)
	srv := httptest.NewServer(be)
	t.Cleanup(srv.Close)
	ca := testutil.NewCA(t, "Test Printer CA")
	report := []byte(`{"print":{"command":"push_status","gcode_state":"RUNNING","mc_percent":42,"mc_remaining_time":70,"fun":"3EC18FFF9CFF"}}`)
	broker := testutil.NewBroker(t, ca.Leaf(t, serial), serial, accessCode, report)
	return dir, be, srv, broker, ca, &lockedBuf{}
}

// TestEndToEnd: pair, approve, sync, save an access code, connect to the
// printer, receive a report, then check that the access code never left the
// device or reached a log or a plain file. Then restart, then revoke.
func TestEndToEnd(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs)

	// 1. Pair through the page.
	if loc := h.post("/pair", url.Values{"code": {"k7p4-qx9m"}}); loc != "/api/hassio_ingress/testtoken/?m=" {
		t.Fatalf("pair redirect %q", loc)
	}
	v := h.app.View()
	if v.Phase != web.PhasePending {
		t.Fatalf("phase %s", v.Phase)
	}
	pubBefore, _ := os.ReadFile(filepath.Join(dir, "identity", "key"))
	if !strings.Contains(h.get("/"), v.CheckNumber) {
		t.Fatal("check number not on page")
	}

	// 2. Approve in "Spoken Parts".
	be.setAll("active")
	waitFor(t, "active with codes open", func() bool { v := h.app.View(); return v.Phase == web.PhaseActive && v.CodesReady })
	if page := h.get("/"); !strings.Contains(page, "Connected to Gilbert (Spoken Parts). Check number "+v.CheckNumber+".") {
		t.Fatalf("connected copy missing:\n%s", page)
	}
	waitFor(t, "first sync", func() bool { return be.lastSync() != nil })

	// 3. A printer announces itself.
	h.sendSSDP()
	waitFor(t, "printer found", func() bool { return len(h.app.View().Printers) == 1 })

	// 4. Type the access code on the page.
	if loc := h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}}); !strings.HasSuffix(loc, "?m=saved") {
		t.Fatalf("save redirect %q", loc)
	}
	waitFor(t, "printer report", func() bool {
		p := h.app.View().Printers[0]
		return p.Access == printers.AccessOK && p.GcodeState == "RUNNING"
	})

	// 5. Sync carries the printer status, never the code.
	waitFor(t, "sync with printer", func() bool {
		s := be.lastSync()
		return s != nil && len(s.Printers) == 1 && s.Printers[0].AccessCode == "ok" && s.Printers[0].GcodeState == "RUNNING"
	})
	sp := be.lastSync().Printers[0]
	if sp.Serial != serial || sp.ModelCode != "O1D" || sp.IP != "192.168.1.40" || *sp.Percent != 42 || *sp.RemainingMin != 70 ||
		sp.DevMode == nil || !*sp.DevMode || sp.LanOnly == nil || !*sp.LanOnly || !strings.HasPrefix(sp.CertFingerprint, "sha256:") || !sp.Online {
		t.Fatalf("sync printer %+v", sp)
	}

	// 6. Exactly one message went to the printer: the pushall request.
	pubs := broker.Publishes()
	if len(pubs) != 1 || pubs[0].Payload != mqtt.PushAllPayload || pubs[0].Topic != "device/"+serial+"/request" {
		t.Fatalf("printer publishes %+v", pubs)
	}

	// 7. The access code is nowhere it should not be.
	page := h.get("/")
	if strings.Contains(page, accessCode) {
		t.Fatal("access code shown on the page")
	}
	h.stop()
	assertNoLeak(t, dir, be, logs, accessCode, pairCode)
	assertNoLeakBytes(t, dir, logs, be.dataKey)
	if st, err := os.Stat(filepath.Join(dir, "secrets", "codes.enc")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("codes.enc: %v", err)
	}

	// 8. Restart: the same data key and local secret open the codes again.
	h = newHarness(t, dir, be, srv, broker, ca, logs)
	waitFor(t, "codes reopened", func() bool { return h.app.View().CodesReady })
	if !h.app.codes.Has(serial) {
		t.Fatal("code lost across restart")
	}
	h.sendSSDP()
	waitFor(t, "reconnected", func() bool {
		ps := h.app.View().Printers
		return len(ps) == 1 && ps[0].Access == printers.AccessOK && ps[0].Connected
	})

	// 9. Revoke in "Spoken Parts": the link wipes codes and key.
	be.setAll("revoked")
	waitFor(t, "unpaired", func() bool { return h.app.View().Phase == web.PhaseUnpaired })
	v = h.app.View()
	if v.Notice != web.MsgRemoved {
		t.Fatalf("notice %q", v.Notice)
	}
	if !strings.Contains(h.get("/"), "This link was removed in Spoken Parts.") {
		t.Fatal("removed copy missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "codes.enc")); !os.IsNotExist(err) {
		t.Fatal("codes.enc not wiped")
	}
	pubAfter, _ := os.ReadFile(filepath.Join(dir, "identity", "key"))
	if bytes.Equal(pubBefore, pubAfter) || len(pubAfter) != 32 {
		t.Fatal("identity key not replaced")
	}
	if v.Printers[0].Access != printers.AccessMissing {
		t.Fatalf("access after wipe: %s", v.Printers[0].Access)
	}
	h.stop()
	assertNoLeak(t, dir, be, logs, accessCode, pairCode)
}

func assertNoLeak(t *testing.T, dir string, be *backend, logs *lockedBuf, secrets ...string) {
	t.Helper()
	reqs := be.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, s := range secrets {
		lower := strings.ToLower(s)
		for _, r := range reqs {
			if s != pairCode || r.Path != "/api/link/v1/pair" { // the pairing code is meant for POST pair
				if strings.Contains(strings.ToLower(string(r.Body)), lower) {
					t.Errorf("%q in %s body", s, r.Path)
				}
			}
			for k, vs := range r.Header {
				for _, v := range vs {
					if strings.Contains(strings.ToLower(k+v), lower) {
						t.Errorf("%q in %s header %s", s, r.Path, k)
					}
				}
			}
			if strings.Contains(r.Path, s) {
				t.Errorf("%q in path", s)
			}
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("%q in logs", s)
		}
		walkFiles(t, dir, func(path string, data []byte) {
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("%q in plain text in %s", s, path)
			}
		})
	}
}

func assertNoLeakBytes(t *testing.T, dir string, logs *lockedBuf, raw []byte) {
	t.Helper()
	forms := []string{base64.StdEncoding.EncodeToString(raw), hex.EncodeToString(raw)}
	for _, f := range forms {
		if strings.Contains(logs.String(), f) {
			t.Error("data key in logs")
		}
	}
	walkFiles(t, dir, func(path string, data []byte) {
		if bytes.Contains(data, raw) {
			t.Errorf("raw data key in %s", path)
		}
		for _, f := range forms {
			if bytes.Contains(data, []byte(f)) {
				t.Errorf("encoded data key in %s", path)
			}
		}
	})
}

func walkFiles(t *testing.T, dir string, f func(string, []byte)) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := d.Info(); st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v is readable by others", p, st.Mode().Perm())
		}
		f(p, data)
		return nil
	})
}

func TestCheckNumberMismatchStops(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	be.badCheck = true
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	loc := h.post("/pair", url.Values{"code": {pairCode}})
	if !strings.HasSuffix(loc, "?m=mismatch") {
		t.Fatalf("redirect %q", loc)
	}
	if v := h.app.View(); v.Phase != web.PhaseUnpaired {
		t.Fatalf("phase %s", v.Phase)
	}
}

func TestBadCodeAndCodesOnlyWhenActive(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	if loc := h.post("/pair", url.Values{"code": {"ZZZZZZZZ"}}); !strings.HasSuffix(loc, "?m=bad_code") {
		t.Fatalf("redirect %q", loc)
	}
	h.sendSSDP()
	waitFor(t, "printer found", func() bool { return len(h.app.View().Printers) == 1 })
	if loc := h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}}); !strings.HasSuffix(loc, "?m=not_ready") {
		t.Fatalf("code accepted before pairing: %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "codes.enc")); !os.IsNotExist(err) {
		t.Fatal("codes file written before pairing")
	}
}

func TestPendingTimesOut(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) { c.PendingTimeout = 150 * time.Millisecond })
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	waitFor(t, "timeout", func() bool { v := h.app.View(); return v.Phase == web.PhaseUnpaired && v.Notice == web.MsgTimedOut })
	if !strings.Contains(h.get("/"), "This code timed out. Get a new code in Spoken Parts.") {
		t.Fatal("timeout copy missing")
	}
}

func TestLocalUnpairWipes(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(h.app.View().Printers) == 1 })
	h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}})
	waitFor(t, "ok", func() bool { return h.app.View().Printers[0].Access == printers.AccessOK })
	if loc := h.post("/remove", url.Values{"confirm": {"yes"}}); !strings.HasSuffix(loc, "?m=removed_local") {
		t.Fatalf("remove %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "codes.enc")); !os.IsNotExist(err) {
		t.Fatal("codes.enc not wiped")
	}
	if h.app.View().Phase != web.PhaseUnpaired {
		t.Fatal("still paired")
	}
}

func TestUnknownLinkConfirmedThenWipes(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) {
		c.UnknownConfirm = 150 * time.Millisecond
		c.UnknownRetry = 20 * time.Millisecond
	})
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(h.app.View().Printers) == 1 })
	h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}})
	waitFor(t, "ok", func() bool { return h.app.View().Printers[0].Access == printers.AccessOK })

	be.mu.Lock()
	be.unknown = true
	be.mu.Unlock()
	start := time.Now()
	waitFor(t, "unpaired", func() bool { return h.app.View().Phase == web.PhaseUnpaired })
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("a single unknown_link answer wiped the link")
	}
	if v := h.app.View(); v.Notice != web.MsgUnknownLink {
		t.Fatalf("notice %q", v.Notice)
	}
	if !strings.Contains(html.UnescapeString(h.get("/")), "This link isn't known to Spoken Parts any more. Get a new code and pair again.") {
		t.Fatal("unknown link copy missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "codes.enc")); !os.IsNotExist(err) {
		t.Fatal("codes.enc not wiped")
	}
	// It stops calling Spoken Parts.
	n := len(be.requests())
	time.Sleep(150 * time.Millisecond)
	if len(be.requests()) != n {
		t.Fatal("kept retrying after giving up")
	}
}

func TestUnknownLinkOnceDoesNotWipe(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) {
		c.UnknownConfirm = time.Hour
		c.UnknownRetry = 20 * time.Millisecond
	})
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	be.mu.Lock()
	be.unknown = true
	be.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	be.mu.Lock()
	be.unknown = false
	be.mu.Unlock()
	n := len(be.requests())
	waitFor(t, "sync again", func() bool { return len(be.requests()) > n+2 })
	if h.app.View().Phase != web.PhaseActive {
		t.Fatal("a short run of unknown_link wiped the link")
	}
}

func TestPairHonoursRetryAfter(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	be.throttlePair = true
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	for i := 0; i < 3; i++ {
		if loc := h.post("/pair", url.Values{"code": {pairCode}}); !strings.HasSuffix(loc, "?m=throttled") {
			t.Fatalf("redirect %q", loc)
		}
	}
	be.mu.Lock()
	calls := be.pairCalls
	be.mu.Unlock()
	if calls != 1 {
		t.Fatalf("pair called %d times during Retry-After", calls)
	}
}

func TestRemovePrinterDeletesItsCode(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(h.app.View().Printers) == 1 })
	h.post("/code", url.Values{"serial": {serial}, "code": {accessCode}})
	waitFor(t, "ok", func() bool { return h.app.View().Printers[0].Access == printers.AccessOK })
	if loc := h.post("/printer/remove", url.Values{"serial": {serial}}); !strings.HasSuffix(loc, "?m=printer_removed") {
		t.Fatalf("redirect %q", loc)
	}
	if h.app.codes.Has(serial) || len(h.app.View().Printers) != 0 {
		t.Fatal("printer or code still here")
	}
}

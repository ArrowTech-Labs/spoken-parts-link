package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/codebox"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/testutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

// codesAPI fakes the 0.4 sealed access code calls of Spoken Parts.
type codesAPI struct {
	mu       sync.Mutex
	keys     []string // registered public keys, in order
	schemes  []string
	envs     []json.RawMessage
	results  map[string][]api.CodeResultRequest
	fetches  int
	failNext map[string]int // id -> how many result posts answer 503
	keyGone  bool           // code-key answers 404 (older Spoken Parts)
	codeGone bool           // codes answers 404
}

func newCodesAPI(be *backend) *codesAPI {
	ca := &codesAPI{results: map[string][]api.CodeResultRequest{}, failNext: map[string]int{}}
	be.mu.Lock()
	be.extra = ca.serve
	be.mu.Unlock()
	return ca
}

func (ca *codesAPI) serve(w http.ResponseWriter, r *http.Request, body []byte, _ *fakeLink) bool {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/link/v1/code-key":
		if ca.keyGone {
			return false
		}
		var in struct {
			PublicKey string `json:"public_key"`
			Scheme    string `json:"scheme"`
		}
		_ = json.Unmarshal(body, &in)
		raw, err := base64.RawURLEncoding.DecodeString(in.PublicKey)
		if err != nil || len(raw) != 65 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":"invalid"}`))
			return true
		}
		ca.keys = append(ca.keys, in.PublicKey)
		ca.schemes = append(ca.schemes, in.Scheme)
		_, _ = fmt.Fprintf(w, `{"key_id":"%s"}`, codebox.KeyID(raw))
		return true
	case r.Method == "GET" && r.URL.Path == "/api/link/v1/codes":
		if ca.codeGone {
			return false
		}
		ca.fetches++
		list := ca.envs
		if list == nil {
			list = []json.RawMessage{}
		}
		b, _ := json.Marshal(map[string]any{"codes": list})
		_, _ = w.Write(b)
		return true
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/link/v1/codes/") && strings.HasSuffix(r.URL.Path, "/result"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/link/v1/codes/"), "/result")
		var in api.CodeResultRequest
		_ = json.Unmarshal(body, &in)
		ca.results[id] = append(ca.results[id], in)
		if ca.failNext[id] > 0 {
			ca.failNext[id]--
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code":"unavailable"}`))
			return true
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
		return true
	}
	return false
}

func (ca *codesAPI) lastKey() string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if len(ca.keys) == 0 {
		return ""
	}
	return ca.keys[len(ca.keys)-1]
}

func (ca *codesAPI) resultsFor(id string) []api.CodeResultRequest {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return append([]api.CodeResultRequest(nil), ca.results[id]...)
}

// add lists an envelope. id is raw JSON (a string or a number).
func (ca *codesAPI) add(t *testing.T, id string, serial string, env codebox.Envelope, scheme bool) {
	t.Helper()
	m := map[string]any{
		"id": json.RawMessage(id), "serial": serial, "key_id": env.KeyID, "epk": env.EPK,
		"nonce": env.Nonce, "ciphertext": env.Ciphertext, "expires_at": "2026-11-11T10:15:00Z",
	}
	if scheme {
		m["scheme"] = env.Scheme
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	ca.mu.Lock()
	ca.envs = append(ca.envs, b)
	ca.mu.Unlock()
}

func pathCount(be *backend, method, path string) int {
	n := 0
	for _, r := range be.requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func sealFor(t *testing.T, pubB64, serial, code string) codebox.Envelope {
	t.Helper()
	pub, err := base64.RawURLEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	env, err := codebox.Seal(pub, serial, []byte(code))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// TestSealedCodeEndToEnd: the link registers its code box key, fetches a
// code typed on spokenparts.com, opens it, saves it like the link page,
// reports the result once, and the code never reaches a log, a request or a
// plain file. Bad envelopes are refused with the right reason.
func TestSealedCodeEndToEnd(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	codes := newCodesAPI(be)
	h := newHarness(t, dir, be, srv, broker, ca, logs)

	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
	waitFor(t, "code key registered", func() bool { return codes.lastKey() != "" })
	pub := codes.lastKey()
	raw, _ := base64.RawURLEncoding.DecodeString(pub)
	keyID := codebox.KeyID(raw)
	if raw[0] != 4 || codes.schemes[0] != "sp-code-v1" || h.app.View().CodeKeyID != keyID {
		t.Fatalf("registered %q scheme %q view %q", pub, codes.schemes[0], h.app.View().CodeKeyID)
	}
	if !strings.Contains(h.get("/"), "Access code key "+keyID+".") {
		t.Fatal("key id not in the page footer")
	}
	h.sendSSDP()
	waitFor(t, "printer", func() bool { return len(h.app.View().Printers) == 1 })

	const badFormat = "Bad!c0de" // made up, 8 bytes but not letters and numbers
	const otherSerial = "01P00A000000009"
	good := sealFor(t, pub, serial, accessCode)
	codes.mu.Lock()
	codes.failNext["e-good"] = 1 // the first report is lost
	codes.mu.Unlock()
	codes.add(t, `"e-good"`, serial, good, false)
	codes.add(t, `"e-replay"`, serial, sealFor(t, pub, otherSerial, accessCode), true) // sealed for another printer
	codes.add(t, `41`, otherSerial, sealFor(t, pub, otherSerial, accessCode), true)    // a printer this link does not see
	codes.add(t, `"e-format"`, serial, sealFor(t, pub, serial, badFormat), true)
	wrongKey := good
	wrongKey.KeyID = "0123456789abcdef"
	codes.add(t, `"e-key"`, serial, wrongKey, true)
	wrongScheme := good
	wrongScheme.Scheme = "sp-code-v2"
	codes.add(t, `"e-scheme"`, serial, wrongScheme, true)
	codes.add(t, `"../x"`, serial, good, true) // a bad id is ignored

	waitFor(t, "every result", func() bool {
		for _, id := range []string{"e-replay", "41", "e-format", "e-key", "e-scheme"} {
			if len(codes.resultsFor(id)) == 0 {
				return false
			}
		}
		return len(codes.resultsFor("e-good")) == 2
	})
	want := map[string]api.CodeResultRequest{
		"e-good":   {Result: "saved"},
		"e-replay": {Result: "failed", Reason: "bad_envelope"},
		"41":       {Result: "failed", Reason: "unknown_printer"},
		"e-format": {Result: "failed", Reason: "format"},
		"e-key":    {Result: "failed", Reason: "bad_envelope"},
		"e-scheme": {Result: "failed", Reason: "bad_envelope"},
	}
	for id, w := range want {
		for _, got := range codes.resultsFor(id) {
			if got != w {
				t.Errorf("%s: result %+v, want %+v", id, got, w)
			}
		}
	}
	waitFor(t, "printer ok with the sealed code", func() bool {
		p := h.app.View().Printers[0]
		return p.Access == printers.AccessOK && p.GcodeState == "RUNNING"
	})
	if c, _ := h.app.codes.Get(serial); c != accessCode {
		t.Fatal("sealed code not saved")
	}

	// Spoken Parts keeps listing them: the link never opens or reports one
	// again.
	codes.mu.Lock()
	f := codes.fetches
	codes.mu.Unlock()
	waitFor(t, "more fetches", func() bool { codes.mu.Lock(); defer codes.mu.Unlock(); return codes.fetches > f+3 })
	for id := range want {
		n := len(codes.resultsFor(id))
		if (id == "e-good" && n != 2) || (id != "e-good" && n != 1) {
			t.Errorf("%s reported %d times", id, n)
		}
	}
	if n := strings.Count(logs.String(), "access code typed on spokenparts.com saved"); n != 1 {
		t.Fatalf("saved %d times", n)
	}
	if pathCount(be, "POST", "/api/link/v1/codes/../x/result") != 0 || len(codes.resultsFor("../x")) != 0 {
		t.Fatal("bad id reported")
	}
	// The envelope sealed to another key made the link register its key
	// once more, and only once.
	if n := pathCount(be, "POST", "/api/link/v1/code-key"); n != 2 {
		t.Fatalf("code key registered %d times", n)
	}

	// Restart: the same key, registered again. (Spoken Parts cleared the
	// envelopes when it got their results.)
	codes.mu.Lock()
	codes.envs = nil
	codes.mu.Unlock()
	h.app.mu.Lock()
	scalar := h.app.boxKey.Bytes()
	h.app.mu.Unlock()
	h.stop()
	assertNoLeak(t, dir, be, logs, accessCode, badFormat, pairCode)
	assertNoLeakBytes(t, dir, logs, scalar)
	if st, err := os.Stat(filepath.Join(dir, "secrets", "codebox.enc")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("codebox.enc: %v", err)
	}
	h = newHarness(t, dir, be, srv, broker, ca, logs)
	waitFor(t, "registered again", func() bool { codes.mu.Lock(); defer codes.mu.Unlock(); return len(codes.keys) == 3 })
	if codes.lastKey() != pub {
		t.Fatal("code box key changed across a restart")
	}

	// Unpair: the key goes with the access codes, and a new pairing gets a
	// new one.
	h.post("/remove", url.Values{"confirm": {"yes"}})
	if _, err := os.Stat(filepath.Join(dir, "secrets", "codebox.enc")); !os.IsNotExist(err) {
		t.Fatal("codebox.enc not wiped")
	}
	if h.app.View().CodeKeyID != "" || strings.Contains(h.get("/"), keyID) {
		t.Fatal("key id still shown after unpairing")
	}
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "registered after pairing again", func() bool { codes.mu.Lock(); defer codes.mu.Unlock(); return len(codes.keys) == 4 })
	if codes.lastKey() == pub {
		t.Fatal("the same code box key after unpairing")
	}
	h.stop()
	assertNoLeak(t, dir, be, logs, accessCode, badFormat, pairCode)
	assertNoLeakBytes(t, dir, logs, scalar)
}

// TestSealedCodesOlderBackend: an older Spoken Parts answers 404. The link
// keeps syncing, asks again only at the slow pace, and says nothing above
// debug about it.
func TestSealedCodesOlderBackend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keyGone    bool
		path       string
		wantFetchs bool
	}{
		{"no code-key", true, "/api/link/v1/code-key", false},
		{"no codes", false, "/api/link/v1/codes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, be, srv, broker, ca, logs := newWorld(t)
			codes := newCodesAPI(be)
			codes.keyGone, codes.codeGone = tc.keyGone, true
			h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) { c.CodeSlowRetry = time.Hour })
			defer h.stop()
			h.post("/pair", url.Values{"code": {pairCode}})
			be.setAll("active")
			waitFor(t, "ready", func() bool { return h.app.View().CodesReady })
			waitFor(t, "first 404", func() bool { return pathCount(be, http.MethodPost, "/api/link/v1/code-key") > 0 })
			be.mu.Lock()
			n := len(be.syncs)
			be.mu.Unlock()
			waitFor(t, "syncs go on", func() bool { be.mu.Lock(); defer be.mu.Unlock(); return len(be.syncs) > n+5 })
			if got := pathCount(be, http.MethodPost, "/api/link/v1/code-key"); got != 1 {
				t.Fatalf("code-key called %d times", got)
			}
			fetches := pathCount(be, http.MethodGet, "/api/link/v1/codes")
			if (tc.wantFetchs && fetches != 1) || (!tc.wantFetchs && fetches != 0) {
				t.Fatalf("codes called %d times", fetches)
			}
			if v := h.app.View(); v.Phase != web.PhaseActive || v.SyncProblem {
				t.Fatalf("view %+v", v)
			}
			quiet := 0
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, " WARN ") || strings.Contains(line, " ERROR ") || (strings.Contains(line, "could not") && !strings.Contains(line, " DEBUG ")) {
					t.Errorf("not quiet: %s", line)
				}
				if strings.Contains(line, " DEBUG Spoken Parts does not") {
					quiet++
				}
			}
			if quiet != 1 {
				t.Fatalf("%d debug lines about the older Spoken Parts", quiet)
			}
		})
	}
}

// TestSealedCodeSerialCase: Spoken Parts sends the serial upper cased. The
// envelope is opened with the serial exactly as sent (the AAD), and the code
// is saved under the serial the printer announced, here in lower case.
func TestSealedCodeSerialCase(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	codes := newCodesAPI(be)
	h := newHarness(t, dir, be, srv, broker, ca, logs)
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "code key registered", func() bool { return codes.lastKey() != "" })
	lower := strings.ToLower(serial)
	h.ssdp.Handle(netip.MustParseAddr("192.168.1.40"), testutil.SSDPPacket(lower, "192.168.1.40", "O1D", "H2D"))
	waitFor(t, "printer", func() bool { ps := h.app.View().Printers; return len(ps) == 1 && ps[0].Serial == lower })

	upper := strings.ToUpper(serial)
	pub := codes.lastKey()
	codes.add(t, `"e-upper"`, upper, sealFor(t, pub, upper, accessCode), true)
	codes.add(t, `"e-aad"`, upper, sealFor(t, pub, lower, accessCode), true) // AAD must match the serial as sent
	codes.add(t, `"e-none"`, "01P00A000000777", sealFor(t, pub, "01P00A000000777", accessCode), true)
	waitFor(t, "results", func() bool {
		return len(codes.resultsFor("e-upper")) == 1 && len(codes.resultsFor("e-aad")) == 1 && len(codes.resultsFor("e-none")) == 1
	})
	for id, w := range map[string]api.CodeResultRequest{
		"e-upper": {Result: "saved"},
		"e-aad":   {Result: "failed", Reason: "bad_envelope"},
		"e-none":  {Result: "failed", Reason: "unknown_printer"},
	} {
		if got := codes.resultsFor(id)[0]; got != w {
			t.Errorf("%s: %+v, want %+v", id, got, w)
		}
	}
	if c, ok := h.app.codes.Get(lower); !ok || c != accessCode {
		t.Fatal("code not saved under the printer's own serial")
	}
	if h.app.codes.Has(upper) {
		t.Fatal("code saved under the serial from Spoken Parts")
	}
}

// TestSealedCodeKeyRefresh: the key is registered again from time to time,
// in case Spoken Parts lost it.
func TestSealedCodeKeyRefresh(t *testing.T) {
	dir, be, srv, broker, ca, logs := newWorld(t)
	codes := newCodesAPI(be)
	h := newHarness(t, dir, be, srv, broker, ca, logs, func(c *Config) { c.CodeKeyRefresh = 100 * time.Millisecond })
	defer h.stop()
	h.post("/pair", url.Values{"code": {pairCode}})
	be.setAll("active")
	waitFor(t, "registered three times", func() bool { codes.mu.Lock(); defer codes.mu.Unlock(); return len(codes.keys) >= 3 })
	codes.mu.Lock()
	keys := append([]string(nil), codes.keys...)
	codes.mu.Unlock()
	for _, k := range keys {
		if k != keys[0] {
			t.Fatal("a different key was registered")
		}
	}
	if n := strings.Count(logs.String(), "can now reach this link"); n != 1 {
		t.Fatalf("info line %d times", n)
	}
}

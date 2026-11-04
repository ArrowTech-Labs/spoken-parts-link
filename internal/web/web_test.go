package web

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
)

type fakeCtrl struct {
	view     View
	pairCode string
	saved    map[string]string
	trusted  string
	unpaired bool
	removed  string
}

func (f *fakeCtrl) View() View { return f.view }
func (f *fakeCtrl) Pair(_ context.Context, code string) error {
	f.pairCode = code
	if code == "BADBADBA" {
		return MsgBadCode
	}
	return nil
}
func (f *fakeCtrl) CancelPairing() error { return nil }
func (f *fakeCtrl) SaveCode(serial, code string) error {
	f.saved[serial] = code
	return nil
}
func (f *fakeCtrl) Trust(serial, fp string) error { f.trusted = serial + " " + fp; return nil }
func (f *fakeCtrl) Unpair() error                 { f.unpaired = true; return nil }
func (f *fakeCtrl) RemovePrinter(serial string) error {
	f.removed = serial
	return nil
}

const ingress = "172.30.32.2"

func newServer(t *testing.T, v View) (*Server, *fakeCtrl, http.Handler, *redact.Redactor) {
	t.Helper()
	f := &fakeCtrl{view: v, saved: map[string]string{}}
	red := redact.New()
	s, err := New(f, Options{Allow: []netip.Addr{netip.MustParseAddr(ingress)}, Log: logx.New(io.Discard, red, logx.Debug), Red: red})
	if err != nil {
		t.Fatal(err)
	}
	return s, f, s.Handler(), red
}

func do(h http.Handler, method, path, from string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = from + ":40000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var activeView = View{
	Phase: PhaseActive, CheckNumber: "4821", SiteName: "Gilbert", CodesReady: true, LastSync: time.Now(), Version: "0.1.0",
	Printers: []printers.Snapshot{{Serial: "01P00A000000001", IP: "192.168.1.40", Model: "H2D", Name: "H2D", Access: printers.AccessOK, Cert: printers.CertTrusted}},
}

func TestOnlyIngressMayConnect(t *testing.T) {
	_, _, h, _ := newServer(t, activeView)
	for _, from := range []string{"192.168.1.20", "172.30.32.1", "127.0.0.1", "172.30.33.5"} {
		for _, p := range []string{"/", "/healthz", "/remove"} {
			if w := do(h, "GET", p, from, nil, nil); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", from, p, w.Code)
			}
		}
	}
	if w := do(h, "GET", "/healthz", ingress, nil, nil); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("healthz %d %q", w.Code, w.Body.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, _, h, _ := newServer(t, activeView)
	for _, from := range []string{ingress, "192.168.1.20"} {
		w := do(h, "GET", "/", from, nil, nil)
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'none'", "frame-ancestors 'self'", "style-src 'sha256-"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP %q lacks %q", csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Error("unsafe-inline in CSP")
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("headers %v", w.Header())
		}
	}
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	if strings.Contains(page, "<script") {
		t.Fatal("script tag in page")
	}
}

func csrf(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token in page")
	}
	return m[1]
}

func TestIngressPathInLinks(t *testing.T) {
	_, _, h, _ := newServer(t, activeView)
	hdr := map[string]string{"X-Ingress-Path": "/api/hassio_ingress/AbC123_-x"}
	page := do(h, "GET", "/", ingress, nil, hdr).Body.String()
	if !strings.Contains(page, `action="/api/hassio_ingress/AbC123_-x/code"`) || !strings.Contains(page, `href="/api/hassio_ingress/AbC123_-x/remove"`) {
		t.Fatalf("ingress path missing:\n%s", page)
	}
	evil := map[string]string{"X-Ingress-Path": `/x"><script>alert(1)</script>`}
	page = do(h, "GET", "/", ingress, nil, evil).Body.String()
	if strings.Contains(page, "<script>") || strings.Contains(page, "/x\"") {
		t.Fatal("bad ingress path used")
	}
	w := do(h, "POST", "/code", ingress, url.Values{"csrf": {csrf(t, page)}, "serial": {"01P00A000000001"}, "code": {"AbCd1234"}}, hdr)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/api/hassio_ingress/AbC123_-x/?m=saved" {
		t.Fatalf("redirect %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestCSRFRequired(t *testing.T) {
	_, f, h, _ := newServer(t, activeView)
	form := url.Values{"serial": {"01P00A000000001"}, "code": {"AbCd1234"}}
	w := do(h, "POST", "/code", ingress, form, nil)
	if w.Header().Get("Location") != "/?m=bad_form" || len(f.saved) != 0 {
		t.Fatalf("saved without csrf: %q", w.Header().Get("Location"))
	}
	form.Set("csrf", "AAAA")
	do(h, "POST", "/code", ingress, form, nil)
	if len(f.saved) != 0 {
		t.Fatal("saved with a forged token")
	}
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	form.Set("csrf", csrf(t, page))
	if w := do(h, "POST", "/code", ingress, form, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross site post: %d", w.Code)
	}
	do(h, "POST", "/code", ingress, form, nil)
	if f.saved["01P00A000000001"] != "AbCd1234" {
		t.Fatal("valid post not saved")
	}
}

func TestCSRFExpires(t *testing.T) {
	s, _, _, _ := newServer(t, activeView)
	now := time.Now()
	s.now = func() time.Time { return now }
	tok := s.csrfToken()
	if !s.checkCSRF(tok) {
		t.Fatal("fresh token refused")
	}
	now = now.Add(13 * time.Hour)
	if s.checkCSRF(tok) {
		t.Fatal("old token accepted")
	}
}

func TestAccessCodeValidationAndWriteOnly(t *testing.T) {
	_, f, h, red := newServer(t, activeView)
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	tok := csrf(t, page)
	for _, bad := range []string{"short", "toolong123", "abc-1234", "abcd 123", "ÄbCd1234"} {
		w := do(h, "POST", "/code", ingress, url.Values{"csrf": {tok}, "serial": {"01P00A000000001"}, "code": {bad}}, nil)
		if w.Header().Get("Location") != "/?m=access_format" {
			t.Errorf("%q: %s", bad, w.Header().Get("Location"))
		}
	}
	if len(f.saved) != 0 {
		t.Fatal("bad code saved")
	}
	do(h, "POST", "/code", ingress, url.Values{"csrf": {tok}, "serial": {"01P00A000000001"}, "code": {"AbCd1234"}}, nil)
	if red.Redact("AbCd1234") != redact.Mask {
		t.Fatal("entered code not registered for redaction")
	}
	page = do(h, "GET", "/?m=saved", ingress, nil, nil).Body.String()
	if strings.Contains(page, "AbCd1234") {
		t.Fatal("code echoed in page")
	}
	if !strings.Contains(page, "Saved. Checking the printer...") || !strings.Contains(page, "Access code (8 characters)") {
		t.Fatal("copy missing")
	}
	if regexp.MustCompile(`type="password"[^>]*value=`).MatchString(page) {
		t.Fatal("access code field has a value")
	}
}

func TestPairForm(t *testing.T) {
	_, f, h, red := newServer(t, View{Phase: PhaseUnpaired, Version: "0.1.0"})
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	if !strings.Contains(page, "Enter the code from Spoken Parts") || strings.Contains(page, `name="code" required minlength="8"`) {
		t.Fatal("unpaired page wrong")
	}
	tok := csrf(t, page)
	w := do(h, "POST", "/pair", ingress, url.Values{"csrf": {tok}, "code": {"k7p4-qx9m"}}, nil)
	if f.pairCode != "K7P4QX9M" || w.Header().Get("Location") != "/?m=" {
		t.Fatalf("pair %q %q", f.pairCode, w.Header().Get("Location"))
	}
	if red.Redact("k7p4-qx9m K7P4QX9M") != redact.Mask+" "+redact.Mask {
		t.Fatal("pairing code not redacted")
	}
	w = do(h, "POST", "/pair", ingress, url.Values{"csrf": {tok}, "code": {"BADBADBA"}}, nil)
	if w.Header().Get("Location") != "/?m=bad_code" {
		t.Fatal(w.Header().Get("Location"))
	}
	w = do(h, "POST", "/pair", ingress, url.Values{"csrf": {tok}, "code": {"<b>x"}}, nil)
	if w.Header().Get("Location") != "/?m=code_format" {
		t.Fatal(w.Header().Get("Location"))
	}
}

func TestPendingPageRefreshesAndShowsNumber(t *testing.T) {
	_, _, h, _ := newServer(t, View{Phase: PhasePending, CheckNumber: "0482"})
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	if !strings.Contains(page, `http-equiv="refresh"`) || !strings.Contains(page, "0482") || !strings.Contains(page, "Waiting for you to confirm in Spoken Parts.") {
		t.Fatal(page)
	}
}

func TestTrustAndRemove(t *testing.T) {
	v := activeView
	fp := "sha256:" + strings.Repeat("ab", 32)
	v.Printers = []printers.Snapshot{{Serial: "01P00A000000001", Cert: printers.CertUntrusted, Candidate: fp, CertFingerprint: fp}}
	_, f, h, _ := newServer(t, v)
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	if !strings.Contains(html.UnescapeString(page), "We couldn't check this printer's certificate.") || !strings.Contains(page, "Trust this printer") {
		t.Fatal("trust copy missing")
	}
	tok := csrf(t, page)
	do(h, "POST", "/trust", ingress, url.Values{"csrf": {tok}, "serial": {"01P00A000000001"}, "fingerprint": {fp}}, nil)
	if f.trusted != "01P00A000000001 "+fp {
		t.Fatalf("trusted %q", f.trusted)
	}
	confirm := do(h, "GET", "/remove", ingress, nil, nil).Body.String()
	if !strings.Contains(confirm, "Remove this link?") {
		t.Fatal("no confirm page")
	}
	do(h, "POST", "/remove", ingress, url.Values{"csrf": {tok}}, nil)
	if f.unpaired {
		t.Fatal("removed without confirm")
	}
	w := do(h, "POST", "/remove", ingress, url.Values{"csrf": {tok}, "confirm": {"yes"}}, nil)
	if !f.unpaired || w.Header().Get("Location") != "/?m=removed_local" {
		t.Fatal("remove failed")
	}
}

func TestUnknownMessageIgnored(t *testing.T) {
	_, _, h, _ := newServer(t, activeView)
	page := do(h, "GET", "/?m=%3Cscript%3E", ingress, nil, nil).Body.String()
	if strings.Contains(page, "script") {
		t.Fatal("message key echoed")
	}
}

func TestNormalizePairCode(t *testing.T) {
	if NormalizePairCode(" k7p4-qx9m ") != "K7P4QX9M" {
		t.Fatal(NormalizePairCode(" k7p4-qx9m "))
	}
}

func TestRejectedInputIsNotRegistered(t *testing.T) {
	_, _, h, red := newServer(t, View{Phase: PhaseUnpaired})
	tok := csrf(t, do(h, "GET", "/", ingress, nil, nil).Body.String())
	for _, junk := range []string{"printer", "serial_tail", "connected"} {
		do(h, "POST", "/pair", ingress, url.Values{"csrf": {tok}, "code": {junk}}, nil)
		do(h, "POST", "/code", ingress, url.Values{"csrf": {tok}, "serial": {"01P00A000000001"}, "code": {junk}}, nil)
	}
	if red.Len() != 0 {
		t.Fatalf("rejected input registered: %d values", red.Len())
	}
	if got := red.Redact("found printer serial_tail=0001"); got != "found printer serial_tail=0001" {
		t.Fatalf("log line changed: %q", got)
	}
}

func TestChangedCertificateNeverOffersTrust(t *testing.T) {
	v := activeView
	fp := "sha256:" + strings.Repeat("cd", 32)
	v.Printers = []printers.Snapshot{{Serial: "01P00A000000001", Cert: printers.CertChanged, CertFingerprint: fp, Known: true, Spoofed: true}}
	_, f, h, _ := newServer(t, v)
	page := html.UnescapeString(do(h, "GET", "/", ingress, nil, nil).Body.String())
	for _, want := range []string{
		"This printer's certificate changed and couldn't be checked. If you just replaced or reset the printer, remove it here and add it again.",
		"We did not send the access code.",
		"Remove this printer here",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Trust this printer") || strings.Contains(page, "trust it again") {
		t.Fatal("trust offered for a changed certificate")
	}
	// Even a candidate on a known printer must not show the button.
	v.Printers[0].Candidate = fp
	v.Printers[0].Cert = printers.CertUntrusted
	_, _, h2, _ := newServer(t, v)
	if strings.Contains(do(h2, "GET", "/", ingress, nil, nil).Body.String(), "Trust this printer") {
		t.Fatal("trust offered for a known printer")
	}
	tok := csrf(t, page)
	w := do(h, "POST", "/printer/remove", ingress, url.Values{"csrf": {tok}, "serial": {"01P00A000000001"}}, nil)
	if f.removed != "01P00A000000001" || w.Header().Get("Location") != "/?m=printer_removed" {
		t.Fatalf("remove printer: %q %q", f.removed, w.Header().Get("Location"))
	}
}

var autofocusRe = regexp.MustCompile(`<input[^>]*\sautofocus[^>]*>`)

func TestUnpairedPageLandsOnCodeEntry(t *testing.T) {
	v := View{Phase: PhaseUnpaired, Notice: MsgTimedOut, Version: "0.1.1"}
	v.Printers = activeView.Printers
	_, _, h, _ := newServer(t, v)
	page := do(h, "GET", "/", ingress, nil, nil).Body.String()
	form := strings.Index(page, `action="/pair"`)
	firstSection := strings.Index(page, "<section")
	pairSection := strings.Index(page, `<section aria-labelledby="pair-h">`)
	printersSection := strings.Index(page, `<section aria-labelledby="printers-h">`)
	if form < 0 || pairSection < 0 || printersSection < 0 {
		t.Fatalf("sections missing:\n%s", page)
	}
	if firstSection != pairSection || form > printersSection {
		t.Fatal("code entry is not the first section, above the printers")
	}
	if strings.Contains(page[:pairSection], "<form") || strings.Contains(page[:pairSection], "<input") {
		t.Fatal("another form comes before code entry")
	}
	inputs := autofocusRe.FindAllString(page, -1)
	if len(inputs) != 1 || !strings.Contains(inputs[0], `name="code"`) || !strings.Contains(inputs[0], `id="pair-code"`) || !strings.Contains(inputs[0], `placeholder="K7P4-QX9M"`) {
		t.Fatalf("want exactly the pairing code field autofocused, got %q", inputs)
	}
	text := html.UnescapeString(page)
	for _, want := range []string{"Connect to Spoken Parts", "Enter the code from Spoken Parts", "Get the code in Spoken Parts: your building's Network setup step.", ">Connect</button>"} {
		if !strings.Contains(text, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "<script") {
		t.Fatal("script tag in page")
	}
}

func TestPairedAndPendingPagesHaveNoAutofocus(t *testing.T) {
	for _, v := range []View{activeView, {Phase: PhasePending, CheckNumber: "0482", Printers: activeView.Printers}} {
		_, _, h, _ := newServer(t, v)
		page := do(h, "GET", "/", ingress, nil, nil).Body.String()
		if strings.Contains(page, "autofocus") {
			t.Errorf("%s page has an autofocused field", v.Phase)
		}
		if strings.Contains(page, `id="pair-code"`) || strings.Contains(page, `action="/pair"`) {
			t.Errorf("%s page shows the code entry form", v.Phase)
		}
	}
}

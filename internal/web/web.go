// Package web serves the link's page inside Home Assistant (ingress).
//
// Only the Supervisor's ingress proxy may connect: the server binds to the
// Supervisor interface and refuses any other source address. Every POST
// needs a CSRF token. Access code fields are write only: the page never
// shows a stored code.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/sends"
)

//go:embed templates/*.html
var templateFS embed.FS

// Phase of the link as shown on the page.
const (
	PhaseUnpaired = "unpaired"
	PhasePending  = "pending"
	PhaseActive   = "active"
)

// View is everything the page shows.
type View struct {
	Phase        string
	CheckNumber  string
	SiteName     string
	CompanyName  string
	Notice       Msg
	LastSync     time.Time
	SyncProblem  bool
	Paused       bool
	CodesReady   bool
	CodesDamaged bool
	Printers     []printers.Snapshot
	Version      string
	// 0.2: the active send per printer serial, and whether auto start is on.
	Sends     map[string]sends.Status
	AutoStart bool
}

// Controller is the app behind the page.
type Controller interface {
	View() View
	Pair(ctx context.Context, code string) error
	CancelPairing() error
	SaveCode(serial, code string) error
	Trust(serial, fingerprint string) error
	RemovePrinter(serial string) error
	Unpair() error
}

// Options for the server.
type Options struct {
	Allow []netip.Addr // source addresses allowed to connect
	Log   *logx.Logger
	Red   *redact.Redactor
	Now   func() time.Time
}

// Server is the ingress HTTP handler.
type Server struct {
	ctrl    Controller
	allow   map[netip.Addr]bool
	log     *logx.Logger
	red     *redact.Redactor
	now     func() time.Time
	secret  []byte
	tmpl    *template.Template
	cspHash string

	refusedMu   sync.Mutex
	refusedLast time.Time
	refusedN    int
}

// css is inlined in the page. Its hash goes in the CSP header.
const css = `:root{color-scheme:light dark;--fg:#1b1d21;--bg:#fff;--mut:#5b6170;--line:#d9dce3;--ok:#1d6b3a;--warn:#8a4b00;--acc:#1f5fbf}
@media (prefers-color-scheme:dark){:root{--fg:#e8eaee;--bg:#16181c;--mut:#a3a9b6;--line:#343842;--ok:#6fd39a;--warn:#ffbf69;--acc:#7fb0ff}}
*{box-sizing:border-box}body{margin:0;font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;color:var(--fg);background:var(--bg)}
main{max-width:44rem;margin:0 auto;padding:1rem}h1{font-size:1.4rem;margin:.2rem 0 1rem}h2{font-size:1.15rem;margin:1.6rem 0 .5rem}h3{font-size:1.05rem;margin:0}
p{margin:.4rem 0}.mut{color:var(--mut)}.ok{color:var(--ok)}.warn{color:var(--warn)}
.note{border:1px solid var(--line);border-left:4px solid var(--acc);padding:.6rem .8rem;margin:.8rem 0;border-radius:6px}
.note.warn{border-left-color:var(--warn);color:inherit}.note.ok{border-left-color:var(--ok);color:inherit}
.big{font-size:2.4rem;font-weight:700;letter-spacing:.2em;font-variant-numeric:tabular-nums}
.card{border:1px solid var(--line);border-radius:8px;padding:.8rem;margin:.8rem 0}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.2rem .8rem;margin:.6rem 0}dt{color:var(--mut)}dd{margin:0;overflow-wrap:anywhere}
form{margin:.6rem 0}label{display:block;font-weight:600;margin:.4rem 0 .2rem}
input[type=text],input[type=password]{font:inherit;width:100%;max-width:18rem;padding:.5rem;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg)}
button,.btn{font:inherit;display:inline-block;margin:.4rem .4rem 0 0;padding:.5rem 1rem;border-radius:6px;border:1px solid var(--acc);background:var(--acc);color:#fff;text-decoration:none;cursor:pointer}
.btn2{background:transparent;color:var(--acc)}.danger{border-color:#b3261e;background:#b3261e}
code{font-size:.85em;overflow-wrap:anywhere}footer{margin:2rem 0 1rem;color:var(--mut);font-size:.9rem}`

// New returns a Server.
func New(ctrl Controller, opts Options) (*Server, error) {
	s := &Server{
		ctrl:  ctrl,
		allow: map[netip.Addr]bool{},
		log:   opts.Log,
		red:   opts.Red,
		now:   opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	for _, a := range opts.Allow {
		s.allow[a.Unmap()] = true
	}
	s.secret = make([]byte, 32)
	if _, err := rand.Read(s.secret); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(css))
	s.cspHash = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	t, err := template.New("").Funcs(template.FuncMap{
		"ago":        s.ago,
		"gcode":      gcodeText,
		"accessText": accessText,
		"certText":   certText,
		"shortFP":    shortFP,
		"pct":        func(p *int) int { return *p },
		"canStart":   canStartText,
		"storage":    storageText,
		"sendFor": func(m map[string]sends.Status, serial string) *sends.Status {
			if st, ok := m[serial]; ok {
				return &st
			}
			return nil
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	return s, nil
}

// Handler returns the HTTP handler with all checks in front.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /remove", s.removeConfirm)
	mux.HandleFunc("POST /pair", s.post(s.pair))
	mux.HandleFunc("POST /pair/cancel", s.post(s.cancelPair))
	mux.HandleFunc("POST /code", s.post(s.saveCode))
	mux.HandleFunc("POST /trust", s.post(s.trust))
	mux.HandleFunc("POST /printer/remove", s.post(s.removePrinter))
	mux.HandleFunc("POST /remove", s.post(s.remove))
	return s.guard(mux)
}

// guard enforces the source allow list and sets security headers.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'none'; style-src "+s.cspHash+"; img-src 'self' data:; form-action 'self'; frame-ancestors 'self'; base-uri 'none'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Cache-Control", "no-store")
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !s.allow[ap.Addr().Unmap()] {
			s.logRefused(r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRefused logs refused sources at most once a minute.
func (s *Server) logRefused(from string) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	s.refusedN++
	if now := s.now(); now.Sub(s.refusedLast) >= time.Minute {
		s.log.Warn("refused web request from a source that is not the Home Assistant ingress proxy", "from", from, "refused_since_last_log", s.refusedN)
		s.refusedLast, s.refusedN = now, 0
	}
}

var ingressRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// base is the ingress path prefix for links and form actions.
func base(r *http.Request) string {
	p := strings.TrimRight(r.Header.Get("X-Ingress-Path"), "/")
	if len(p) > 200 || !ingressRe.MatchString(p) {
		return ""
	}
	return p
}

type page struct {
	View
	Base    string
	CSRF    string
	CSS     template.CSS
	Msg     Msg
	Refresh bool
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int) {
	v := s.ctrl.View()
	p := page{View: v, Base: base(r), CSRF: s.csrfToken(), CSS: template.CSS(css)}
	if m := Msg(r.URL.Query().Get("m")); m.Text() != "" {
		p.Msg = m
	}
	p.Refresh = name == "index.html" && v.Phase == PhasePending
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.log.Error("page render failed", "err", err)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "index.html", http.StatusOK)
}

func (s *Server) removeConfirm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "remove.html", http.StatusOK)
}

// post wraps a form handler: body limit, CSRF, then redirect with a message.
func (s *Server) post(h func(r *http.Request) Msg) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var m Msg
		if !s.checkCSRF(r.PostForm.Get("csrf")) {
			m = MsgBadForm
		} else {
			m = h(r)
		}
		http.Redirect(w, r, base(r)+"/?m="+string(m), http.StatusSeeOther)
	}
}

var (
	accessCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)
	pairCodeRe   = regexp.MustCompile(`^[0-9A-Z]{8}$`)
	serialRe     = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)
	fpRe         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// NormalizePairCode strips spaces and dashes and upper cases.
func NormalizePairCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func (s *Server) pair(r *http.Request) Msg {
	raw := strings.TrimSpace(r.PostForm.Get("code"))
	code := NormalizePairCode(raw)
	if !pairCodeRe.MatchString(code) {
		return MsgCodeFormat
	}
	// Registered only once valid, so junk input cannot redact ordinary
	// words or grow the list.
	s.red.AddTransient(code)
	s.red.AddTransient(raw)
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	return msgFor(s.ctrl.Pair(ctx, code), "")
}

func (s *Server) cancelPair(r *http.Request) Msg {
	return msgFor(s.ctrl.CancelPairing(), MsgCancelled)
}

func (s *Server) saveCode(r *http.Request) Msg {
	code := strings.TrimSpace(r.PostForm.Get("code"))
	serial := r.PostForm.Get("serial")
	if !serialRe.MatchString(serial) {
		return MsgUnknownPrinter
	}
	if !accessCodeRe.MatchString(code) {
		return MsgAccessFormat
	}
	s.red.Add(code)
	return msgFor(s.ctrl.SaveCode(serial, code), MsgSaved)
}

func (s *Server) trust(r *http.Request) Msg {
	serial := r.PostForm.Get("serial")
	fp := r.PostForm.Get("fingerprint")
	if !serialRe.MatchString(serial) || !fpRe.MatchString(fp) {
		return MsgNothingToTrust
	}
	return msgFor(s.ctrl.Trust(serial, fp), MsgTrusted)
}

func (s *Server) removePrinter(r *http.Request) Msg {
	serial := r.PostForm.Get("serial")
	if !serialRe.MatchString(serial) {
		return MsgUnknownPrinter
	}
	return msgFor(s.ctrl.RemovePrinter(serial), MsgPrinterRemoved)
}

func (s *Server) remove(r *http.Request) Msg {
	if r.PostForm.Get("confirm") != "yes" {
		return ""
	}
	return msgFor(s.ctrl.Unpair(), MsgRemovedLocal)
}

func msgFor(err error, ok Msg) Msg {
	if err == nil {
		return ok
	}
	var m Msg
	if errors.As(err, &m) {
		return m
	}
	return MsgError
}

// ---- CSRF ----
// A token is 8 bytes of unix time plus an HMAC of it with a per process
// secret. It is valid for 12 hours. The ingress page is only reachable by
// Home Assistant admins, so a per process token is enough.

const csrfMaxAge = 12 * time.Hour

func (s *Server) csrfToken() string {
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(s.now().Unix()))
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("csrf-v1"))
	mac.Write(ts[:])
	return base64.RawURLEncoding.EncodeToString(append(ts[:], mac.Sum(nil)...))
}

func (s *Server) checkCSRF(tok string) bool {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != 8+sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("csrf-v1"))
	mac.Write(b[:8])
	if !hmac.Equal(mac.Sum(nil), b[8:]) {
		return false
	}
	ts := time.Unix(int64(binary.BigEndian.Uint64(b[:8])), 0)
	age := s.now().Sub(ts)
	return age >= -time.Minute && age <= csrfMaxAge
}

// ---- template helpers ----

func (s *Server) ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := s.now().Sub(t)
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < 2*time.Minute:
		return "1 minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "1 hour ago"
	default:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
}

func gcodeText(s string) string {
	switch s {
	case "IDLE":
		return "Idle"
	case "RUNNING":
		return "Printing"
	case "PAUSE":
		return "Paused"
	case "FINISH":
		return "Finished"
	case "FAILED":
		return "Failed"
	case "PREPARE":
		return "Getting ready"
	case "SLICING":
		return "Slicing"
	default:
		return "Not known yet"
	}
}

func accessText(a printers.AccessState) string {
	switch a {
	case printers.AccessOK:
		return "Working"
	case printers.AccessWrong:
		return "The access code didn't work. Check it on the printer and type it again."
	case printers.AccessUnchecked:
		return "Saved. Checking the printer..."
	default:
		return "Not entered yet"
	}
}

func certText(c printers.CertState) string {
	switch c {
	case printers.CertTrusted:
		return "Checked"
	case printers.CertPinned:
		return "Trusted by you"
	case printers.CertUntrusted:
		return "We couldn't check this printer's certificate."
	case printers.CertChanged:
		return "This printer's certificate changed and couldn't be checked. If you just replaced or reset the printer, remove it here and add it again."
	case printers.CertClock:
		return "We couldn't check this printer's certificate yet. This device's clock may be wrong. We will try again."
	default:
		return "Not checked yet"
	}
}

func shortFP(fp string) string {
	fp = strings.TrimPrefix(fp, "sha256:")
	var parts []string
	for i := 0; i+4 <= len(fp) && i < 32; i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	return strings.Join(parts, " ")
}

func canStartText(p printers.Snapshot, autoStart bool) string {
	switch p.CanStart(autoStart) {
	case "auto":
		return "Yes. Developer Mode is on, so the link starts prints itself."
	case "upload_only":
		return "No, upload only. Someone taps the file on the printer screen (Cloud mode)."
	default:
		return "Not known yet"
	}
}

func storageText(b *bool) string {
	switch {
	case b == nil:
		return "Not known yet"
	case *b:
		return "USB stick or SD card in"
	default:
		return "No USB stick. Insert one so prints can be uploaded."
	}
}

// Package labels prints Spoken Parts label jobs on label printers through
// Home Assistant (0.5, LABEL-1). The printers are set up in Home Assistant's
// Spoken Parts Printers integration, which talks to them over Bluetooth; the
// link only relays.
//
// While the link is active and not paused:
//
//   - At start, every 60 s and right after each job it asks Home Assistant
//     for its printers (list_printers) and reports them to Spoken Parts. When
//     Home Assistant has no integration or cannot be reached it still reports,
//     with integration.present=false and a short error code, so Spoken Parts
//     can say "install the integration".
//   - It polls Spoken Parts for queued jobs every 4 s while Home Assistant has
//     at least one printer, else every 60 s.
//   - For each job, one at a time: claim it, report "printing", hand exactly
//     the claimed fields to Home Assistant (print_raster), then report
//     "printed" (only on ok: true) or "failed" with a code.
//
// A job handed to Home Assistant is never handed again, whatever happens
// next (no double prints). Only the state reports are retried. The pages
// are opaque: the link never decodes, stores or logs them. Nothing here
// talks to a 3D printer.
package labels

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/haapi"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
)

// API is the part of the Spoken Parts client the runner uses (signed calls).
type API interface {
	ReportLabelPrinters(ctx context.Context, r api.LabelReport) error
	ListLabelJobs(ctx context.Context) ([]api.LabelJobRef, error)
	ClaimLabelJob(ctx context.Context, id string) (*api.LabelJob, error)
	LabelJobState(ctx context.Context, id string, req api.LabelStateRequest) error
}

// HA calls a Home Assistant service and returns its service_response
// (internal/haapi).
type HA interface {
	CallService(ctx context.Context, domain, service string, data any) (json.RawMessage, error)
}

// Config for New. Zero durations get production defaults.
type Config struct {
	API          API
	HA           HA
	Log          *logx.Logger
	Now          func() time.Time
	ReportEvery  time.Duration // 60 s
	PollFast     time.Duration // 4 s while Home Assistant has a printer
	PollSlow     time.Duration // 60 s otherwise
	ListTimeout  time.Duration // 10 s for list_printers
	PrintTimeout time.Duration // 75 s for print_raster (it waits up to 60 s)
	StateTries   int           // 4 tries per state report
	StateBackoff time.Duration // 1 s, doubled per try
	CallTimeout  time.Duration // 30 s per Spoken Parts call
	// The "printing" report is tried once, this long; the job goes to Home
	// Assistant only if Spoken Parts accepted it within PrintWindow of the
	// claim. Otherwise nothing is printed (never a late label).
	PrintingTimeout time.Duration // 10 s
	PrintWindow     time.Duration // 20 s
}

// Failure codes of the contract.
var failCodes = map[string]bool{
	"offline": true, "busy": true, "no_media": true, "cover_open": true, "bad_format": true,
	"too_big": true, "write_failed": true, "timeout": true, "no_integration": true,
	"unknown_printer": true, "printer_error": true, "wrong_media": true, "too_late": true,
	"link_paused": true,
}

// Printer states of the contract.
var printerStates = map[string]bool{"ready": true, "printing": true, "offline": true, "error": true, "waiting": true}

// PrinterStatus is one printer as the link page shows it.
type PrinterStatus struct {
	Name      string
	State     string
	LastError string
	Battery   *int
	Media     string
}

// JobResult is how the last job went (no payload).
type JobResult struct {
	At      time.Time
	Printer string // the printer's name, or its id when unknown
	State   string // printed, failed
	Code    string
	Message string
}

// Status is what the link page shows. It holds no payload.
type Status struct {
	Checked  bool // Home Assistant was asked at least once
	Present  bool
	Version  string
	Error    string // no_token, not_installed, ha_unreachable, http_<n>, ...
	Printers []PrinterStatus
	LastJob  *JobResult
}

// Runner relays label jobs. Run it once; turn it on with SetEnabled.
type Runner struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	enabled  bool
	paused   bool
	gen      uint64
	status   Status
	names    map[string]string    // printer id -> name
	present  bool                 // the last list said the integration is there
	handled  map[string]time.Time // jobs claimed here: never claimed again
	printers int
}

var (
	codeRe      = regexp.MustCompile(`^[a-z0-9_]{1,30}$`)
	versionRe   = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,20}$`)
	printerIDRe = regexp.MustCompile(`^[\x21-\x7e]{1,80}$`)
)

// New returns a Runner.
func New(cfg Config) *Runner {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.ReportEvery, 60*time.Second)
	def(&cfg.PrintingTimeout, 10*time.Second)
	def(&cfg.PrintWindow, 20*time.Second)
	def(&cfg.PollFast, 4*time.Second)
	def(&cfg.PollSlow, 60*time.Second)
	def(&cfg.ListTimeout, 10*time.Second)
	def(&cfg.PrintTimeout, 75*time.Second)
	def(&cfg.StateBackoff, time.Second)
	def(&cfg.CallTimeout, 30*time.Second)
	if cfg.StateTries <= 0 {
		cfg.StateTries = 4
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, wake: make(chan struct{}, 1), names: map[string]string{}, handled: map[string]time.Time{}}
}

// SetEnabled turns the runner on once the link is active (off when it is
// not paired, pending or removed).
func (r *Runner) SetEnabled(on bool) {
	r.mu.Lock()
	changed := r.enabled != on
	r.enabled = on
	r.mu.Unlock()
	if changed {
		r.kick()
	}
}

// SetPaused follows the sync answer: a paused link does nothing here.
func (r *Runner) SetPaused(p bool) {
	r.mu.Lock()
	changed := r.paused != p
	r.paused = p
	r.mu.Unlock()
	if changed {
		r.kick()
	}
}

// Reset forgets everything (the link was removed or unpaired).
func (r *Runner) Reset() {
	r.mu.Lock()
	r.gen++
	r.enabled = false
	r.paused = false
	r.status = Status{}
	r.names = map[string]string{}
	r.present = false
	r.printers = 0
	r.mu.Unlock()
	r.kick()
}

func (r *Runner) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Status returns a copy of the page status.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.Printers = append([]PrinterStatus(nil), r.status.Printers...)
	if r.status.LastJob != nil {
		j := *r.status.LastJob
		st.LastJob = &j
	}
	return st
}

// on reports whether the runner may act now, and the current generation.
func (r *Runner) on() (bool, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled && !r.paused, r.gen
}

func (r *Runner) current(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled && !r.paused && r.gen == gen
}

// Run works until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	var nextReport, nextPoll time.Time
	for ctx.Err() == nil {
		wait := time.Hour
		if on, gen := r.on(); on {
			if !r.cfg.Now().Before(nextReport) {
				r.report(ctx, gen)
				nextReport = r.cfg.Now().Add(r.cfg.ReportEvery)
			}
			if !r.cfg.Now().Before(nextPoll) {
				if r.poll(ctx, gen) {
					r.report(ctx, gen)
					nextReport = r.cfg.Now().Add(r.cfg.ReportEvery)
				}
				nextPoll = r.cfg.Now().Add(r.pollEvery())
			}
			now := r.cfg.Now()
			wait = max(min(nextReport.Sub(now), nextPoll.Sub(now)), time.Millisecond)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
			// Turned on, resumed or reset: report and poll right away.
			nextReport, nextPoll = time.Time{}, time.Time{}
		case <-t.C:
		}
	}
}

func (r *Runner) pollEvery() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.printers > 0 {
		return r.cfg.PollFast
	}
	return r.cfg.PollSlow
}

// report asks Home Assistant for its printers and tells Spoken Parts.
func (r *Runner) report(ctx context.Context, gen uint64) {
	rep := r.listPrinters(ctx)
	if !r.current(gen) {
		return
	}
	r.mu.Lock()
	prev := r.status
	r.present = rep.Integration.Present
	r.printers = len(rep.Printers)
	r.names = map[string]string{}
	ps := make([]PrinterStatus, 0, len(rep.Printers))
	for _, p := range rep.Printers {
		r.names[p.ID] = p.Name
		ps = append(ps, PrinterStatus{Name: p.Name, State: p.State, LastError: p.LastError, Battery: p.Battery, Media: p.Media})
	}
	r.status = Status{Checked: true, Present: rep.Integration.Present, Version: rep.Integration.Version, Error: rep.Integration.Error, Printers: ps, LastJob: prev.LastJob}
	r.mu.Unlock()
	if !prev.Checked || prev.Present != rep.Integration.Present || prev.Error != rep.Integration.Error {
		if rep.Integration.Present {
			r.cfg.Log.Info("label printers through Home Assistant", "integration", rep.Integration.Version, "printers", len(rep.Printers))
		} else {
			r.cfg.Log.Info("no label printers through Home Assistant", "reason", rep.Integration.Error)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	if err := r.cfg.API.ReportLabelPrinters(cctx, rep); err != nil {
		r.cfg.Log.Debug("could not report label printers", "err", err)
	}
}

// listPrinters builds the report from Home Assistant's answer.
func (r *Runner) listPrinters(ctx context.Context) api.LabelReport {
	rep := api.LabelReport{Printers: []api.LabelPrinter{}}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.ListTimeout)
	defer cancel()
	raw, err := r.cfg.HA.CallService(cctx, haapi.Domain, "list_printers", nil)
	if err != nil {
		code := haapi.Code(err)
		if code == "timeout" {
			code = "ha_unreachable"
		}
		rep.Integration.Error = code
		r.cfg.Log.Debug("list_printers failed", "code", code)
		return rep
	}
	var ans struct {
		Version  string             `json:"version"`
		Printers []api.LabelPrinter `json:"printers"`
	}
	if json.Unmarshal(raw, &ans) != nil {
		rep.Integration.Error = "bad_answer"
		return rep
	}
	rep.Integration.Present = true
	if versionRe.MatchString(ans.Version) {
		rep.Integration.Version = ans.Version
	}
	seen := map[string]bool{}
	for _, p := range ans.Printers {
		if len(rep.Printers) >= 50 {
			break
		}
		if !printerIDRe.MatchString(p.ID) || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		rep.Printers = append(rep.Printers, clean(p))
	}
	return rep
}

// clean keeps a printer object inside the backend's field sizes and states.
func clean(p api.LabelPrinter) api.LabelPrinter {
	p.Name = api.Truncate(p.Name, 60)
	if p.Name == "" {
		p.Name = api.Truncate(p.ID, 60)
	}
	p.Driver = api.Truncate(p.Driver, 40)
	p.Transport = api.Truncate(p.Transport, 10)
	p.Address = api.Truncate(p.Address, 17)
	p.Media = api.Truncate(p.Media, 20)
	p.LastError = api.Truncate(p.LastError, 200)
	p.LastSeen = api.Truncate(p.LastSeen, 40)
	if !printerStates[p.State] {
		if p.LastError == "" {
			p.LastError = api.Truncate("unknown state "+p.State, 200)
		}
		p.State = "error"
	}
	if p.Battery != nil && (*p.Battery < 0 || *p.Battery > 100) {
		p.Battery = nil
	}
	return p
}

// poll lists queued jobs and runs them one at a time. It returns true when
// a job was handed to Home Assistant or failed here.
func (r *Runner) poll(ctx context.Context, gen uint64) bool {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	jobs, err := r.cfg.API.ListLabelJobs(cctx)
	cancel()
	if err != nil {
		r.cfg.Log.Debug("could not list label jobs", "err", err)
		return false
	}
	r.forgetOld()
	did := false
	for _, j := range jobs {
		if ctx.Err() != nil || !r.current(gen) {
			break
		}
		r.mu.Lock()
		_, seen := r.handled[j.ID]
		r.mu.Unlock()
		if seen {
			continue
		}
		if r.run(ctx, gen, j.ID) {
			did = true
		}
	}
	return did
}

// forgetOld drops handled ids after 15 minutes (queued jobs expire after 3).
func (r *Runner) forgetOld() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, at := range r.handled {
		if r.cfg.Now().Sub(at) > 15*time.Minute {
			delete(r.handled, id)
		}
	}
}

// printCall is exactly the claimed fields print_raster takes.
type printCall struct {
	PrinterID    string          `json:"printer_id"`
	JobID        string          `json:"job_id"`
	Driver       string          `json:"driver"`
	DocumentType string          `json:"document_type"`
	Media        json.RawMessage `json:"media,omitempty"`  // omitted, not null, when the job has none
	Copies       json.RawMessage `json:"copies,omitempty"` // Home Assistant then uses 1
	Pages        json.RawMessage `json:"pages"`
}

// printAnswer is print_raster's service response.
type printAnswer struct {
	OK      *bool  `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Bytes   int    `json:"bytes"`
}

// run claims and prints one job. It returns true when the job was claimed.
func (r *Runner) run(ctx context.Context, gen uint64, id string) bool {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	job, err := r.cfg.API.ClaimLabelJob(cctx, id)
	cancel()
	switch {
	case api.HasCode(err, api.CodeNotQueued):
		r.cfg.Log.Debug("label job no longer queued, skipped", "job", id[:8])
		return false
	case err != nil:
		r.cfg.Log.Info("could not claim a label job", "job", id[:8], "err", err)
		return false
	}
	// From here the job is this link's: never claimed or printed again.
	r.mu.Lock()
	r.handled[id] = r.cfg.Now()
	name := r.names[job.PrinterID]
	present := r.present
	r.mu.Unlock()
	if name == "" {
		name = api.Truncate(job.PrinterID, 60)
	}

	if job.Format != "raster" || job.PrinterID == "" {
		// Only print_raster exists. Nothing went to Home Assistant.
		r.finish(ctx, gen, id, name, api.LabelStateRequest{State: "failed", Code: "bad_format", Message: "The link can only print raster labels."})
		return true
	}
	// Say "printing" once, quickly. Only if Spoken Parts accepts it, soon after
	// the claim, does the job go to Home Assistant: a job the page has already
	// given up on is never printed late.
	claimed := r.cfg.Now()
	sctx, scancel := context.WithTimeout(ctx, r.cfg.PrintingTimeout)
	err = r.cfg.API.LabelJobState(sctx, id, api.LabelStateRequest{State: "printing"})
	scancel()
	if err != nil || r.cfg.Now().Sub(claimed) > r.cfg.PrintWindow {
		r.cfg.Log.Info("label job not confirmed in time, not printed", "job", id[:8], "err", err)
		r.finish(ctx, gen, id, name, api.LabelStateRequest{State: "failed", Code: "too_late", Message: "Spoken Parts did not confirm it in time. Nothing was sent to the printer."})
		return true
	}
	if !r.current(gen) {
		// Paused or removed between the claim and the print: do not print.
		r.finish(ctx, gen, id, name, api.LabelStateRequest{State: "failed", Code: "link_paused", Message: "The link was paused before printing. Nothing was printed."})
		return true
	}

	pctx, pcancel := context.WithTimeout(ctx, r.cfg.PrintTimeout)
	raw, err := r.cfg.HA.CallService(pctx, haapi.Domain, "print_raster", printCall{
		PrinterID:    job.PrinterID,
		JobID:        job.ID,
		Driver:       job.Driver,
		DocumentType: job.DocumentType,
		Media:        job.Media,
		Copies:       job.Copies,
		Pages:        job.Pages,
	})
	pcancel()
	job.Pages = nil // the payload is not kept
	r.finish(ctx, gen, id, name, result(raw, err, present))
	return true
}

// result turns Home Assistant's answer into the final job state.
func result(raw json.RawMessage, err error, present bool) api.LabelStateRequest {
	fail := func(code, msg string) api.LabelStateRequest {
		return api.LabelStateRequest{State: "failed", Code: code, Message: msg}
	}
	if err != nil {
		var he *haapi.Error
		switch {
		case errors.Is(err, haapi.ErrTimeout), errors.Is(err, haapi.ErrUnreachable):
			return fail("timeout", "Home Assistant did not answer. The label may or may not have printed.")
		case errors.Is(err, haapi.ErrNotInstalled) && present:
			// The integration answered list_printers a moment ago: a bare 400
			// now means it refused the call itself.
			return fail("bad_format", "Home Assistant refused the print call.")
		case errors.Is(err, haapi.ErrNotInstalled), errors.Is(err, haapi.ErrNoToken):
			return fail("no_integration", "The Spoken Parts Printers integration is not installed in Home Assistant.")
		case errors.As(err, &he):
			msg := "Home Assistant answered " + strconv.Itoa(he.Status) + "."
			if he.Message != "" {
				msg = "Home Assistant: " + he.Message
			}
			return fail("printer_error", msg)
		}
		return fail("printer_error", "Home Assistant gave no usable answer.")
	}
	var ans printAnswer
	if json.Unmarshal(raw, &ans) != nil || ans.OK == nil {
		return fail("printer_error", "Home Assistant gave no usable answer.")
	}
	if *ans.OK {
		return api.LabelStateRequest{State: "printed", Bytes: max(ans.Bytes, 0)}
	}
	code := ans.Code
	if !codeRe.MatchString(code) || !failCodes[code] {
		code = "printer_error"
	}
	return fail(code, ans.Message)
}

// finish reports the final state and keeps it for the page.
func (r *Runner) finish(ctx context.Context, gen uint64, id, printer string, req api.LabelStateRequest) {
	req.Message = api.Truncate(req.Message, api.MaxLabelMessage)
	if req.State == "printed" {
		r.cfg.Log.Info("label printed", "job", id[:8], "bytes", req.Bytes)
	} else {
		r.cfg.Log.Warn("label job failed", "job", id[:8], "code", req.Code)
	}
	r.mu.Lock()
	if r.gen == gen {
		r.status.LastJob = &JobResult{At: r.cfg.Now(), Printer: printer, State: req.State, Code: req.Code, Message: req.Message}
	}
	r.mu.Unlock()
	r.state(ctx, id, req)
}

// state reports a job state, trying a few times: it is only a report, so
// trying again is safe. The print itself is never repeated.
func (r *Runner) state(ctx context.Context, id string, req api.LabelStateRequest) {
	wait := r.cfg.StateBackoff
	for try := 1; ; try++ {
		cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
		err := r.cfg.API.LabelJobState(cctx, id, req)
		cancel()
		var ae *api.Error
		if err == nil || (errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 429) {
			if err != nil {
				r.cfg.Log.Info("Spoken Parts refused a label job state", "job", id[:8], "state", req.State, "err", err)
			}
			return
		}
		if try >= r.cfg.StateTries || ctx.Err() != nil {
			r.cfg.Log.Warn("could not report a label job state", "job", id[:8], "state", req.State, "err", err)
			return
		}
		t := time.NewTimer(min(max(wait, api.RetryAfter(err)), 30*time.Second))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait *= 2
	}
}

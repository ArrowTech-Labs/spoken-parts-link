// Package sends delivers the prints Spoken Parts sends to this link's
// printers, and reports how they go.
//
// For each send: download the file once (one-time token), check its sha256
// and that it is a complete sliced plate, check the printer (the right one,
// storage present, not busy, no serious error), upload it over FTPS, and say
// "uploaded". In Cloud mode a person then taps the file on the printer
// screen; the link sees the print start (its file name in the report) and
// reports the send delivered, then its progress, until it finishes. Then it
// deletes the file it uploaded. A different file starting is reported as the
// wrong file; a wrong AMS slot after the start as an alert. The link cannot
// pause or stop a print.
//
// Auto start (project_file) runs only when Spoken Parts allows it for the
// printer, the link's own option is on (SP_LINK_AUTOSTART, off by default)
// and the printer reports Developer Mode. It is sent at most once per send,
// recorded before it is sent, so a restart never starts a print twice.
//
// State lives in /data/state/sends.json (no secrets) and files in
// /data/spool (0600, deleted after the print or a failure).
package sends

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/mqtt"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/threemf"
)

// API is the part of the Spoken Parts client the runner uses (signed calls).
type API interface {
	ListSends(ctx context.Context) ([]api.Send, error)
	Download(ctx context.Context, s api.Send, w io.Writer) (n int64, sum, header string, err error)
	SendState(ctx context.Context, id string, req api.StateRequest) error
	Events(ctx context.Context, serial string, events []api.Event) error
}

// Printers is the part of the printer manager the runner uses.
type Printers interface {
	Get(serial string) (printers.Snapshot, bool)
	UploadClean(ctx context.Context, serial, name string, r io.Reader, size int64, progress func(int64), removable func(name string, size int64) bool) ([]string, error)
	Delete(ctx context.Context, serial, name string) error
	StartProjectFile(serial string, pf mqtt.ProjectFile) error
	AutoStart() bool
}

// Config for New. Zero durations get production defaults.
type Config struct {
	SpoolDir  string
	StatePath string
	// ManifestPath lists, per printer, the files the link put on its storage.
	// Only files in it are ever deleted (default: storage.json next to StatePath).
	ManifestPath string
	API          API
	Printers     Printers
	Log          *logx.Logger
	Now          func() time.Time
	PollBusy     time.Duration // 15 s while a send is open
	PollIdle     time.Duration // 60 s
	Tick         time.Duration // 2 s
	EventEvery   time.Duration // 30 s between progress events
	MaxAge       time.Duration // 30 min: a send older than this is not started
}

// Phases of a record.
const (
	phaseDownloaded = "downloaded"
	phaseUploaded   = "uploaded"
	phaseStarted    = "started"
)

// record is what the link keeps about one send. It holds no secrets.
type record struct {
	ID        string          `json:"id"`
	Serial    string          `json:"serial"`
	Printer   string          `json:"printer"`
	FileName  string          `json:"file_name"`
	Subtask   string          `json:"subtask"`
	SHA256    string          `json:"sha256"`
	MD5       string          `json:"md5,omitempty"`
	Plate     int             `json:"plate"`
	Mapping   []api.MapRow    `json:"mapping,omitempty"`
	AutoStart bool            `json:"autostart,omitempty"`
	Options   api.SendOptions `json:"options"`
	Phase     string          `json:"phase"`
	// StartSent: a project_file went out for this send. Never again.
	StartSent    bool        `json:"start_sent,omitempty"`
	AutoRefused  bool        `json:"auto_refused,omitempty"`
	WaitingRetry bool        `json:"waiting_retry,omitempty"`
	WrongFile    string      `json:"wrong_file,omitempty"`
	Alerted      bool        `json:"alerted,omitempty"`
	LastError    string      `json:"last_error,omitempty"`
	UploadedAt   time.Time   `json:"uploaded_at,omitzero"`
	StartedAt    time.Time   `json:"started_at,omitzero"`
	Seq          int         `json:"seq"`
	LastState    string      `json:"last_state,omitempty"`
	LastPercent  int         `json:"last_percent"`
	LastLayer    int         `json:"last_layer"`
	LastEventAt  time.Time   `json:"last_event_at,omitzero"`
	Pending      []api.Event `json:"pending,omitempty"`
}

// Status is what the link page shows for a printer's active send.
type Status struct {
	FileName  string
	Text      string
	LastError string
}

// Runner delivers sends. Run it once the link is active and codes are open.
type Runner struct {
	cfg  Config
	wake chan struct{}
	wg   sync.WaitGroup // deliveries in flight

	mu      sync.Mutex
	recs    map[string]*record
	files   map[string][]ManifestEntry // serial -> files the link uploaded there
	busy    map[string]bool            // sends being downloaded or uploaded now
	errs    map[string]string
	gen     uint64
	enabled bool
}

var (
	fileNameRe = printers.FileNameRe
	idRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// New loads saved records.
func New(cfg Config) (*Runner, error) {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.PollBusy, 15*time.Second)
	def(&cfg.PollIdle, 60*time.Second)
	def(&cfg.Tick, 2*time.Second)
	def(&cfg.EventEvery, 30*time.Second)
	def(&cfg.MaxAge, 30*time.Minute)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := fsutil.EnsureDir(cfg.SpoolDir); err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, wake: make(chan struct{}, 1), recs: map[string]*record{}, busy: map[string]bool{}, errs: map[string]string{}}
	var saved []*record
	if err := store.LoadJSON(cfg.StatePath, &saved); err != nil {
		cfg.Log.Warn("could not read saved sends, starting fresh", "err", err)
	}
	for _, rec := range saved {
		if rec != nil && idRe.MatchString(rec.ID) && fileNameRe.MatchString(rec.FileName) {
			r.recs[rec.ID] = rec
		}
	}
	if r.cfg.ManifestPath == "" {
		r.cfg.ManifestPath = filepath.Join(filepath.Dir(cfg.StatePath), "storage.json")
	}
	r.files = map[string][]ManifestEntry{}
	if err := store.LoadJSON(r.cfg.ManifestPath, &r.files); err != nil {
		cfg.Log.Warn("could not read the storage manifest; no earlier file will be deleted", "err", err)
		r.files = map[string][]ManifestEntry{}
	}
	return r, nil
}

// ManifestEntry is one file the link uploaded to a printer. It holds no secrets.
type ManifestEntry struct {
	FileName   string    `json:"file_name"`
	SendID     string    `json:"send_id"`
	Size       int64     `json:"size"`
	MD5        string    `json:"md5,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

func (r *Runner) saveManifestLocked() {
	if err := store.SaveJSON(r.cfg.ManifestPath, r.files); err != nil {
		r.cfg.Log.Error("could not save the storage manifest", "err", err)
	}
}

// removable says whether an earlier file on the printer may be deleted: a
// reserved name the link uploaded itself (in the manifest), the size it
// recorded, and not the file of a print that is running now.
func (r *Runner) removable(serial string, snap printers.Snapshot) func(string, int64) bool {
	return func(name string, size int64) bool {
		if !fileNameRe.MatchString(name) {
			return false
		}
		if snap.Busy() && matches(&record{Subtask: strings.TrimSuffix(name, ".gcode.3mf")}, snap) {
			return false
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, e := range r.files[serial] {
			if e.FileName == name && (e.Size <= 0 || e.Size == size) {
				return true
			}
		}
		return false
	}
}

// recordUpload keeps the new file and the earlier ones still there.
func (r *Runner) recordUpload(serial string, entry ManifestEntry, others []string) {
	still := map[string]bool{}
	for _, o := range others {
		still[o] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []ManifestEntry
	for _, e := range r.files[serial] {
		if still[e.FileName] && e.FileName != entry.FileName {
			keep = append(keep, e)
		}
	}
	r.files[serial] = append(keep, entry)
	r.saveManifestLocked()
}

func (r *Runner) forgetFile(serial, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []ManifestEntry
	for _, e := range r.files[serial] {
		if e.FileName != name {
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		delete(r.files, serial)
	} else {
		r.files[serial] = keep
	}
	r.saveManifestLocked()
}

// SetEnabled turns delivery on once the link is active with its codes open.
func (r *Runner) SetEnabled(on bool) {
	r.mu.Lock()
	r.enabled = on
	r.mu.Unlock()
	r.Kick()
}

// Kick asks for a sends check now (the sync answer said sends are waiting).
func (r *Runner) Kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Reset forgets every send and deletes the spool (the link was removed).
func (r *Runner) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.enabled = false
	r.recs = map[string]*record{}
	r.errs = map[string]string{}
	r.saveLocked()
	r.wipeSpoolLocked()
}

func (r *Runner) wipeSpoolLocked() {
	entries, _ := os.ReadDir(r.cfg.SpoolDir)
	for _, e := range entries {
		_ = fsutil.Wipe(filepath.Join(r.cfg.SpoolDir, e.Name()))
	}
}

func (r *Runner) spool(id string) string { return filepath.Join(r.cfg.SpoolDir, id+".gcode.3mf") }

func (r *Runner) saveLocked() {
	list := make([]*record, 0, len(r.recs))
	for _, rec := range r.recs {
		list = append(list, rec)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if err := store.SaveJSON(r.cfg.StatePath, list); err != nil {
		r.cfg.Log.Error("could not save sends", "err", err)
	}
}

// Status returns the active send for a printer, if any.
func (r *Runner) Status(serial string) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *record
	for _, rec := range r.recs {
		if rec.Serial == serial && (best == nil || rec.ID > best.ID) {
			best = rec
		}
	}
	if best == nil {
		if e := r.errs[serial]; e != "" {
			return Status{LastError: e}, true
		}
		return Status{}, false
	}
	st := Status{FileName: best.FileName, LastError: best.LastError}
	switch {
	case best.WaitingRetry:
		st.Text = "Upload failed. Waiting for Try again in Spoken Parts."
	case best.Phase == phaseDownloaded:
		st.Text = "Uploading to the printer."
	case best.Phase == phaseUploaded:
		st.Text = "Uploaded. Waiting for someone to tap it on the printer screen."
	case best.Phase == phaseStarted:
		st.Text = "Printing."
	default:
		st.Text = "Getting the file."
	}
	return st, true
}

// Run delivers sends until ctx is done, then waits for deliveries in flight.
func (r *Runner) Run(ctx context.Context) {
	defer r.wg.Wait()
	next := time.Time{}
	for ctx.Err() == nil {
		r.mu.Lock()
		enabled := r.enabled
		open := len(r.recs) > 0
		r.mu.Unlock()
		now := r.cfg.Now()
		if enabled && !now.Before(next) {
			busy := r.poll(ctx)
			wait := r.cfg.PollIdle
			if busy || open {
				wait = r.cfg.PollBusy
			}
			next = r.cfg.Now().Add(wait)
		}
		if enabled {
			r.tick(ctx)
		}
		t := time.NewTimer(r.cfg.Tick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
			next = time.Time{}
		case <-t.C:
		}
	}
}

// poll lists the queued sends and starts work on new ones. It returns true
// when any send is open.
func (r *Runner) poll(ctx context.Context) bool {
	sends, err := r.cfg.API.ListSends(ctx)
	if err != nil {
		r.cfg.Log.Debug("could not list sends", "err", err)
		return false
	}
	listed := map[string]bool{}
	for _, s := range sends {
		if !idRe.MatchString(s.ID) || !fileNameRe.MatchString(s.FileName) {
			continue
		}
		listed[s.ID] = true
		r.mu.Lock()
		rec := r.recs[s.ID]
		busy := r.busy[s.ID]
		gen := r.gen
		switch {
		case busy:
		case rec == nil && (s.LinkState == "" || s.LinkState == "uploading") && s.FileToken != "":
			r.busy[s.ID] = true
			r.goDeliver(ctx, s, gen)
		case rec == nil && (s.LinkState == "uploaded" || s.LinkState == "wrong_file" || s.LinkState == "started"):
			// Known to Spoken Parts as on the printer, but not here (state
			// lost): watch for the start. Never start it again.
			rec = newRecord(s)
			rec.Phase = phaseUploaded
			rec.StartSent = true
			r.recs[s.ID] = rec
			r.saveLocked()
		case rec != nil && rec.WaitingRetry && s.LinkState == "" && s.FileToken != "":
			rec.WaitingRetry, rec.LastError = false, ""
			r.busy[s.ID] = true
			r.saveLocked()
			r.goDeliver(ctx, s, gen)
		}
		r.mu.Unlock()
	}
	// A send that is no longer queued before its print started was cancelled,
	// failed or delivered some other way: stop and clean up.
	r.mu.Lock()
	var gone []*record
	for id, rec := range r.recs {
		if !listed[id] && rec.Phase != phaseStarted && !r.busy[id] {
			gone = append(gone, rec)
			delete(r.recs, id)
		}
	}
	if len(gone) > 0 {
		r.saveLocked()
	}
	r.mu.Unlock()
	for _, rec := range gone {
		_ = fsutil.Wipe(r.spool(rec.ID))
		if rec.Phase == phaseUploaded {
			r.removeFromPrinter(ctx, rec)
		}
		r.cfg.Log.Info("send closed in Spoken Parts, stopped", "send", rec.ID[:8])
	}
	return len(sends) > 0
}

func (r *Runner) goDeliver(ctx context.Context, s api.Send, gen uint64) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.deliver(ctx, s, gen)
	}()
}

func newRecord(s api.Send) *record {
	return &record{
		ID:        s.ID,
		Serial:    s.Printer.Serial,
		Printer:   s.Printer.Name,
		FileName:  s.FileName,
		Subtask:   strings.TrimSuffix(s.FileName, ".gcode.3mf"),
		SHA256:    strings.ToLower(s.SHA256),
		Plate:     s.PlateIndex,
		Mapping:   s.Mapping,
		AutoStart: s.AutoStart,
		Options:   s.Options,
	}
}

func (r *Runner) state(ctx context.Context, id string, req api.StateRequest) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := r.cfg.API.SendState(cctx, id, req)
	if err != nil {
		r.cfg.Log.Info("could not report a send state", "send", id[:8], "state", req.State, "err", err)
	}
	return err
}

// failFinal reports a problem no retry can fix and forgets the send.
func (r *Runner) failFinal(ctx context.Context, s api.Send, reason string) {
	r.cfg.Log.Warn("send failed", "send", s.ID[:8], "serial_tail", ssdp.Tail(s.Printer.Serial), "reason", reason)
	_ = r.state(ctx, s.ID, api.StateRequest{State: "failed", Reason: reason})
	_ = fsutil.Wipe(r.spool(s.ID))
	r.mu.Lock()
	delete(r.recs, s.ID)
	r.errs[s.Printer.Serial] = reason
	r.saveLocked()
	r.mu.Unlock()
}

// uploadFailed reports a problem a person can fix; the link waits for Try again.
func (r *Runner) uploadFailed(ctx context.Context, s api.Send, rec *record, code, reason string) {
	r.cfg.Log.Info("upload waits for the worker", "send", s.ID[:8], "code", code)
	_ = r.state(ctx, s.ID, api.StateRequest{State: "upload_failed", Code: code, Reason: reason})
	r.mu.Lock()
	if rec == nil {
		rec = newRecord(s)
	}
	rec.WaitingRetry = true
	rec.LastError = reason
	r.recs[s.ID] = rec
	r.saveLocked()
	r.mu.Unlock()
}

func (r *Runner) current(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen == gen && r.enabled
}

// deliver runs one send from download to "uploaded" (and the auto start).
func (r *Runner) deliver(ctx context.Context, s api.Send, gen uint64) {
	defer func() {
		r.mu.Lock()
		delete(r.busy, s.ID)
		r.mu.Unlock()
	}()
	snap, ok := r.cfg.Printers.Get(s.Printer.Serial)
	if ok {
		s.Printer.Serial = snap.Serial // the spelling the printer announces
	}
	r.mu.Lock()
	rec := r.recs[s.ID]
	r.mu.Unlock()
	switch {
	case !ok || snap.Access != printers.AccessOK || !snap.Connected:
		if time.Duration(s.AgeS)*time.Second > r.cfg.MaxAge {
			r.uploadFailed(ctx, s, rec, "cant_reach", "The link can't reach the printer.")
		}
		return // try again on the next list
	case snap.ModelCode != "" && s.Printer.ModelCode != "" && !strings.EqualFold(snap.ModelCode, s.Printer.ModelCode):
		r.failFinal(ctx, s, "This send is for a different printer model than the one with this serial.")
		return
	case snap.Storage != nil && !*snap.Storage:
		r.uploadFailed(ctx, s, rec, "no_storage", "No USB stick in the printer.")
		return
	case snap.Busy():
		if time.Duration(s.AgeS)*time.Second > r.cfg.MaxAge {
			r.uploadFailed(ctx, s, rec, "printer_busy", "The printer is busy with another print.")
		}
		return
	case snap.HMSFatal:
		r.uploadFailed(ctx, s, rec, "printer_error", "The printer reports an error ("+strings.Join(snap.HMS, ", ")+").")
		return
	}
	path := r.spool(s.ID)
	if rec == nil || rec.MD5 == "" || !exists(path) {
		if time.Duration(s.AgeS)*time.Second > r.cfg.MaxAge {
			r.failFinal(ctx, s, "This send is older than 30 minutes. Send it again in Spoken Parts.")
			return
		}
		info, err := r.download(ctx, s, path)
		switch {
		case errors.Is(err, errChecksum):
			r.failFinal(ctx, s, "The file did not match its checksum.")
			return
		case errors.Is(err, errBadFile):
			r.failFinal(ctx, s, "The file is not a complete sliced plate.")
			return
		case err != nil:
			r.cfg.Log.Info("could not download a send, trying again", "send", s.ID[:8], "err", err)
			return
		}
		rec = newRecord(s)
		rec.MD5 = info.GcodeMD5
		rec.Phase = phaseDownloaded
		r.mu.Lock()
		if r.gen != gen {
			r.mu.Unlock()
			_ = fsutil.Wipe(path)
			return
		}
		r.recs[s.ID] = rec
		r.saveLocked()
		r.mu.Unlock()
	}
	if !r.current(gen) {
		return
	}
	others, err := r.upload(ctx, s, rec, path, snap)
	if err != nil {
		return
	}
	r.mu.Lock()
	if r.gen != gen {
		r.mu.Unlock()
		return
	}
	rec.Phase, rec.UploadedAt, rec.LastError = phaseUploaded, r.cfg.Now(), ""
	r.saveLocked()
	r.mu.Unlock()
	if len(others) > 0 {
		r.cfg.Log.Warn("earlier Spoken Parts files are still on the printer", "serial_tail", ssdp.Tail(s.Printer.Serial), "files", strings.Join(others, ","))
	}
	_ = r.state(ctx, s.ID, api.StateRequest{State: "uploaded", FileName: s.FileName, Others: others})
	r.autoStart(rec, snap)
}

var (
	errChecksum = errors.New("checksum mismatch")
	errBadFile  = errors.New("not a sliced plate")
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (r *Runner) download(ctx context.Context, s api.Send, path string) (*threemf.Info, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fsutil.PrivateFile)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(fsutil.PrivateFile)
	dctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	_, sum, header, err := r.cfg.API.Download(dctx, s, f)
	cancel()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = fsutil.Wipe(path)
		return nil, err
	}
	want := strings.ToLower(s.SHA256)
	if want == "" || sum != want || (header != "" && header != want) {
		_ = fsutil.Wipe(path)
		return nil, errChecksum
	}
	info, err := threemf.Check(path, max(s.PlateIndex, 1))
	if err != nil {
		_ = fsutil.Wipe(path)
		return nil, errBadFile
	}
	return info, nil
}

func (r *Runner) upload(ctx context.Context, s api.Send, rec *record, path string, snap printers.Snapshot) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zero := 0
	_ = r.state(ctx, s.ID, api.StateRequest{State: "uploading", Percent: &zero})
	var mu sync.Mutex
	sent := int64(0)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		last := -1
		for {
			select {
			case <-done:
				return
			case <-t.C:
				mu.Lock()
				pct := int(sent * 100 / max(st.Size(), 1))
				mu.Unlock()
				if pct != last && pct < 100 {
					last = pct
					_ = r.state(ctx, s.ID, api.StateRequest{State: "uploading", Percent: &pct})
				}
			}
		}
	}()
	uctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	others, err := r.cfg.Printers.UploadClean(uctx, s.Printer.Serial, s.FileName, f, st.Size(), func(n int64) {
		mu.Lock()
		sent = n
		mu.Unlock()
	}, r.removable(s.Printer.Serial, snap))
	cancel()
	close(done)
	if err == nil {
		r.recordUpload(s.Printer.Serial, ManifestEntry{FileName: s.FileName, SendID: s.ID, Size: st.Size(), MD5: rec.MD5, UploadedAt: r.cfg.Now().UTC()}, others)
		return others, nil
	}
	code, reason := "", "The upload failed."
	switch {
	case errors.Is(err, printers.ErrNoStorage):
		code, reason = "no_storage", "No USB stick in the printer."
	case errors.Is(err, printers.ErrRefused):
		code, reason = "storage_refused", "The printer did not take the file (553)."
	case errors.Is(err, printers.ErrAccessCode):
		code, reason = "access_code", "The printer refused the access code."
	case errors.Is(err, printers.ErrCertificate):
		code, reason = "certificate", "The printer's certificate did not check out."
	case errors.Is(err, printers.ErrCantReach), errors.Is(err, printers.ErrNotReady):
		code, reason = "cant_reach", "The link can't reach the printer."
	}
	r.uploadFailed(ctx, s, rec, code, reason)
	return nil, err
}

// autoStart sends project_file only if everyone allows it. Recorded first.
func (r *Runner) autoStart(rec *record, snap printers.Snapshot) {
	if !rec.AutoStart || !r.cfg.Printers.AutoStart() || snap.DevMode == nil || !*snap.DevMode {
		return
	}
	r.mu.Lock()
	if rec.StartSent {
		r.mu.Unlock()
		return
	}
	rec.StartSent = true
	r.saveLocked()
	r.mu.Unlock()
	pf := mqtt.ProjectFile{
		Plate:         max(rec.Plate, 1),
		URL:           "ftp:///" + rec.FileName,
		MD5:           rec.MD5,
		SubtaskName:   rec.Subtask,
		UseAMS:        rec.Options.UseAMS && len(rec.Mapping) > 0,
		Timelapse:     rec.Options.Timelapse,
		BedLeveling:   rec.Options.BedLeveling,
		FlowCali:      rec.Options.FlowCali,
		VibrationCali: rec.Options.VibrationCali,
		LayerInspect:  rec.Options.LayerInspect,
	}
	pf.AmsMapping, pf.AmsMapping2 = amsMapping(rec.Mapping)
	if err := r.cfg.Printers.StartProjectFile(rec.Serial, pf); err != nil {
		r.cfg.Log.Info("auto start not sent, waiting for someone to tap the file", "send", rec.ID[:8], "err", err)
	}
}

// amsMapping builds ams_mapping and ams_mapping2 indexed by filament (1 based
// in the plan): -1 and 255/255 for filaments the plate does not use.
func amsMapping(rows []api.MapRow) ([]int, []mqtt.AmsSlot) {
	n := 0
	for _, row := range rows {
		n = max(n, row.Filament)
	}
	m := make([]int, n)
	m2 := make([]mqtt.AmsSlot, n)
	for i := range m {
		m[i], m2[i] = -1, mqtt.AmsSlot{AmsID: 255, SlotID: 255}
	}
	for _, row := range rows {
		if row.Filament < 1 {
			continue
		}
		m[row.Filament-1] = row.Tray
		m2[row.Filament-1] = mqtt.AmsSlot{AmsID: row.AmsID, SlotID: row.SlotID}
	}
	return m, m2
}

// matches reports whether the printer is running this send's file.
func matches(rec *record, snap printers.Snapshot) bool {
	want := strings.ToLower(rec.Subtask)
	sub := strings.ToLower(snap.Subtask)
	return sub == want || strings.TrimSuffix(sub, ".gcode.3mf") == want || strings.TrimSuffix(sub, ".3mf") == want ||
		strings.Contains(strings.ToLower(snap.GcodeFile), want)
}

// tick watches every uploaded or started send.
func (r *Runner) tick(ctx context.Context) {
	r.mu.Lock()
	var recs []*record
	for _, rec := range r.recs {
		if rec.Phase == phaseUploaded || rec.Phase == phaseStarted {
			recs = append(recs, rec)
		}
	}
	r.mu.Unlock()
	for _, rec := range recs {
		snap, ok := r.cfg.Printers.Get(rec.Serial)
		if !ok || !snap.Connected {
			continue
		}
		switch rec.Phase {
		case phaseUploaded:
			r.watchStart(ctx, rec, snap)
		case phaseStarted:
			r.track(ctx, rec, snap)
		}
	}
}

func (r *Runner) watchStart(ctx context.Context, rec *record, snap printers.Snapshot) {
	r.mu.Lock()
	startSent, refused, wrong := rec.StartSent, rec.AutoRefused, rec.WrongFile
	r.mu.Unlock()
	if startSent && !refused && snap.Command != nil && snap.Command.Command == "project_file" && snap.Command.Refused() {
		r.mu.Lock()
		rec.AutoRefused = true
		r.saveLocked()
		r.mu.Unlock()
		r.cfg.Log.Warn("the printer refused the auto start (Cloud mode or no authorization), waiting for someone to tap the file",
			"serial_tail", ssdp.Tail(rec.Serial), "reason", snap.Command.Reason)
	}
	if !snap.Busy() || snap.GcodeState == "PAUSE" {
		return
	}
	if !matches(rec, snap) {
		if snap.Subtask != "" && snap.Subtask != wrong {
			if r.state(ctx, rec.ID, api.StateRequest{State: "wrong_file", SubtaskName: snap.Subtask}) == nil {
				r.mu.Lock()
				rec.WrongFile = snap.Subtask
				r.saveLocked()
				r.mu.Unlock()
			}
		}
		return
	}
	err := r.state(ctx, rec.ID, api.StateRequest{State: "delivered", SHA256: rec.SHA256})
	var ae *api.Error
	if err != nil && !(errors.As(err, &ae) && ae.Status == 409) {
		return // try again on the next tick
	}
	r.cfg.Log.Info("print started on the printer", "send", rec.ID[:8], "serial_tail", ssdp.Tail(rec.Serial))
	r.mu.Lock()
	rec.Phase, rec.StartedAt = phaseStarted, r.cfg.Now()
	r.saveLocked()
	r.mu.Unlock()
	_ = fsutil.Wipe(r.spool(rec.ID)) // on the printer now
	r.track(ctx, rec, snap)
}

func pint(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (r *Runner) track(ctx context.Context, rec *record, snap printers.Snapshot) {
	now := r.cfg.Now()
	ended := snap.GcodeState == "FINISH" || snap.GcodeState == "FAILED"
	other := snap.Busy() && !matches(rec, snap) // a new print: ours ended unseen
	if other {
		ended = true
	}
	r.mu.Lock()
	changed := snap.GcodeState != rec.LastState || pint(snap.Percent) != rec.LastPercent || pint(snap.Layer) != rec.LastLayer
	if !other && changed && (snap.GcodeState != rec.LastState || now.Sub(rec.LastEventAt) >= r.cfg.EventEvery) && snap.GcodeState != "" {
		rec.Seq++
		ev := api.Event{
			Key:          fmt.Sprintf("%s-%05d", rec.ID[:8], rec.Seq),
			SendID:       rec.ID,
			ObservedAt:   now.UTC().Format(time.RFC3339Nano),
			GcodeState:   snap.GcodeState,
			Percent:      snap.Percent,
			Layer:        snap.Layer,
			TotalLayers:  snap.TotalLayers,
			RemainingMin: snap.RemainingMin,
			PrintError:   snap.PrintError,
			HMS:          snap.HMS,
			SubtaskName:  snap.Subtask,
		}
		rec.Pending = append(rec.Pending, ev)
		if len(rec.Pending) > 200 {
			rec.Pending = rec.Pending[len(rec.Pending)-200:]
		}
		rec.LastState, rec.LastPercent, rec.LastLayer, rec.LastEventAt = snap.GcodeState, pint(snap.Percent), pint(snap.Layer), now
		r.saveLocked()
	}
	alert := ""
	if !rec.Alerted && !other && len(rec.Mapping) > 0 && len(snap.Mapping) > 0 {
		alert = wrongSlot(rec.Mapping, snap.Mapping)
		if alert != "" {
			rec.Alerted = true
			r.saveLocked()
		}
	}
	pending := append([]api.Event(nil), rec.Pending...)
	r.mu.Unlock()
	if alert != "" {
		r.cfg.Log.Warn("the print uses an AMS slot it should not", "send", rec.ID[:8], "detail", alert)
		_ = r.state(ctx, rec.ID, api.StateRequest{State: "started", Alert: &api.SendAlert{Kind: "wrong_slot", Detail: alert}})
	}
	sentAll := true
	for len(pending) > 0 {
		n := min(len(pending), 50)
		if err := r.cfg.API.Events(ctx, rec.Serial, pending[:n]); err != nil {
			r.cfg.Log.Debug("could not report events, keeping them", "send", rec.ID[:8], "err", err)
			sentAll = false
			break
		}
		r.mu.Lock()
		rec.Pending = rec.Pending[min(n, len(rec.Pending)):]
		r.saveLocked()
		r.mu.Unlock()
		pending = pending[n:]
	}
	if ended && sentAll {
		r.removeFromPrinter(ctx, rec)
		r.mu.Lock()
		delete(r.recs, rec.ID)
		r.saveLocked()
		r.mu.Unlock()
		_ = fsutil.Wipe(r.spool(rec.ID))
		r.cfg.Log.Info("print ended, send closed", "send", rec.ID[:8], "state", snap.GcodeState)
	}
}

// wrongSlot compares the plan with the printer's mapping in use
// ((ams_id << 8) | slot_id per filament, 0xFFFF unused).
func wrongSlot(plan []api.MapRow, mapping []int) string {
	for _, row := range plan {
		i := row.Filament - 1
		if i < 0 || i >= len(mapping) || mapping[i] == 0xFFFF || mapping[i] < 0 {
			continue
		}
		want := row.AmsID<<8 | row.SlotID
		if mapping[i] != want {
			got := mapping[i]
			return fmt.Sprintf("Filament %d prints from %s, not %s.", row.Filament, slotName(got>>8, got&0xFF), slotName(row.AmsID, row.SlotID))
		}
	}
	return ""
}

func slotName(ams, slot int) string {
	switch {
	case ams >= 0 && ams < 4:
		return fmt.Sprintf("AMS %c slot %d", 'A'+ams, slot+1)
	case ams >= 128 && ams < 136:
		return fmt.Sprintf("AMS HT %d", ams-127)
	case ams == 254 || ams == 255:
		return "the external spool"
	}
	return fmt.Sprintf("AMS %d slot %d", ams, slot+1)
}

// removeFromPrinter deletes the file the link uploaded (our names only).
func (r *Runner) removeFromPrinter(ctx context.Context, rec *record) {
	if !fileNameRe.MatchString(rec.FileName) {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := r.cfg.Printers.Delete(dctx, rec.Serial, rec.FileName); err != nil {
		// It stays in the manifest: the next upload to this printer removes it.
		r.cfg.Log.Info("could not delete the uploaded file from the printer", "serial_tail", ssdp.Tail(rec.Serial), "err", err)
		return
	}
	r.forgetFile(rec.Serial, rec.FileName)
	r.cfg.Log.Info("deleted the uploaded file from the printer", "serial_tail", ssdp.Tail(rec.Serial), "file", rec.FileName)
}

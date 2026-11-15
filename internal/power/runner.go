// Package power relays between the Spoken Parts Power integration in Home
// Assistant (the engine that protects each breaker) and Spoken Parts (0.7).
//
// The engine runs inside Home Assistant and does all switching itself, on
// the plugs of circuits the owner configured in Spoken Parts. This package
// never switches anything and never talks to a printer. It:
//
//   - sends the engine a feed frame every 2 s: each printer's state (from the
//     reports the link already receives) and the value of its print (from the
//     job values Spoken Parts sends), so the engine knows which print to keep;
//   - pulls the engine's audit entries every second, keeps them in a disk
//     queue, acknowledges them to the engine, and uploads them to Spoken
//     Parts (POST power/events) when it can;
//   - every 5 s (or as Spoken Parts says) sends the engine's status to
//     Spoken Parts (POST power/sync) and passes back the circuit config and
//     people's requests (turn a printer's power back on, try a heater again).
//
// When the link is paused, nothing goes to Spoken Parts, but the feed and
// the queue keep running: protection is local and never waits for the cloud.
package power

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/haapi"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
)

// API is the Spoken Parts side (signed calls).
type API interface {
	PowerSync(ctx context.Context, r api.PowerSyncRequest) (*api.PowerSyncResponse, error)
	PowerEvents(ctx context.Context, r api.PowerEventsRequest) (*api.PowerEventsResponse, error)
}

// Engine is the Spoken Parts Power integration (haapi.WS).
type Engine interface {
	Call(ctx context.Context, typ string, data map[string]any) (json.RawMessage, error)
	Close()
}

// Source gives the printers' reports (the printer manager).
type Source interface {
	Feeds() []monitor.PrinterFeed
}

// Config for New. Zero durations get production defaults.
type Config struct {
	API       API
	Engine    Engine
	Source    Source
	Log       *logx.Logger
	Now       func() time.Time
	QueuePath string

	Tick        time.Duration // 1 s: entries pulled from the engine
	FeedEvery   time.Duration // 2 s
	SyncEvery   time.Duration // 5 s unless Spoken Parts says otherwise
	CallTimeout time.Duration // 10 s per call
	RetryMax    time.Duration // 60 s between failed uploads at most
	MaxQueue    int           // 5000 entries kept on disk
}

// Status is what the link page can show.
type Status struct {
	Engine        string // ok, not_installed, no_token, unreachable, ""
	EngineVersion string
	Circuits      int
	Plugs         int
	Queued        int
	LastSync      time.Time
	LastError     string
}

type queued struct {
	EngineID string         `json:"engine_id"`
	Entry    api.PowerEntry `json:"entry"`
}

type queueFile struct {
	EngineID string   `json:"engine_id"`
	LastSeq  int64    `json:"last_seq"`
	Entries  []queued `json:"entries"`
}

// Runner relays power data. Run it once; turn it on with SetEnabled.
type Runner struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	enabled  bool
	paused   bool
	gen      uint64
	status   Status
	jobs     map[string]api.PowerJob
	q        queueFile
	failures int
	nextUp   time.Time
	nextSync time.Time
	nextFeed time.Time
}

// New returns a Runner and loads its queue.
func New(cfg Config) *Runner {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Tick, time.Second)
	def(&cfg.FeedEvery, 2*time.Second)
	def(&cfg.SyncEvery, 5*time.Second)
	def(&cfg.CallTimeout, 10*time.Second)
	def(&cfg.RetryMax, time.Minute)
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 5000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Runner{cfg: cfg, wake: make(chan struct{}, 1), jobs: map[string]api.PowerJob{}}
	r.loadQueue()
	return r
}

func (r *Runner) loadQueue() {
	if r.cfg.QueuePath == "" {
		return
	}
	data, err := os.ReadFile(r.cfg.QueuePath)
	if err != nil {
		return
	}
	var q queueFile
	if json.Unmarshal(data, &q) == nil {
		r.q = q
	}
}

func (r *Runner) saveQueueLocked() error {
	if r.cfg.QueuePath == "" {
		return nil
	}
	data, err := json.Marshal(r.q)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(r.cfg.QueuePath, data)
}

// SetEnabled turns the runner on once the link is active.
func (r *Runner) SetEnabled(on bool) {
	r.mu.Lock()
	changed := r.enabled != on
	r.enabled = on
	r.mu.Unlock()
	if changed {
		r.kick()
	}
}

// SetPaused follows the sync answer: a paused link sends nothing to Spoken Parts.
func (r *Runner) SetPaused(p bool) {
	r.mu.Lock()
	r.paused = p
	r.mu.Unlock()
}

// Reset forgets the queue and the jobs (the link was removed or unpaired).
func (r *Runner) Reset() {
	r.mu.Lock()
	r.gen++
	r.enabled, r.paused = false, false
	r.status = Status{}
	r.jobs = map[string]api.PowerJob{}
	r.q = queueFile{}
	if r.cfg.QueuePath != "" {
		_ = fsutil.Wipe(r.cfg.QueuePath)
	}
	r.mu.Unlock()
	r.cfg.Engine.Close()
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
	st.Queued = len(r.q.Entries)
	return st
}

// Run works until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	for ctx.Err() == nil {
		r.mu.Lock()
		on := r.enabled
		r.mu.Unlock()
		wait := time.Hour
		if on {
			r.Step(ctx)
			wait = r.cfg.Tick
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Step does one pass: feed, pull entries, upload, sync (each when due).
func (r *Runner) Step(ctx context.Context) {
	now := r.cfg.Now()
	r.mu.Lock()
	gen, paused := r.gen, r.paused
	feedDue := !now.Before(r.nextFeed)
	upDue := !paused && !now.Before(r.nextUp)
	syncDue := !paused && !now.Before(r.nextSync)
	r.mu.Unlock()
	if feedDue {
		r.feed(ctx)
		r.mu.Lock()
		r.nextFeed = now.Add(r.cfg.FeedEvery)
		r.mu.Unlock()
	}
	r.pull(ctx, gen)
	if upDue {
		r.upload(ctx, gen)
	}
	if syncDue {
		r.sync(ctx, gen)
	}
}

func (r *Runner) call(ctx context.Context, typ string, data map[string]any, out any) error {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	raw, err := r.cfg.Engine.Call(cctx, typ, data)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func engineCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, haapi.ErrNotInstalled):
		return "not_installed"
	case errors.Is(err, haapi.ErrNoToken):
		return "no_token"
	}
	return "unreachable"
}

func (r *Runner) setEngine(err error) {
	r.mu.Lock()
	r.status.Engine = engineCode(err)
	r.mu.Unlock()
}

func (r *Runner) feed(ctx context.Context) {
	r.mu.Lock()
	jobs := make(map[string]api.PowerJob, len(r.jobs))
	for k, v := range r.jobs {
		jobs[k] = v
	}
	r.mu.Unlock()
	printers := Build(r.cfg.Source.Feeds(), jobs, r.cfg.Now())
	if err := r.call(ctx, "spoken_parts_power/feed", map[string]any{"printers": printers}, nil); err != nil {
		r.setEngine(err)
	}
}

type entriesAnswer struct {
	EngineID string           `json:"engine_id"`
	Entries  []api.PowerEntry `json:"entries"`
	LastSeq  int64            `json:"last_seq"`
}

// pull moves new engine entries to the disk queue, then acknowledges them
// to the engine (only once they are safely on disk).
func (r *Runner) pull(ctx context.Context, gen uint64) {
	r.mu.Lock()
	after := r.q.LastSeq
	known := r.q.EngineID
	r.mu.Unlock()
	var ans entriesAnswer
	err := r.call(ctx, "spoken_parts_power/entries", map[string]any{"after": after, "limit": 200}, &ans)
	if err != nil {
		r.setEngine(err)
		return
	}
	if ans.EngineID == "" {
		return
	}
	if ans.EngineID != known || ans.LastSeq < after {
		// A new engine (or its storage was reset): start from its first entry.
		r.mu.Lock()
		r.q.EngineID, r.q.LastSeq = ans.EngineID, 0
		r.mu.Unlock()
		if after != 0 {
			return // pull again from 0 on the next pass
		}
	}
	if len(ans.Entries) == 0 {
		return
	}
	r.mu.Lock()
	if r.gen != gen {
		r.mu.Unlock()
		return
	}
	last := r.q.LastSeq
	for _, e := range ans.Entries {
		if e.Seq <= last || !validKind(e.Kind) {
			continue
		}
		r.q.Entries = append(r.q.Entries, queued{EngineID: ans.EngineID, Entry: e})
		last = e.Seq
	}
	if n := len(r.q.Entries) - r.cfg.MaxQueue; n > 0 {
		r.q.Entries = r.q.Entries[n:]
	}
	r.q.LastSeq = last
	err = r.saveQueueLocked()
	r.mu.Unlock()
	if err != nil {
		r.logWarn("could not save the power queue", err)
		return
	}
	_ = r.call(ctx, "spoken_parts_power/ack", map[string]any{"through": last}, nil)
}

func validKind(k string) bool {
	if k == "" || len(k) > 40 {
		return false
	}
	for _, c := range k {
		if (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

func (r *Runner) upload(ctx context.Context, gen uint64) {
	for i := 0; i < 5 && ctx.Err() == nil; i++ {
		r.mu.Lock()
		if len(r.q.Entries) == 0 || r.gen != gen {
			r.mu.Unlock()
			return
		}
		eid := r.q.Entries[0].EngineID
		var batch []api.PowerEntry
		for _, q := range r.q.Entries {
			if q.EngineID != eid || len(batch) == 200 {
				break
			}
			batch = append(batch, q.Entry)
		}
		r.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
		ans, err := r.cfg.API.PowerEvents(cctx, api.PowerEventsRequest{EngineID: eid, Entries: batch})
		cancel()
		r.mu.Lock()
		if err != nil {
			r.failures++
			r.nextUp = r.cfg.Now().Add(min(time.Duration(1<<min(r.failures, 6))*time.Second, r.cfg.RetryMax))
			r.status.LastError = "events: " + short(err)
			r.mu.Unlock()
			return
		}
		r.failures = 0
		keep := r.q.Entries[:0]
		for _, q := range r.q.Entries {
			if q.EngineID == eid && q.Entry.Seq <= ans.StoredThrough {
				continue
			}
			keep = append(keep, q)
		}
		dropped := len(keep) < len(r.q.Entries)
		r.q.Entries = keep
		_ = r.saveQueueLocked()
		r.mu.Unlock()
		if !dropped {
			return // nothing was taken: do not loop on it
		}
	}
}

type statusAnswer struct {
	Engine   json.RawMessage `json:"engine"`
	Circuits json.RawMessage `json:"circuits"`
	Plugs    json.RawMessage `json:"plugs"`
	Errors   []string        `json:"errors"`
}

// withErrors puts the engine's config errors (circuits it refused) into the engine
// object as engine.errors, short strings, at most 20, unless it already has them.
func withErrors(engine json.RawMessage, errs []string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(engine, &m) != nil || m == nil {
		return engine
	}
	if _, ok := m["errors"]; ok {
		return engine
	}
	out := make([]string, 0, len(errs))
	for i, e := range errs {
		if i >= 20 {
			break
		}
		out = append(out, cut(e, 200))
	}
	b, _ := json.Marshal(out)
	m["errors"] = b
	merged, err := json.Marshal(m)
	if err != nil {
		return engine
	}
	return merged
}

func (r *Runner) sync(ctx context.Context, gen uint64) {
	var st statusAnswer
	err := r.call(ctx, "spoken_parts_power/status", nil, &st)
	r.setEngine(err)
	req := api.PowerSyncRequest{Engine: withErrors(st.Engine, st.Errors), Circuits: st.Circuits, Plugs: st.Plugs}
	var engineVersion string
	var engineConfig int
	if err != nil {
		code := engineCode(err)
		req = api.PowerSyncRequest{EngineError: &code}
	} else {
		var e struct {
			Version       string `json:"version"`
			ConfigVersion int    `json:"config_version"`
		}
		_ = json.Unmarshal(st.Engine, &e)
		engineVersion, engineConfig = e.Version, e.ConfigVersion
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	ans, aerr := r.cfg.API.PowerSync(cctx, req)
	cancel()
	now := r.cfg.Now()
	r.mu.Lock()
	if r.gen != gen {
		r.mu.Unlock()
		return
	}
	r.status.EngineVersion = engineVersion
	r.status.Circuits, r.status.Plugs = countArray(st.Circuits), countArray(st.Plugs)
	if aerr != nil {
		r.status.LastError = "sync: " + short(aerr)
		wait := r.cfg.SyncEvery * 2
		if api.HasCode(aerr, "not_found") || isStatus(aerr, 404) {
			wait = 5 * time.Minute // a Spoken Parts without power protection yet
		}
		if ra := api.RetryAfter(aerr); ra > wait {
			wait = ra
		}
		r.nextSync = now.Add(wait)
		r.mu.Unlock()
		return
	}
	r.status.LastSync, r.status.LastError = now, ""
	next := r.cfg.SyncEvery
	if ans.NextS > 0 {
		next = min(max(time.Duration(ans.NextS)*time.Second, 2*time.Second), time.Minute)
	}
	r.nextSync = now.Add(next)
	jobs := map[string]api.PowerJob{}
	for _, j := range ans.Jobs {
		s := strings.ToUpper(strings.TrimSpace(j.Serial))
		if s != "" && len(jobs) < 200 {
			jobs[s] = j
		}
	}
	r.jobs = jobs
	r.mu.Unlock()
	if err != nil {
		return // the engine is not there: nothing to hand it
	}
	if len(ans.Config) > 0 && string(ans.Config) != "null" && ans.ConfigVersion != engineConfig {
		if cerr := r.call(ctx, "spoken_parts_power/config", map[string]any{"config": ans.Config}, nil); cerr != nil {
			r.logWarn("the power engine did not take the config", cerr)
		}
	}
	for i, q := range ans.Requests {
		if i >= 20 {
			break
		}
		req := map[string]any{"id": cut(q.ID, 64), "kind": q.Kind, "plug": cut(q.Plug, 64), "by": cut(q.By, 80)}
		if q.Kind != "power_on" && q.Kind != "retry" {
			continue
		}
		// The engine decides, journals its answer, and that entry goes up.
		_ = r.call(ctx, "spoken_parts_power/command", map[string]any{"request": req}, nil)
	}
}

func isStatus(err error, code int) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Status == code
}

func countArray(raw json.RawMessage) int {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return 0
	}
	return len(a)
}

func short(err error) string {
	return cut(err.Error(), 120)
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (r *Runner) logWarn(msg string, err error) {
	if r.cfg.Log != nil {
		r.cfg.Log.Warn(msg, "err", short(err))
	}
}

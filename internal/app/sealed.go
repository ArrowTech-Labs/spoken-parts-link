package app

// Sealed access codes (0.4, scheme sp-code-v1): an owner types a printer's
// access code on spokenparts.com, the browser seals it to this link's code
// box key, and Spoken Parts passes the envelope on without being able to
// open it. The link opens it and saves the code exactly like the link page.
//
// All of this is additive. An older Spoken Parts answers 404 to the new
// calls: the link then stays quiet and asks again at a slow pace. Nothing
// here can stop the sync or anything else.

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/codebox"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

const (
	// codeBoxInfo is the HKDF info for the file that keeps the code box
	// private key (secrets/codebox.enc), keyed like the access codes.
	codeBoxInfo  = "code-box-v1"
	codeBoxEntry = "p256"
	// codeSlowRetry is how often the link asks again when Spoken Parts does
	// not know the new calls yet (404).
	codeSlowRetry = time.Hour
	// maxHandled ids are remembered so an envelope is never opened twice.
	// Envelopes expire after 15 minutes, so this is far above normal use.
	maxHandled = 256
	// maxEnvelopes is the most envelopes handled in one sync cycle.
	maxEnvelopes = 20
)

var sealedSerialRe = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)

// sealedState is guarded by App.mu. It lives in memory only, so the key is
// registered again on every start.
type sealedState struct {
	registered bool
	keyNext    time.Time
	keyFails   int
	codesNext  time.Time
	codesFails int
	handled    map[api.EnvelopeID]*handledCode
	order      []api.EnvelopeID
}

type handledCode struct {
	req      api.CodeResultRequest
	reported bool
}

func notFound(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Status == 404
}

// openBoxLocked opens (or makes) the code box key. It is called right after
// the access codes are opened, with the same local secret and data key.
func (a *App) openBoxLocked(localSecret, dataKey []byte) {
	a.boxKey = nil
	a.sealed = sealedState{}
	fk, err := store.DeriveKeyFor(localSecret, dataKey, codeBoxInfo)
	if err != nil {
		a.log.Error("could not derive the code box file key", "err", err)
		return
	}
	a.red.AddBytes(fk)
	err = a.box.Unlock(fk)
	clear(fk)
	switch {
	case errors.Is(err, store.ErrCannotOpen):
		a.log.Warn("the saved key for access codes typed on spokenparts.com could not be opened, making a new one")
	case err != nil:
		a.log.Error("could not read the key for access codes typed on spokenparts.com", "err", err)
		return
	}
	if v, ok := a.box.Get(codeBoxEntry); ok {
		a.red.Add(v)
		raw, derr := hex.DecodeString(v)
		if derr == nil {
			a.red.AddBytes(raw)
			if k, kerr := codebox.FromBytes(raw); kerr == nil {
				a.boxKey = k
			}
		}
		clear(raw)
	}
	if a.boxKey != nil {
		return
	}
	k, err := codebox.Generate()
	if err != nil {
		a.log.Error("could not make a key for access codes typed on spokenparts.com", "err", err)
		return
	}
	raw := k.Bytes()
	a.red.AddBytes(raw)
	v := hex.EncodeToString(raw)
	clear(raw)
	if err := a.box.Set(codeBoxEntry, v); err != nil {
		// Still usable until the next start; then a new key is registered.
		a.log.Error("could not save the key for access codes typed on spokenparts.com", "err", err)
	}
	a.boxKey = k
	a.log.Info("made a key for access codes typed on spokenparts.com", "key_id", k.ID())
}

// wipeBoxLocked deletes the code box key with the access codes.
func (a *App) wipeBoxLocked(wipe bool) {
	if wipe {
		if err := a.box.Wipe(); err != nil {
			a.log.Error("could not delete the code box key file", "err", err)
		}
	} else {
		a.box.Lock()
	}
	a.boxKey = nil
	a.sealed = sealedState{}
}

// sealedStep runs after a good sync: register the key if needed, then fetch
// and handle sealed codes. It never changes the sync's own wait.
func (a *App) sealedStep(ctx context.Context, s snapshot) {
	a.mu.Lock()
	k := a.boxKey
	ok := a.currentLocked(s) && a.codesReady && k != nil
	now := a.cfg.Now()
	needKey := !a.sealed.registered && !now.Before(a.sealed.keyNext)
	a.mu.Unlock()
	if !ok {
		return
	}
	if needKey {
		a.registerBoxKey(ctx, s, k)
	}
	a.mu.Lock()
	fetch := a.currentLocked(s) && a.boxKey == k && a.sealed.registered && !a.cfg.Now().Before(a.sealed.codesNext)
	a.mu.Unlock()
	if fetch {
		a.fetchSealed(ctx, s, k)
	}
}

func (a *App) registerBoxKey(ctx context.Context, s snapshot, k *codebox.Key) {
	id, err := a.api.RegisterCodeKey(ctx, s.key, s.linkID, k.PublicKey())
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.currentLocked(s) || a.boxKey != k {
		return
	}
	now := a.cfg.Now()
	switch {
	case notFound(err):
		a.sealed.keyNext = now.Add(a.cfg.CodeSlowRetry)
		a.log.Debug("Spoken Parts does not take access codes typed on its website yet", "retry_in", a.cfg.CodeSlowRetry)
	case err == nil && id != "" && id != k.ID():
		a.sealed.keyFails++
		wait := api.Backoff(a.sealed.keyFails, a.cfg.RetryMin, a.cfg.RetryMax)
		a.sealed.keyNext = now.Add(wait)
		a.log.Warn("Spoken Parts answered a different key id for the access code key", "key_id", k.ID(), "retry_in", wait.Round(time.Second))
	case err != nil:
		a.sealed.keyFails++
		wait := max(api.Backoff(a.sealed.keyFails, a.cfg.RetryMin, a.cfg.RetryMax), api.RetryAfter(err))
		a.sealed.keyNext = now.Add(wait)
		a.log.Info("could not register the access code key with Spoken Parts", "err", err, "retry_in", wait.Round(time.Second))
	default:
		a.sealed.registered = true
		a.sealed.keyFails = 0
		a.sealed.codesNext = time.Time{}
		a.log.Info("access codes typed on spokenparts.com can now reach this link", "key_id", k.ID())
	}
}

func (a *App) fetchSealed(ctx context.Context, s snapshot, k *codebox.Key) {
	envs, err := a.api.Codes(ctx, s.key, s.linkID)
	a.mu.Lock()
	if !a.currentLocked(s) || a.boxKey != k {
		a.mu.Unlock()
		return
	}
	now := a.cfg.Now()
	switch {
	case notFound(err):
		a.sealed.codesNext = now.Add(a.cfg.CodeSlowRetry)
		a.log.Debug("Spoken Parts does not list access codes typed on its website yet", "retry_in", a.cfg.CodeSlowRetry)
		a.mu.Unlock()
		return
	case err != nil:
		a.sealed.codesFails++
		wait := max(api.Backoff(a.sealed.codesFails, a.cfg.RetryMin, a.cfg.RetryMax), api.RetryAfter(err))
		a.sealed.codesNext = now.Add(wait)
		a.log.Info("could not check for access codes typed on spokenparts.com", "err", err, "retry_in", wait.Round(time.Second))
		a.mu.Unlock()
		return
	}
	a.sealed.codesFails = 0
	a.mu.Unlock()
	if len(envs) > maxEnvelopes {
		envs = envs[:maxEnvelopes]
	}
	for _, e := range envs {
		if ctx.Err() != nil {
			return
		}
		a.handleSealed(ctx, s, k, e)
	}
}

func (a *App) handleSealed(ctx context.Context, s snapshot, k *codebox.Key, e api.CodeEnvelope) {
	if !api.EnvelopeIDRe.MatchString(string(e.ID)) {
		a.log.Debug("ignored a sealed access code with a bad id")
		return
	}
	a.mu.Lock()
	h := a.sealed.handled[e.ID]
	done := h != nil && h.reported
	a.mu.Unlock()
	if done {
		return
	}
	if h == nil {
		// Never opened twice, whatever happens next. A result that could
		// not be reported is sent again while Spoken Parts lists the id.
		h = &handledCode{req: a.openSealed(k, e)}
		a.mu.Lock()
		a.rememberLocked(e.ID, h)
		a.mu.Unlock()
	}
	a.reportSealed(ctx, s, e.ID, h)
}

func (a *App) rememberLocked(id api.EnvelopeID, h *handledCode) {
	if a.sealed.handled == nil {
		a.sealed.handled = map[api.EnvelopeID]*handledCode{}
	}
	a.sealed.handled[id] = h
	a.sealed.order = append(a.sealed.order, id)
	for len(a.sealed.order) > maxHandled {
		delete(a.sealed.handled, a.sealed.order[0])
		a.sealed.order = a.sealed.order[1:]
	}
}

func failed(reason string) api.CodeResultRequest {
	return api.CodeResultRequest{Result: api.ResultFailed, Reason: reason}
}

// openSealed opens one envelope and saves the code. It never logs the code,
// the shared secret or the AES key (none of them leave codebox.Open except
// the code, which goes to the redactor first).
func (a *App) openSealed(k *codebox.Key, e api.CodeEnvelope) api.CodeResultRequest {
	tail := ssdp.Tail(e.Serial)
	if !sealedSerialRe.MatchString(e.Serial) {
		a.log.Warn("a sealed access code from Spoken Parts named no valid printer")
		return failed(api.ReasonUnknownPrinter)
	}
	scheme := e.Scheme
	if scheme == "" {
		scheme = codebox.Scheme // GET codes may leave it out
	}
	plain, err := k.Open(codebox.Envelope{
		Scheme: scheme, KeyID: e.KeyID, EPK: e.EPK, Nonce: e.Nonce, Ciphertext: e.Ciphertext,
	}, e.Serial)
	if err != nil {
		a.log.Warn("a sealed access code from Spoken Parts could not be opened", "serial_tail", tail)
		return failed(api.ReasonBadEnvelope)
	}
	code := string(plain)
	clear(plain)
	a.red.Add(code) // before anything else touches it
	if !accessCodeRe.MatchString(code) {
		a.log.Warn("a sealed access code from Spoken Parts is not 8 letters or numbers", "serial_tail", tail)
		return failed(api.ReasonFormat)
	}
	switch err := a.SaveCode(e.Serial, code); {
	case err == nil:
		a.log.Info("access code typed on spokenparts.com saved", "serial_tail", tail)
		return api.CodeResultRequest{Result: api.ResultSaved}
	case errors.Is(err, web.MsgAccessFormat):
		return failed(api.ReasonFormat)
	case errors.Is(err, web.MsgNotReady):
		return failed(api.ReasonNotReady)
	case errors.Is(err, web.MsgUnknownPrinter):
		a.log.Info("an access code typed on spokenparts.com is for a printer this link does not see", "serial_tail", tail)
		return failed(api.ReasonUnknownPrinter)
	default:
		return failed(api.ReasonSaveFailed)
	}
}

func (a *App) reportSealed(ctx context.Context, s snapshot, id api.EnvelopeID, h *handledCode) {
	a.mu.Lock()
	req := h.req
	a.mu.Unlock()
	err := a.api.CodeResult(ctx, s.key, s.linkID, id, req)
	var ae *api.Error
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil:
		h.reported = true
	case errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 408 && ae.Status != 429:
		// Gone or refused: asking again would not help.
		h.reported = true
		a.log.Debug("Spoken Parts did not take a sealed access code result", "err", err)
	default:
		// Sent again if Spoken Parts still lists the envelope.
		a.log.Info("could not report a sealed access code result", "err", err)
	}
}

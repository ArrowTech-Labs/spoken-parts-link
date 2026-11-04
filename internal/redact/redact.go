// Package redact removes known secrets from text before it is logged.
//
// Every secret the link handles (printer access codes, the pairing code being
// entered, the server data key, the identity key and the local secret) is
// registered here. Log lines are passed through Redact, which replaces each
// registered value with "[redacted]".
package redact

import (
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// Mask is what a secret is replaced with.
const Mask = "[redacted]"

// MinLen is the shortest value that is registered. Shorter values would
// redact ordinary words and numbers.
const MinLen = 6

// Caps on how many values are kept. Values are only registered after they
// passed validation, so these are far above normal use; they stop growth if
// something goes wrong. The oldest value is dropped first.
const (
	MaxPermanent = 2048 // access codes and key material
	MaxTransient = 16   // pairing codes
)

// Redactor holds the set of secret strings.
type Redactor struct {
	mu        sync.RWMutex
	secrets   map[string]bool // value -> permanent
	permanent []string        // insertion order
	transient []string        // insertion order
	sorted    []string        // longest first
}

// New returns an empty Redactor.
func New() *Redactor {
	return &Redactor{secrets: map[string]bool{}}
}

// Add registers a secret string for good (an access code or key material).
// Values shorter than MinLen are ignored.
func (r *Redactor) Add(s string) { r.add(s, true) }

// AddTransient registers a short lived secret (a pairing code). Only the
// last MaxTransient of these are kept.
func (r *Redactor) AddTransient(s string) { r.add(s, false) }

func (r *Redactor) add(s string, permanent bool) {
	if r == nil || len(s) < MinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if was, ok := r.secrets[s]; ok {
		if permanent && !was {
			r.secrets[s] = true
			r.permanent = append(r.permanent, s)
			r.trimLocked()
		}
		return
	}
	r.secrets[s] = permanent
	if permanent {
		r.permanent = append(r.permanent, s)
	} else {
		r.transient = append(r.transient, s)
	}
	r.trimLocked()
}

func (r *Redactor) trimLocked() {
	for len(r.permanent) > MaxPermanent {
		old := r.permanent[0]
		r.permanent = r.permanent[1:]
		delete(r.secrets, old)
	}
	for len(r.transient) > MaxTransient {
		old := r.transient[0]
		r.transient = r.transient[1:]
		if !r.secrets[old] {
			delete(r.secrets, old)
		}
	}
	r.rebuild()
}

// AddBytes registers raw key material in every text form it could appear in:
// standard and URL base64 (padded and raw) and lower and upper case hex.
func (r *Redactor) AddBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	for _, s := range []string{
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b),
		strings.ToUpper(hex.EncodeToString(b)),
	} {
		r.Add(s)
	}
	if len(b) >= MinLen {
		r.Add(string(b))
	}
}

func (r *Redactor) rebuild() {
	r.sorted = r.sorted[:0]
	for s := range r.secrets {
		r.sorted = append(r.sorted, s)
	}
	sort.Slice(r.sorted, func(i, j int) bool {
		if len(r.sorted[i]) != len(r.sorted[j]) {
			return len(r.sorted[i]) > len(r.sorted[j])
		}
		return r.sorted[i] < r.sorted[j]
	})
}

// Redact returns s with every registered secret replaced by Mask.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.sorted {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, Mask)
		}
	}
	return s
}

// Len reports how many secret strings are registered (for tests).
func (r *Redactor) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.secrets)
}

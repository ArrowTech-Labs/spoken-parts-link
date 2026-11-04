package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
)

// Link status values kept in state.json.
const (
	StatusNone    = ""
	StatusPending = "pending"
	StatusActive  = "active"
)

// State is /data/state/state.json. It holds no secrets.
type State struct {
	LinkID       string    `json:"link_id,omitempty"`
	Status       string    `json:"status"`
	CheckNumber  string    `json:"check_number,omitempty"`
	SiteID       int       `json:"site_id,omitempty"`
	SiteName     string    `json:"site_name,omitempty"`
	CompanyName  string    `json:"company_name,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
	PairedAt     time.Time `json:"paired_at,omitzero"`
	// Notice is a message to show on the page after a state change that
	// happened in the background (for example "removed").
	Notice string `json:"notice,omitempty"`
}

// PrinterMeta is what the link remembers per printer, in
// /data/state/printers.json. It holds no secrets.
type PrinterMeta struct {
	// Pin is the leaf certificate fingerprint the owner chose to trust,
	// "sha256:<64 hex>", or "".
	Pin string `json:"pin,omitempty"`
	// Access is the last answer from the printer for the stored code:
	// "ok", "wrong" or "" (not checked).
	Access string `json:"access,omitempty"`
	// Verified is the fingerprint of the last leaf certificate that passed
	// the Bambu Lab CA check for this serial. Once set, the owner is never
	// offered to trust a different certificate for this serial.
	Verified   string    `json:"verified,omitempty"`
	VerifiedAt time.Time `json:"verified_at,omitzero"`
}

// LoadJSON reads a JSON file into v. A missing file leaves v unchanged and
// returns nil.
func LoadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SaveJSON writes v to path atomically with mode 0600.
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'))
}

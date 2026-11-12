package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"
)

// Label printers (0.5, LABEL-1 CONTRACT "Backend <- link"). The link reports
// what the Spoken Parts Printers integration in Home Assistant has, polls for
// label jobs, claims one, hands it to Home Assistant and reports how it went.
// A job's pages are opaque here: the link never decodes or logs them.

// MaxLabelJob is the largest claimed job answer the link takes (2 MB). A 4x6
// page at 203 dpi is about 170 KB of base64.
const MaxLabelJob = 2 << 20

// CodeNotQueued is the 409 answer of a claim for a job that is no longer
// queued (claimed elsewhere, expired or cancelled).
const CodeNotQueued = "not_queued"

// MaxLabelMessage is the longest message a job state carries.
const MaxLabelMessage = 200

// LabelIntegration says whether Home Assistant has the integration.
type LabelIntegration struct {
	Present bool   `json:"present"`
	Version string `json:"version"`
	Error   string `json:"error"`
}

// LabelPrinter is one printer configured in the integration. The same object
// comes from Home Assistant's list_printers and goes to Spoken Parts.
type LabelPrinter struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Transport string `json:"transport"`
	Address   string `json:"address"`
	State     string `json:"state"`
	LastError string `json:"last_error"`
	Battery   *int   `json:"battery"`
	Media     string `json:"media"`
	LastSeen  string `json:"last_seen"`
}

// LabelReport is the body of POST label-printers.
type LabelReport struct {
	Integration LabelIntegration `json:"integration"`
	Printers    []LabelPrinter   `json:"printers"`
}

// LabelJobRef is one queued job in GET label-jobs.
type LabelJobRef struct {
	ID        string `json:"id"`
	PrinterID string `json:"printer_id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// LabelJob is the answer of POST label-jobs/<id>/claim. Media and Pages are
// kept as the raw JSON Spoken Parts sent, so they go to Home Assistant
// exactly as they came and the page data is never decoded here.
type LabelJob struct {
	ID           string          `json:"id"`
	PrinterID    string          `json:"printer_id"`
	Driver       string          `json:"driver"`
	DocumentType string          `json:"document_type"`
	Format       string          `json:"format"`
	Media        json.RawMessage `json:"media"`
	Copies       json.RawMessage `json:"copies"`
	Pages        json.RawMessage `json:"pages"`
}

// LabelStateRequest is the body of POST label-jobs/<id>/state.
type LabelStateRequest struct {
	State   string `json:"state"` // printing, printed, failed
	Code    string `json:"code"`
	Message string `json:"message"`
	Bytes   int    `json:"bytes"`
}

// ReportLabelPrinters calls POST label-printers.
func (c *Client) ReportLabelPrinters(ctx context.Context, key ed25519.PrivateKey, linkID string, r LabelReport) error {
	if r.Printers == nil {
		r.Printers = []LabelPrinter{}
	}
	if len(r.Printers) > 100 {
		return errors.New("bad label printer report")
	}
	return c.do(ctx, key, linkID, http.MethodPost, "label-printers", r, nil)
}

// ListLabelJobs calls GET label-jobs?state=queued. Jobs with a bad id are
// dropped.
func (c *Client) ListLabelJobs(ctx context.Context, key ed25519.PrivateKey, linkID string) ([]LabelJobRef, error) {
	var out struct {
		Jobs []LabelJobRef `json:"jobs"`
	}
	if err := c.do(ctx, key, linkID, http.MethodGet, "label-jobs?state=queued", nil, &out); err != nil {
		return nil, err
	}
	jobs := out.Jobs[:0]
	for _, j := range out.Jobs {
		if sendIDRe.MatchString(j.ID) {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

// ClaimLabelJob calls POST label-jobs/<id>/claim and returns the job. A job
// that is no longer queued answers 409 not_queued (HasCode CodeNotQueued).
func (c *Client) ClaimLabelJob(ctx context.Context, key ed25519.PrivateKey, linkID, id string) (*LabelJob, error) {
	if !sendIDRe.MatchString(id) {
		return nil, errors.New("bad label job id")
	}
	var out LabelJob
	if err := c.doLimit(ctx, key, linkID, http.MethodPost, "label-jobs/"+id+"/claim", struct{}{}, &out, MaxLabelJob); err != nil {
		return nil, err
	}
	switch {
	case out.ID != id:
		return nil, errors.New("claimed label job answer is for another job")
	case !isJSONArray(out.Pages):
		return nil, errors.New("claimed label job has no pages")
	}
	return &out, nil
}

// LabelJobState calls POST label-jobs/<id>/state. The message is cut to 200
// characters.
func (c *Client) LabelJobState(ctx context.Context, key ed25519.PrivateKey, linkID, id string, req LabelStateRequest) error {
	if !sendIDRe.MatchString(id) {
		return errors.New("bad label job id")
	}
	switch req.State {
	case "printing", "printed", "failed":
	default:
		return errors.New("bad label job state")
	}
	req.Message = Truncate(req.Message, MaxLabelMessage)
	if req.Bytes < 0 {
		req.Bytes = 0
	}
	return c.do(ctx, key, linkID, http.MethodPost, "label-jobs/"+id+"/state", req, nil)
}

func isJSONArray(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) >= 2 && b[0] == '[' && b[len(b)-1] == ']'
}

// Truncate cuts s to at most n characters, on a character boundary.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
)

// 0.7, power protection (CONTRACT 1.2). The link relays between the Spoken
// Parts Power integration in Home Assistant and Spoken Parts.

// PowerSyncRequest is the body of POST power/sync. Circuits and Plugs are
// passed through from the integration's status as they are.
type PowerSyncRequest struct {
	Engine      json.RawMessage `json:"engine"`
	EngineError *string         `json:"engine_error"`
	Circuits    json.RawMessage `json:"circuits"`
	Plugs       json.RawMessage `json:"plugs"`
}

// PowerJob is the value of the print on one printer.
type PowerJob struct {
	Serial     string  `json:"serial"`
	TotalMills *int64  `json:"total_mills"`
	Grams      float64 `json:"grams"`
	Name       string  `json:"name"`
	Basis      string  `json:"basis"`
}

// PowerRequest is a person's request for the engine.
type PowerRequest struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Plug string `json:"plug"`
	By   string `json:"by"`
}

// PowerSyncResponse is the answer of POST power/sync.
type PowerSyncResponse struct {
	ConfigVersion int             `json:"config_version"`
	Config        json.RawMessage `json:"config"`
	Jobs          []PowerJob      `json:"jobs"`
	Requests      []PowerRequest  `json:"requests"`
	NextS         int             `json:"next_s"`
}

// PowerEntry is one audit entry of the engine.
type PowerEntry struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Circuit *string         `json:"circuit"`
	Plug    *string         `json:"plug"`
	Event   *int64          `json:"event"`
	At      string          `json:"at"`
	Data    json.RawMessage `json:"data"`
}

// PowerEventsRequest is the body of POST power/events.
type PowerEventsRequest struct {
	EngineID string       `json:"engine_id"`
	Entries  []PowerEntry `json:"entries"`
}

// PowerEventsResponse is the answer of POST power/events.
type PowerEventsResponse struct {
	StoredThrough int64 `json:"stored_through"`
}

// maxPowerAnswer: the config document of a site with many plugs.
const maxPowerAnswer = 2 << 20

// PowerSync calls POST power/sync.
func (c *Client) PowerSync(ctx context.Context, key ed25519.PrivateKey, linkID string, r PowerSyncRequest) (*PowerSyncResponse, error) {
	if len(r.Circuits) == 0 {
		r.Circuits = json.RawMessage("[]")
	}
	if len(r.Plugs) == 0 {
		r.Plugs = json.RawMessage("[]")
	}
	if len(r.Engine) == 0 {
		r.Engine = json.RawMessage("null")
	}
	var out PowerSyncResponse
	if err := c.doLimit(ctx, key, linkID, http.MethodPost, "power/sync", r, &out, maxPowerAnswer); err != nil {
		return nil, err
	}
	return &out, nil
}

// PowerEvents calls POST power/events (at most 200 entries).
func (c *Client) PowerEvents(ctx context.Context, key ed25519.PrivateKey, linkID string, r PowerEventsRequest) (*PowerEventsResponse, error) {
	if len(r.Entries) == 0 || len(r.Entries) > 200 || r.EngineID == "" {
		return nil, errors.New("bad power events batch")
	}
	var out PowerEventsResponse
	if err := c.do(ctx, key, linkID, http.MethodPost, "power/events", r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

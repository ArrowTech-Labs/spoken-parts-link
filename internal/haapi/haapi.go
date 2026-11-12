// Package haapi calls Home Assistant services through the Supervisor (0.5,
// label printers). The app has the homeassistant_api permission, so the
// Supervisor gives it SUPERVISOR_TOKEN and proxies http://supervisor/core/api
// to Home Assistant's REST API.
//
// That token can do anything Home Assistant's REST API can, so this client
// only calls the services in AllowedServices, of the one Spoken Parts
// Printers domain, and refuses anything else before a request is made. A
// repository test checks that no other code reads the token or names the
// Supervisor's address.
//
// The token goes only in the Authorization header. It is never logged and
// never part of an error.
package haapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBase is Home Assistant's REST API through the Supervisor.
const DefaultBase = "http://supervisor/core/api"

// TokenEnv is the environment variable the Supervisor sets for an app with
// the homeassistant_api permission.
const TokenEnv = "SUPERVISOR_TOKEN"

// Domain is the Spoken Parts Printers integration.
const Domain = "spoken_parts_printers"

// AllowedServices are the only services the link ever calls.
var AllowedServices = [...]string{"list_printers", "print_raster"}

// maxAnswer caps Home Assistant's answer (list_printers is a few KB).
const maxAnswer = 1 << 20

// Errors. Code turns any error of CallService into a short code.
var (
	// ErrNoToken: the app runs without SUPERVISOR_TOKEN (no
	// homeassistant_api permission, or outside Home Assistant).
	ErrNoToken = errors.New("no Home Assistant access token")
	// ErrNotInstalled: Home Assistant does not have the service.
	ErrNotInstalled = errors.New("the Spoken Parts Printers integration is not installed in Home Assistant")
	// ErrTimeout: Home Assistant did not answer in time.
	ErrTimeout = errors.New("Home Assistant did not answer in time")
	// ErrUnreachable: the request did not get an answer (refused, reset).
	ErrUnreachable = errors.New("could not reach Home Assistant")
	// ErrNotAllowed: a call outside AllowedServices.
	ErrNotAllowed  = errors.New("refused: the link calls only the Spoken Parts Printers services")
	errNoResponse  = errors.New("Home Assistant answered without a service response")
	errBadAnswer   = errors.New("Home Assistant's answer was not valid JSON")
	errAnswerLarge = errors.New("Home Assistant's answer was too large")
)

// Error is an HTTP answer other than 200 that does not mean "not installed".
type Error struct {
	Status int
	// Message is Home Assistant's own message ({"message": ...}), cut to
	// 200 characters. It never holds the token.
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Home Assistant answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("Home Assistant answered %d", e.Status)
}

// Code is a short code for err: no_token, not_installed, timeout,
// ha_unreachable, http_<n>, bad_answer, refused or "" for nil.
func Code(err error) string {
	var he *Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoToken):
		return "no_token"
	case errors.Is(err, ErrNotInstalled):
		return "not_installed"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrUnreachable):
		return "ha_unreachable"
	case errors.As(err, &he):
		return fmt.Sprintf("http_%d", he.Status)
	case errors.Is(err, ErrNotAllowed):
		return "refused"
	}
	return "bad_answer"
}

// Client calls Home Assistant services.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for base (DefaultBase in production) with the
// Supervisor token. An empty token makes every call ErrNoToken.
func New(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: strings.TrimSpace(token),
		http: &http.Client{
			// No overall timeout: each call's context has one (a print waits
			// up to 60 s for the printer).
			Transport: &http.Transport{
				Proxy:               nil, // the Supervisor is local; never a proxy
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConns:        2,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// HasToken reports whether the client has a token.
func (c *Client) HasToken() bool { return c.token != "" }

func allowed(domain, service string) bool {
	if domain != Domain {
		return false
	}
	for _, s := range AllowedServices {
		if s == service {
			return true
		}
	}
	return false
}

// CallService posts data to /services/<domain>/<service>?return_response and
// returns the service_response. Errors: ErrNoToken, ErrNotInstalled (404, or
// 400 for a service Home Assistant does not have), ErrTimeout (ctx deadline),
// ErrUnreachable, *Error, or a bad answer. None names the token.
func (c *Client) CallService(ctx context.Context, domain, service string, data any) (json.RawMessage, error) {
	if !allowed(domain, service) {
		return nil, ErrNotAllowed
	}
	if c.token == "" {
		return nil, ErrNoToken
	}
	if data == nil {
		data = struct{}{}
	}
	body, err := json.Marshal(data)
	if err != nil {
		return nil, errors.New("could not build the service call")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/services/"+domain+"/"+service+"?return_response", bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("could not build the service call")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return nil, ErrTimeout
		}
		return nil, ErrUnreachable
	}
	defer resp.Body.Close()
	ans, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return nil, ErrTimeout
		}
		return nil, ErrUnreachable
	}
	if len(ans) > maxAnswer {
		return nil, errAnswerLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, answerError(resp.StatusCode, ans)
	}
	var out struct {
		ServiceResponse json.RawMessage `json:"service_response"`
	}
	if json.Unmarshal(ans, &out) != nil {
		return nil, errBadAnswer
	}
	if r := bytes.TrimSpace(out.ServiceResponse); len(r) == 0 || bytes.Equal(r, []byte("null")) {
		return nil, errNoResponse
	}
	return out.ServiceResponse, nil
}

// answerError reads a non 200 answer. Home Assistant answers a call to a
// service it does not have with 400 and aiohttp's plain "400: Bad Request"
// body (HTTPBadRequest from ServiceNotFound), and its own errors with JSON
// {"message": ...}. So a 400 is "not installed" when its message says the
// service was not found, or when it carries no JSON message at all. A 404
// is "not installed" too (the contract; older Home Assistant).
func answerError(status int, ans []byte) error {
	var m struct {
		Message string `json:"message"`
	}
	hasMsg := json.Unmarshal(ans, &m) == nil && m.Message != ""
	msg := strings.ToLower(m.Message)
	switch {
	case status == http.StatusNotFound:
		return ErrNotInstalled
	case status == http.StatusBadRequest && (!hasMsg || (strings.Contains(msg, "service") && strings.Contains(msg, "not found"))):
		return ErrNotInstalled
	}
	e := &Error{Status: status}
	if hasMsg {
		e.Message = cut(m.Message, 200)
	}
	return e
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue) && ue.Timeout()
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

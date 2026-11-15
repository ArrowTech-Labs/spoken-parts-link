package haapi

// 0.7, power protection: a small websocket client (RFC 6455, standard
// library only) for Home Assistant's own websocket API through the
// Supervisor, used to talk to the Spoken Parts Power integration.
//
// Like CallService, it refuses anything outside AllowedCommands before a
// byte is sent: the link can only send the spoken_parts_power/* commands. It
// never calls a Home Assistant action (no call_service), so it can never
// turn a plug on or off itself. The Spoken Parts Power integration does
// that, inside Home Assistant, on the plugs of configured circuits.
//
// The token goes only in the auth message. It is never logged and never
// part of an error.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultWSURL is Home Assistant's websocket API through the Supervisor.
const DefaultWSURL = "ws://supervisor/core/websocket"

// PowerDomain is the Spoken Parts Power integration.
const PowerDomain = "spoken_parts_power"

// AllowedCommands are the only websocket commands the link ever sends.
var AllowedCommands = [...]string{"spoken_parts_power/hello", "spoken_parts_power/status", "spoken_parts_power/feed", "spoken_parts_power/config", "spoken_parts_power/command", "spoken_parts_power/entries", "spoken_parts_power/ack"}

// maxFrame caps one websocket message from Home Assistant.
const maxFrame = 4 << 20

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Websocket errors.
var (
	ErrAuth          = errors.New("Home Assistant did not accept the app's token")
	ErrCmdNotAllowed = errors.New("refused: the link sends only the Spoken Parts Power commands")
	errHandshake     = errors.New("Home Assistant's websocket did not open")
	errClosed        = errors.New("Home Assistant closed the websocket")
	errFrameLarge    = errors.New("Home Assistant's websocket message was too large")
)

// WS is one websocket to Home Assistant. Calls are one at a time.
type WS struct {
	url   string
	token string
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
	id   int
}

// NewWS returns a client for wsURL (DefaultWSURL in production). An empty
// token makes every call ErrNoToken.
func NewWS(wsURL, token string) *WS {
	return &WS{url: wsURL, token: strings.TrimSpace(token), dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
}

// HasToken reports whether the client has a token.
func (w *WS) HasToken() bool { return w.token != "" }

func commandAllowed(typ string) bool {
	for _, c := range AllowedCommands {
		if c == typ {
			return true
		}
	}
	return false
}

// Close drops the connection (the next call dials again).
func (w *WS) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeLocked()
}

func (w *WS) closeLocked() {
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn, w.br = nil, nil
	}
}

// Call sends one command and returns its result. Errors: ErrCmdNotAllowed,
// ErrNoToken, ErrAuth, ErrNotInstalled (the integration is not set up),
// ErrTimeout, ErrUnreachable, *Error.
func (w *WS) Call(ctx context.Context, typ string, data map[string]any) (json.RawMessage, error) {
	if !commandAllowed(typ) {
		return nil, ErrCmdNotAllowed
	}
	if w.token == "" {
		return nil, ErrNoToken
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		if err := w.connectLocked(ctx); err != nil {
			w.closeLocked()
			return nil, err
		}
	}
	res, err := w.callLocked(ctx, typ, data)
	if err != nil {
		var he *Error
		if !errors.Is(err, ErrNotInstalled) && !errors.As(err, &he) {
			w.closeLocked()
		}
	}
	return res, err
}

func (w *WS) deadline(ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		_ = w.conn.SetDeadline(dl)
	} else {
		_ = w.conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
}

func (w *WS) connectLocked(ctx context.Context) error {
	u, err := url.Parse(w.url)
	if err != nil || u.Scheme != "ws" || u.Host == "" {
		return errors.New("bad Home Assistant websocket address")
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := w.dial(ctx, "tcp", host)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return ErrTimeout
		}
		return ErrUnreachable
	}
	w.conn, w.br = conn, bufio.NewReaderSize(conn, 32<<10)
	w.deadline(ctx)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: http.Header{}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		return w.netErr(ctx)
	}
	resp, err := http.ReadResponse(w.br, req)
	if err != nil {
		return w.netErr(ctx)
	}
	_ = resp.Body.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return errHandshake
	}
	var m struct {
		Type string `json:"type"`
	}
	if err := w.readJSON(ctx, &m); err != nil {
		return err
	}
	if m.Type != "auth_required" {
		return errHandshake
	}
	if err := w.writeJSON(ctx, map[string]string{"type": "auth", "access_token": w.token}); err != nil {
		return err
	}
	if err := w.readJSON(ctx, &m); err != nil {
		return err
	}
	if m.Type != "auth_ok" {
		return ErrAuth
	}
	return nil
}

func (w *WS) netErr(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrTimeout
	}
	return ErrUnreachable
}

func (w *WS) callLocked(ctx context.Context, typ string, data map[string]any) (json.RawMessage, error) {
	w.deadline(ctx)
	w.id++
	id := w.id
	msg := map[string]any{}
	for k, v := range data {
		if k != "id" && k != "type" {
			msg[k] = v
		}
	}
	msg["id"], msg["type"] = id, typ
	if err := w.writeJSON(ctx, msg); err != nil {
		return nil, err
	}
	for {
		var ans struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := w.readJSON(ctx, &ans); err != nil {
			return nil, err
		}
		if ans.ID != id || ans.Type != "result" {
			continue // an answer to something else (none expected)
		}
		if ans.Success {
			return ans.Result, nil
		}
		if ans.Error != nil && (ans.Error.Code == "unknown_command" || ans.Error.Code == "not_ready") {
			return nil, ErrNotInstalled
		}
		e := &Error{Status: 0}
		if ans.Error != nil {
			e.Message = cut(ans.Error.Code+": "+ans.Error.Message, 200)
		}
		return nil, e
	}
}

func (w *WS) writeJSON(ctx context.Context, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return errors.New("could not build the websocket message")
	}
	if err := w.writeFrame(0x1, body); err != nil {
		return w.netErr(ctx)
	}
	return nil
}

// writeFrame sends one final, masked frame (clients always mask).
func (w *WS) writeFrame(op byte, payload []byte) error {
	var hdr [14]byte
	hdr[0] = 0x80 | op
	n := 2
	switch l := len(payload); {
	case l < 126:
		hdr[1] = 0x80 | byte(l)
	case l <= 0xFFFF:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(l))
		n = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(l))
		n = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(hdr[n:], mask[:])
	n += 4
	buf := make([]byte, n+len(payload))
	copy(buf, hdr[:n])
	for i, b := range payload {
		buf[n+i] = b ^ mask[i%4]
	}
	_, err := w.conn.Write(buf)
	return err
}

// readJSON reads one whole message (joining continuation frames, answering
// pings) and decodes it.
func (w *WS) readJSON(ctx context.Context, v any) error {
	var msg []byte
	for {
		fin, op, payload, err := w.readFrame()
		if err != nil {
			if errors.Is(err, errFrameLarge) || errors.Is(err, errClosed) {
				return err
			}
			return w.netErr(ctx)
		}
		switch op {
		case 0x8:
			return errClosed
		case 0x9:
			if err := w.writeFrame(0xA, payload); err != nil {
				return w.netErr(ctx)
			}
			continue
		case 0xA:
			continue
		}
		msg = append(msg, payload...)
		if len(msg) > maxFrame {
			return errFrameLarge
		}
		if fin {
			break
		}
	}
	if err := json.Unmarshal(msg, v); err != nil {
		return errBadAnswer
	}
	return nil
}

func (w *WS) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(w.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op := h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	l := uint64(h[1] & 0x7F)
	switch l {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		l = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		l = binary.BigEndian.Uint64(b[:])
	}
	if l > maxFrame {
		return false, 0, nil, errFrameLarge
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, l)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

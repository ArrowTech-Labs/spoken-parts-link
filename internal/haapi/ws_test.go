package haapi

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsFakeHA is a minimal Home Assistant websocket server: auth, then results.
type wsFakeHA struct {
	ln       net.Listener
	token    string
	mu       sync.Mutex
	got      []map[string]any
	answer   func(msg map[string]any) map[string]any
	pingOnce bool
}

func newWSFakeHA(t *testing.T, token string) *wsFakeHA {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &wsFakeHA{ln: ln, token: token}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *wsFakeHA) url() string { return "ws://" + f.ln.Addr().String() + "/core/websocket" }

func (f *wsFakeHA) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.conn(c)
	}
}

func srvRead(br *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, nil, err
	}
	l := uint64(h[1] & 0x7F)
	if l == 126 {
		var b [2]byte
		_, _ = io.ReadFull(br, b[:])
		l = uint64(binary.BigEndian.Uint16(b[:]))
	} else if l == 127 {
		var b [8]byte
		_, _ = io.ReadFull(br, b[:])
		l = binary.BigEndian.Uint64(b[:])
	}
	var m [4]byte
	if h[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame not masked")
	}
	_, _ = io.ReadFull(br, m[:])
	p := make([]byte, l)
	if _, err := io.ReadFull(br, p); err != nil {
		return 0, nil, err
	}
	for i := range p {
		p[i] ^= m[i%4]
	}
	return h[0] & 0x0F, p, nil
}

func srvWrite(c net.Conn, op byte, p []byte) {
	hdr := []byte{0x80 | op}
	switch {
	case len(p) < 126:
		hdr = append(hdr, byte(len(p)))
	case len(p) <= 0xFFFF:
		hdr = append(hdr, 126, byte(len(p)>>8), byte(len(p)))
	default:
		hdr = append(hdr, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(len(p)))
		hdr = append(hdr, b[:]...)
	}
	_, _ = c.Write(append(hdr, p...))
}

func (f *wsFakeHA) conn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsGUID))
	_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
		base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	srvWrite(c, 1, []byte(`{"type":"auth_required","ha_version":"2026.11.1"}`))
	_, p, err := srvRead(br)
	if err != nil {
		return
	}
	var auth map[string]string
	_ = json.Unmarshal(p, &auth)
	if auth["access_token"] != f.token {
		srvWrite(c, 1, []byte(`{"type":"auth_invalid"}`))
		return
	}
	srvWrite(c, 1, []byte(`{"type":"auth_ok"}`))
	for {
		op, p, err := srvRead(br)
		if err != nil || op == 8 {
			return
		}
		var msg map[string]any
		_ = json.Unmarshal(p, &msg)
		f.mu.Lock()
		f.got = append(f.got, msg)
		ping := f.pingOnce
		f.pingOnce = false
		f.mu.Unlock()
		if ping {
			srvWrite(c, 9, []byte("hi"))
			if op2, _, err := srvRead(br); err != nil || op2 != 0xA {
				return
			}
		}
		ans := f.answer(msg)
		ans["id"] = msg["id"]
		ans["type"] = "result"
		b, _ := json.Marshal(ans)
		// Send big answers in two frames to exercise continuation.
		if len(b) > 1000 {
			c.Write([]byte{0x01, 126, byte(500 >> 8), byte(500 & 0xff)})
			c.Write(b[:500])
			srvWrite(c, 0, b[500:])
			continue
		}
		srvWrite(c, 1, b)
	}
}

func TestWSCallAndAllowList(t *testing.T) {
	f := newWSFakeHA(t, "tok")
	f.answer = func(msg map[string]any) map[string]any {
		if msg["type"] == "spoken_parts_power/status" {
			return map[string]any{"success": true, "result": map[string]any{"pad": strings.Repeat("x", 3000)}}
		}
		return map[string]any{"success": true, "result": map[string]any{"echo": msg["type"]}}
	}
	ws := NewWS(f.url(), "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := ws.Call(ctx, "spoken_parts_power/hello", nil)
	if err != nil || !strings.Contains(string(res), "spoken_parts_power/hello") {
		t.Fatalf("hello: %s %v", res, err)
	}
	f.pingOnce = true
	res, err = ws.Call(ctx, "spoken_parts_power/status", map[string]any{"type": "evil", "id": 99})
	if err != nil || len(res) < 3000 {
		t.Fatalf("status: %d %v", len(res), err)
	}
	f.mu.Lock()
	if f.got[1]["type"] != "spoken_parts_power/status" || f.got[1]["id"].(float64) == 99 {
		t.Fatalf("data may not override type or id: %v", f.got[1])
	}
	f.mu.Unlock()
	for _, bad := range []string{"call_service", "spoken_parts_power/../call_service", "config/auth/list", "subscribe_events"} {
		if _, err := ws.Call(ctx, bad, map[string]any{"domain": "switch", "service": "turn_off"}); !errors.Is(err, ErrCmdNotAllowed) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	f.mu.Lock()
	n := len(f.got)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("a refused command reached Home Assistant: %d messages", n)
	}
}

func TestWSErrors(t *testing.T) {
	f := newWSFakeHA(t, "tok")
	f.answer = func(msg map[string]any) map[string]any {
		return map[string]any{"success": false, "error": map[string]any{"code": "unknown_command", "message": "Unknown command."}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewWS(f.url(), "").Call(ctx, "spoken_parts_power/hello", nil); !errors.Is(err, ErrNoToken) {
		t.Fatal(err)
	}
	if _, err := NewWS(f.url(), "wrong").Call(ctx, "spoken_parts_power/hello", nil); !errors.Is(err, ErrAuth) {
		t.Fatalf("auth: %v", err)
	}
	ws := NewWS(f.url(), "tok")
	if _, err := ws.Call(ctx, "spoken_parts_power/hello", nil); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	if _, err := NewWS("ws://127.0.0.1:1/core/websocket", "tok").Call(ctx, "spoken_parts_power/hello", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unreachable: %v", err)
	}
	if err := (&Error{Message: "x"}); strings.Contains(err.Error(), "tok") {
		t.Fatal("token in error")
	}
}

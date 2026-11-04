// Package mqtt is a minimal MQTT 3.1.1 client for reading Bambu Lab printer
// status.
//
// It supports CONNECT/CONNACK, SUBSCRIBE/SUBACK, incoming PUBLISH,
// PINGREQ/PINGRESP and DISCONNECT. On purpose it has no general publish
// function: the only message it can ever send to a printer is the constant
// "pushall" status request, through RequestPushAll. Tests enforce this.
package mqtt

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

// PushAllPayload is the one and only payload this package can publish.
const PushAllPayload = `{"pushing":{"sequence_id":"0","command":"pushall","version":1,"push_target":1}}`

// Packet types (high nibble of the fixed header).
const (
	typeConnect    = 1
	typeConnack    = 2
	typePublish    = 3
	typePuback     = 4
	typeSubscribe  = 8
	typeSuback     = 9
	typePingreq    = 12
	typePingresp   = 13
	typeDisconnect = 14
)

// DefaultMaxPacket bounds memory per incoming packet. Bambu H2D reports are
// large, so this is generous.
const DefaultMaxPacket = 4 << 20

var serialRe = regexp.MustCompile(`^[A-Za-z0-9]{6,40}$`)

// ConnackError is a refused CONNECT. Code is the CONNACK return code.
type ConnackError struct{ Code byte }

func (e *ConnackError) Error() string {
	return fmt.Sprintf("printer refused the connection (code %d)", e.Code)
}

// BadCredentials reports whether the printer rejected the user name or
// password (return codes 4 and 5).
func (e *ConnackError) BadCredentials() bool { return e.Code == 4 || e.Code == 5 }

// ErrWatchdog means nothing arrived from the printer for too long.
var ErrWatchdog = errors.New("no data from the printer")

// Options for Connect.
type Options struct {
	Username  string
	Password  string
	KeepAlive time.Duration // PINGREQ interval, default 30 s
	Watchdog  time.Duration // max silence, default 60 s
	Timeout   time.Duration // for CONNACK and SUBACK, default 15 s
	MaxPacket int
}

// Client is one MQTT session with one printer.
type Client struct {
	conn      net.Conn
	r         *bufio.Reader
	serial    string
	keepAlive time.Duration
	watchdog  time.Duration
	maxPacket int

	wmu     sync.Mutex
	closed  bool
	pushed  bool
	closeMu sync.Once
}

// Connect runs CONNECT and SUBSCRIBE device/<serial>/report over conn, which
// must already be an authenticated TLS connection. The password is written
// only inside the CONNECT packet and never appears in an error.
func Connect(conn net.Conn, serial string, opts Options) (*Client, error) {
	if !serialRe.MatchString(serial) {
		return nil, errors.New("bad serial")
	}
	if opts.KeepAlive <= 0 {
		opts.KeepAlive = 30 * time.Second
	}
	if opts.Watchdog <= 0 {
		opts.Watchdog = 60 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.MaxPacket <= 0 {
		opts.MaxPacket = DefaultMaxPacket
	}
	c := &Client{
		conn:      conn,
		r:         bufio.NewReaderSize(conn, 16<<10),
		serial:    serial,
		keepAlive: opts.KeepAlive,
		watchdog:  opts.Watchdog,
		maxPacket: opts.MaxPacket,
	}
	deadline := time.Now().Add(opts.Timeout)
	_ = conn.SetDeadline(deadline)

	pkt := connectPacket(clientID(), opts.Username, opts.Password, opts.KeepAlive)
	_, err := conn.Write(pkt)
	clear(pkt)
	if err != nil {
		return nil, fmt.Errorf("sending connect: %w", err)
	}
	typ, body, err := readPacket(c.r, 64)
	if err != nil {
		return nil, fmt.Errorf("waiting for connack: %w", err)
	}
	if typ>>4 != typeConnack || len(body) != 2 {
		return nil, errors.New("printer did not answer with a connack")
	}
	if body[1] != 0 {
		return nil, &ConnackError{Code: body[1]}
	}

	if _, err := conn.Write(subscribePacket(1, reportTopic(serial))); err != nil {
		return nil, fmt.Errorf("sending subscribe: %w", err)
	}
	for {
		typ, body, err := readPacket(c.r, c.maxPacket)
		if err != nil {
			return nil, fmt.Errorf("waiting for suback: %w", err)
		}
		if typ>>4 != typeSuback {
			// A broker may send nothing else here; skip anything odd.
			continue
		}
		if len(body) < 3 || body[0] != 0 || body[1] != 1 {
			return nil, errors.New("bad suback")
		}
		if body[2] == 0x80 {
			return nil, errors.New("printer refused the report subscription")
		}
		break
	}
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

func reportTopic(serial string) string  { return "device/" + serial + "/report" }
func requestTopic(serial string) string { return "device/" + serial + "/request" }

func clientID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "spoken-parts-link-" + hex.EncodeToString(b)
}

// RequestPushAll sends the constant pushall request to
// device/<serial>/request. It works once per Client; later calls are
// ignored. The caller also limits it to once per 5 minutes per printer.
func (c *Client) RequestPushAll() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if c.pushed {
		return nil
	}
	c.pushed = true
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(pushAllPacket(c.serial))
	return err
}

// Run reads packets until ctx is done, the connection fails or the watchdog
// fires. It sends PINGREQ every KeepAlive. onReport gets the payload of every
// message on device/<serial>/report.
func (c *Client) Run(ctx context.Context, onReport func(payload []byte)) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(c.keepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				c.Close()
				return
			case <-done:
				return
			case <-t.C:
				if err := c.write([]byte{typePingreq << 4, 0}); err != nil {
					_ = c.conn.Close()
					return
				}
			}
		}
	}()
	want := reportTopic(c.serial)
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.watchdog))
		typ, body, err := readPacket(c.r, c.maxPacket)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return ErrWatchdog
			}
			return err
		}
		switch typ >> 4 {
		case typePublish:
			topic, payload, id, err := parsePublish(typ, body)
			if err != nil {
				return err
			}
			if id != 0 {
				// QoS 1: acknowledge. PUBACK is not a publish.
				if err := c.write([]byte{typePuback << 4, 2, byte(id >> 8), byte(id)}); err != nil {
					return err
				}
			}
			if topic == want && onReport != nil {
				onReport(payload)
			}
		case typePingresp, typeSuback:
		default:
			// Ignore anything else.
		}
	}
}

func (c *Client) write(p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(p)
	return err
}

// Close sends DISCONNECT and closes the connection.
func (c *Client) Close() {
	c.closeMu.Do(func() {
		c.wmu.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.conn.Write([]byte{typeDisconnect << 4, 0})
		c.closed = true
		c.wmu.Unlock()
		_ = c.conn.Close()
	})
}

// ---- encoding ----

func appendRemaining(b []byte, n int) []byte {
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		b = append(b, d)
		if n == 0 {
			return b
		}
	}
}

func appendString(b []byte, s string) []byte {
	b = append(b, byte(len(s)>>8), byte(len(s)))
	return append(b, s...)
}

func connectPacket(clientID, user, pass string, keepAlive time.Duration) []byte {
	var vh []byte
	vh = appendString(vh, "MQTT")
	vh = append(vh, 4)  // protocol level 3.1.1
	flags := byte(0x02) // clean session
	if user != "" {
		flags |= 0x80
	}
	if pass != "" {
		flags |= 0x40
	}
	vh = append(vh, flags)
	ka := int(keepAlive / time.Second)
	vh = append(vh, byte(ka>>8), byte(ka))
	vh = appendString(vh, clientID)
	if user != "" {
		vh = appendString(vh, user)
	}
	if pass != "" {
		vh = appendString(vh, pass)
	}
	out := []byte{typeConnect << 4}
	out = appendRemaining(out, len(vh))
	out = append(out, vh...)
	clear(vh)
	return out
}

func subscribePacket(id uint16, topic string) []byte {
	var body []byte
	body = append(body, byte(id>>8), byte(id))
	body = appendString(body, topic)
	body = append(body, 0) // QoS 0
	out := []byte{typeSubscribe<<4 | 0x02}
	out = appendRemaining(out, len(body))
	return append(out, body...)
}

// pushAllPacket is the only PUBLISH encoder in this package. It takes no
// payload argument: the payload is always PushAllPayload, QoS 0, no retain.
func pushAllPacket(serial string) []byte {
	var body []byte
	body = appendString(body, requestTopic(serial))
	body = append(body, PushAllPayload...)
	out := []byte{typePublish << 4}
	out = appendRemaining(out, len(body))
	return append(out, body...)
}

// ---- decoding ----

func readPacket(r *bufio.Reader, max int) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; ; i++ {
		if i == 4 {
			return 0, nil, errors.New("bad remaining length")
		}
		d, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(d&0x7f) * mult
		if d&0x80 == 0 {
			break
		}
		mult *= 128
	}
	if n > max {
		return 0, nil, fmt.Errorf("packet of %d bytes is too large", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func parsePublish(typ byte, body []byte) (topic string, payload []byte, id uint16, err error) {
	if len(body) < 2 {
		return "", nil, 0, errors.New("short publish")
	}
	tl := int(body[0])<<8 | int(body[1])
	if len(body) < 2+tl {
		return "", nil, 0, errors.New("short publish topic")
	}
	topic = string(body[2 : 2+tl])
	rest := body[2+tl:]
	qos := (typ >> 1) & 0x03
	if qos > 0 {
		if len(rest) < 2 {
			return "", nil, 0, errors.New("short publish id")
		}
		id = uint16(rest[0])<<8 | uint16(rest[1])
		rest = rest[2:]
		if qos == 2 {
			// We subscribe with QoS 0, so a broker must never send QoS 2.
			return "", nil, 0, errors.New("unexpected QoS 2 publish")
		}
	}
	return topic, rest, id, nil
}

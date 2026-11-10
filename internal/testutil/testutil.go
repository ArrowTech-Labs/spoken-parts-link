// Package testutil has helpers for tests only: a certificate authority made
// at test runtime and a fake printer MQTT broker. No key material is ever
// stored in the repository; everything here is generated when a test runs.
package testutil

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// CA is a throwaway certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
}

// NewCA makes a self signed CA named cn.
func NewCA(t testing.TB, cn string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Test CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Leaf issues a server certificate with common name cn (a printer serial).
func (ca *CA) Leaf(t testing.TB, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sn, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// Publish is one PUBLISH packet the broker received.
type Publish struct {
	Topic   string
	Payload string
}

// Connect is one CONNECT packet the broker received.
type Connect struct {
	ClientID string
	Username string
	Password string
}

// Broker is a fake Bambu printer MQTT broker over TLS 1.2.
type Broker struct {
	Addr   string
	Serial string

	ln   net.Listener
	code string

	mu         sync.Mutex
	cert       tls.Certificate
	handshakes int
	connects   []Connect
	publishes  []Publish
	reports    [][]byte
	conns      []net.Conn
	writers    []func([]byte)
	mode       string // "", "cloud" or "developer": how project_file is answered
	walkDelay  time.Duration
	wg         sync.WaitGroup
}

// SetMode sets how the fake printer answers project_file: "cloud" refuses it
// like a printer that needs Bambu's authorization, "developer" accepts it and
// walks PREPARE, RUNNING (3 layers) and FINISH.
func (b *Broker) SetMode(mode string, step time.Duration) {
	b.mu.Lock()
	b.mode, b.walkDelay = mode, step
	b.mu.Unlock()
}

// Push sends a report payload to every connected, subscribed client.
func (b *Broker) Push(payload []byte) {
	b.mu.Lock()
	ws := make([]func([]byte), len(b.writers))
	copy(ws, b.writers)
	b.mu.Unlock()
	pkt := encodePublish("device/"+b.Serial+"/report", payload)
	for _, w := range ws {
		w(pkt)
	}
}

func (b *Broker) answerProjectFile(payload string) {
	b.mu.Lock()
	mode, step := b.mode, b.walkDelay
	b.mu.Unlock()
	switch mode {
	case "cloud":
		b.Push([]byte(`{"print":{"command":"project_file","sequence_id":"1","result":"failed","reason":"mqtt message verify failed","err_code":84033543}}`))
	case "developer":
		var msg struct {
			Print struct {
				Subtask string `json:"subtask_name"`
			} `json:"print"`
		}
		_ = json.Unmarshal([]byte(payload), &msg)
		name := msg.Print.Subtask
		b.Push([]byte(`{"print":{"command":"project_file","sequence_id":"1","result":"success"}}`))
		go func() {
			walk := []string{
				`{"print":{"gcode_state":"PREPARE","subtask_name":"` + name + `","mc_percent":0,"layer_num":0,"total_layer_num":3}}`,
				`{"print":{"gcode_state":"RUNNING","subtask_name":"` + name + `","mc_percent":33,"layer_num":1,"total_layer_num":3}}`,
				`{"print":{"gcode_state":"RUNNING","subtask_name":"` + name + `","mc_percent":66,"layer_num":2,"total_layer_num":3}}`,
				`{"print":{"gcode_state":"FINISH","subtask_name":"` + name + `","mc_percent":100,"layer_num":3,"total_layer_num":3}}`,
			}
			for _, r := range walk {
				time.Sleep(step)
				b.Push([]byte(r))
			}
		}()
	}
}

// NewBroker listens on 127.0.0.1. code is the access code it accepts.
// After a client publishes anything, the broker sends each report payload on
// device/<serial>/report.
func NewBroker(t testing.TB, cert tls.Certificate, serial, code string, reports ...[]byte) *Broker {
	t.Helper()
	b := &Broker{Serial: serial, code: code, cert: cert, reports: reports}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			c := b.cert
			return &c, nil
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.ln = ln
	b.Addr = ln.Addr().String()
	b.wg.Add(1)
	go b.accept()
	t.Cleanup(b.Close)
	return b
}

// SetCert changes the certificate for new connections.
func (b *Broker) SetCert(c tls.Certificate) {
	b.mu.Lock()
	b.cert = c
	b.mu.Unlock()
}

// SetCode changes the accepted access code.
func (b *Broker) SetCode(code string) {
	b.mu.Lock()
	b.code = code
	b.mu.Unlock()
}

// Close stops the broker and drops all connections.
func (b *Broker) Close() {
	_ = b.ln.Close()
	b.DropAll()
	b.wg.Wait()
}

// DropAll closes every open client connection.
func (b *Broker) DropAll() {
	b.mu.Lock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
	b.writers = nil
	b.mu.Unlock()
}

// SetReports changes the reports sent after the next pushall.
func (b *Broker) SetReports(reports ...[]byte) {
	b.mu.Lock()
	b.reports = reports
	b.mu.Unlock()
}

// Connects returns every CONNECT received.
func (b *Broker) Connects() []Connect {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Connect(nil), b.connects...)
}

// Publishes returns every PUBLISH received.
func (b *Broker) Publishes() []Publish {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Publish(nil), b.publishes...)
}

// Handshakes counts completed TLS handshakes.
func (b *Broker) Handshakes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handshakes
}

func (b *Broker) accept() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, c)
		b.mu.Unlock()
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer c.Close()
			b.serve(c.(*tls.Conn))
		}()
	}
}

func (b *Broker) serve(c *tls.Conn) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		return
	}
	b.mu.Lock()
	b.handshakes++
	b.mu.Unlock()
	_ = c.SetDeadline(time.Time{})
	r := bufio.NewReader(c)
	var wmu sync.Mutex
	write := func(p []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(p)
	}
	for {
		typ, body, err := readPacket(r)
		if err != nil {
			return
		}
		switch typ >> 4 {
		case 1: // CONNECT
			cn, err := parseConnect(body)
			if err != nil {
				return
			}
			b.mu.Lock()
			b.connects = append(b.connects, cn)
			ok := cn.Username == "bblp" && cn.Password == b.code
			b.mu.Unlock()
			if !ok {
				write([]byte{0x20, 2, 0, 5})
				return
			}
			write([]byte{0x20, 2, 0, 0})
		case 8: // SUBSCRIBE
			if len(body) < 2 {
				return
			}
			write([]byte{0x90, 3, body[0], body[1], 0})
			b.mu.Lock()
			b.writers = append(b.writers, write)
			b.mu.Unlock()
		case 3: // PUBLISH
			if len(body) < 2 {
				return
			}
			tl := int(body[0])<<8 | int(body[1])
			if len(body) < 2+tl {
				return
			}
			p := Publish{Topic: string(body[2 : 2+tl]), Payload: string(body[2+tl:])}
			b.mu.Lock()
			b.publishes = append(b.publishes, p)
			reports := b.reports
			b.mu.Unlock()
			if strings.Contains(p.Payload, `"command":"project_file"`) {
				b.answerProjectFile(p.Payload)
				continue
			}
			for _, rep := range reports {
				write(encodePublish("device/"+b.Serial+"/report", rep))
			}
		case 12: // PINGREQ
			write([]byte{0xD0, 0})
		case 14: // DISCONNECT
			return
		}
	}
}

func encodePublish(topic string, payload []byte) []byte {
	body := []byte{byte(len(topic) >> 8), byte(len(topic))}
	body = append(body, topic...)
	body = append(body, payload...)
	out := []byte{0x30}
	n := len(body)
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			break
		}
	}
	return append(out, body...)
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; i < 4; i++ {
		d, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(d&0x7f) * mult
		if d&0x80 == 0 {
			body := make([]byte, n)
			_, err := io.ReadFull(r, body)
			return typ, body, err
		}
		mult *= 128
	}
	return 0, nil, errors.New("bad length")
}

func parseConnect(b []byte) (Connect, error) {
	var c Connect
	str := func() (string, error) {
		if len(b) < 2 {
			return "", errors.New("short")
		}
		n := int(b[0])<<8 | int(b[1])
		if len(b) < 2+n {
			return "", errors.New("short")
		}
		s := string(b[2 : 2+n])
		b = b[2+n:]
		return s, nil
	}
	proto, err := str()
	if err != nil || proto != "MQTT" || len(b) < 4 {
		return c, errors.New("bad connect")
	}
	flags := b[1]
	b = b[4:]
	if c.ClientID, err = str(); err != nil {
		return c, err
	}
	if flags&0x80 != 0 {
		if c.Username, err = str(); err != nil {
			return c, err
		}
	}
	if flags&0x40 != 0 {
		if c.Password, err = str(); err != nil {
			return c, err
		}
	}
	return c, nil
}

// SSDPPacket builds a Bambu NOTIFY packet.
func SSDPPacket(serial, ip, model, name string) []byte {
	return []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1990\r\n" +
		"Server: UPnP/1.0\r\n" +
		"Location: " + ip + "\r\n" +
		"NT: urn:bambulab-com:device:3dprinter:1\r\n" +
		"USN: " + serial + "\r\n" +
		"Cache-Control: max-age=1800\r\n" +
		"DevModel.bambu.com: " + model + "\r\n" +
		"DevName.bambu.com: " + name + "\r\n" +
		"DevSignal.bambu.com: -45\r\n" +
		"DevConnect.bambu.com: lan\r\n" +
		"DevBind.bambu.com: free\r\n" +
		"Devseclink.bambu.com: secure\r\n" +
		"DevVersion.bambu.com: 01.04.20.00\r\n" +
		"DevCap.bambu.com: 1\r\n\r\n")
}

// ReadFile reads a file relative to the test's package directory.
func ReadFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

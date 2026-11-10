// Package ftps is a minimal implicit FTPS client for uploading a file to a
// Bambu Lab printer's storage (port 990, vsftpd).
//
// The control and data connections both use TLS from the same tls.Config, so
// the data connection resumes the control connection's TLS session: the
// printer's server requires that (vsftpd require_ssl_reuse). The caller's
// tls.Config must set ClientSessionCache, ServerName and VerifyConnection.
//
// Only what an upload needs: USER/PASS, PBSZ/PROT, TYPE I, PASV, STOR, SIZE,
// DELE and QUIT. The data address from PASV is only used for its port: the
// host is always the control connection's, so a server can't send us
// elsewhere. The password is written only inside the PASS command and never
// appears in an error.
package ftps

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DialFunc opens a TCP connection.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ReplyError is an FTP reply other than the one expected.
type ReplyError struct {
	Code int
	Msg  string
}

func (e *ReplyError) Error() string { return fmt.Sprintf("ftp %d %s", e.Code, e.Msg) }

// Code returns the FTP reply code of err, or 0.
func Code(err error) int {
	var re *ReplyError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

// Client is one logged in FTPS session.
type Client struct {
	conn    *tls.Conn
	tp      *textproto.Conn
	host    string
	cfg     *tls.Config
	dial    DialFunc
	timeout time.Duration
}

// Dial connects to addr (host:port) with implicit TLS and reads the greeting.
func Dial(ctx context.Context, addr string, cfg *tls.Config, dial DialFunc, timeout time.Duration) (*Client, error) {
	if cfg == nil || cfg.ClientSessionCache == nil || cfg.ServerName == "" || cfg.VerifyConnection == nil {
		return nil, errors.New("ftps: tls.Config needs ClientSessionCache, ServerName and VerifyConnection")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := dial(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(dctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	c := &Client{conn: tc, tp: textproto.NewConn(tc), host: host, cfg: cfg, dial: dial, timeout: timeout}
	if _, err := c.expect(220); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *Client) close() { _ = c.conn.Close() }

func (c *Client) deadline() { _ = c.conn.SetDeadline(time.Now().Add(c.timeout)) }

// cmd sends one command. The PASS command is the only one with a secret.
func (c *Client) cmd(format string, args ...any) error {
	c.deadline()
	_, err := c.tp.Cmd(format, args...)
	return err
}

func (c *Client) expect(codes ...int) (string, error) {
	c.deadline()
	code, msg, err := c.tp.ReadResponse(0)
	if err != nil && code == 0 {
		return "", err
	}
	for _, want := range codes {
		if code == want || (want < 10 && code/100 == want) {
			return msg, nil
		}
	}
	return msg, &ReplyError{Code: code, Msg: firstLine(msg)}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.TrimSpace(s)
}

// Login sends USER and PASS, then PBSZ 0, PROT P and TYPE I.
func (c *Client) Login(user, pass string) error {
	if err := c.cmd("USER %s", user); err != nil {
		return err
	}
	if _, err := c.expect(331, 230); err != nil {
		return err
	}
	if err := c.cmd("PASS %s", pass); err != nil {
		return errors.New("ftps: sending the password failed")
	}
	if _, err := c.expect(230); err != nil {
		return err
	}
	for _, step := range []struct {
		cmd  string
		want int
	}{{"PBSZ 0", 200}, {"PROT P", 200}, {"TYPE I", 200}} {
		if err := c.cmd("%s", step.cmd); err != nil {
			return err
		}
		if _, err := c.expect(step.want); err != nil {
			return err
		}
	}
	return nil
}

var pasvRe = regexp.MustCompile(`\((\d+),(\d+),(\d+),(\d+),(\d+),(\d+)\)`)

func (c *Client) passive(ctx context.Context) (net.Conn, error) {
	if err := c.cmd("PASV"); err != nil {
		return nil, err
	}
	msg, err := c.expect(227)
	if err != nil {
		return nil, err
	}
	m := pasvRe.FindStringSubmatch(msg)
	if m == nil {
		return nil, errors.New("ftps: bad PASV answer")
	}
	hi, _ := strconv.Atoi(m[5])
	lo, _ := strconv.Atoi(m[6])
	port := hi<<8 | lo
	if hi > 255 || lo > 255 || port == 0 {
		return nil, errors.New("ftps: bad PASV port")
	}
	dctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.dial(dctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(port)))
}

var nameRe = regexp.MustCompile(`^/?[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)

// Store uploads size bytes from r as name, then checks the size on the
// server. progress (may be nil) gets the bytes sent so far.
func (c *Client) Store(ctx context.Context, name string, r io.Reader, size int64, progress func(int64)) error {
	if !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return errors.New("ftps: bad file name")
	}
	raw, err := c.passive(ctx)
	if err != nil {
		return err
	}
	defer raw.Close()
	if err := c.cmd("STOR %s", name); err != nil {
		return err
	}
	if _, err := c.expect(150, 125); err != nil {
		return err
	}
	// The data connection resumes the control connection's TLS session.
	dc := tls.Client(raw, c.cfg)
	hctx, cancel := context.WithTimeout(ctx, c.timeout)
	err = dc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		return fmt.Errorf("ftps: data connection: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	sent, err := copyWithProgress(dc, r, size, progress, c.timeout, raw)
	// Close without waiting for the server's close_notify (vsftpd may never
	// send it, and Python's client skips unwrap for that reason).
	_ = dc.Close()
	if err != nil {
		return fmt.Errorf("ftps: sending the file: %w", err)
	}
	if sent != size {
		return fmt.Errorf("ftps: sent %d of %d bytes", sent, size)
	}
	// The 226 can take a while on a printer.
	_ = c.conn.SetDeadline(time.Now().Add(max(c.timeout, 90*time.Second)))
	code, msg, rerr := c.tp.ReadResponse(0)
	switch {
	case rerr == nil && code == 226:
	case code == 426 || code == 451:
		// Seen on Bambu printers after a full upload: check the size instead.
	case rerr != nil && code == 0:
		return rerr
	default:
		return &ReplyError{Code: code, Msg: firstLine(msg)}
	}
	got, err := c.Size(name)
	if err != nil {
		return err
	}
	if got != size {
		return fmt.Errorf("ftps: the printer has %d of %d bytes", got, size)
	}
	return nil
}

func copyWithProgress(w io.Writer, r io.Reader, size int64, progress func(int64), timeout time.Duration, raw net.Conn) (int64, error) {
	buf := make([]byte, 64<<10)
	var sent int64
	for sent < size {
		n, rerr := r.Read(buf[:min(int64(len(buf)), size-sent)])
		if n > 0 {
			_ = raw.SetWriteDeadline(time.Now().Add(timeout))
			if _, err := w.Write(buf[:n]); err != nil {
				return sent, err
			}
			sent += int64(n)
			if progress != nil {
				progress(sent)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return sent, rerr
		}
	}
	return sent, nil
}

// Size returns the size of name on the server.
func (c *Client) Size(name string) (int64, error) {
	if !nameRe.MatchString(name) {
		return 0, errors.New("ftps: bad file name")
	}
	if err := c.cmd("SIZE %s", name); err != nil {
		return 0, err
	}
	msg, err := c.expect(213)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(msg), 10, 64)
}

// Delete removes name.
func (c *Client) Delete(name string) error {
	if !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return errors.New("ftps: bad file name")
	}
	if err := c.cmd("DELE %s", name); err != nil {
		return err
	}
	_, err := c.expect(250)
	return err
}

// Quit ends the session.
func (c *Client) Quit() {
	if c.cmd("QUIT") == nil {
		_, _ = c.expect(221)
	}
	c.close()
}

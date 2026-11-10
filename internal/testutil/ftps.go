package testutil

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// FTPServer is a fake Bambu printer FTPS server (implicit TLS, like vsftpd
// 3.0.5 on the printers). The data connection must resume the control
// connection's TLS session, as with vsftpd's require_ssl_reuse.
type FTPServer struct {
	Addr string

	ln  net.Listener
	cfg *tls.Config

	mu        sync.Mutex
	code      string
	noStorage bool // STOR answers 553 Could not create file. (no USB stick)
	reply426  bool // answer 426 after a complete upload (seen on printers)
	files     map[string][]byte
	logins    []Connect
	commands  []string
	resumed   []bool
	noDelete  map[string]bool // DELE of these names fails (550)
	conns     []net.Conn
	wg        sync.WaitGroup
}

// NewFTPServer listens on 127.0.0.1 with cert and accepts user bblp + code.
func NewFTPServer(t testing.TB, cert tls.Certificate, code string) *FTPServer {
	t.Helper()
	s := &FTPServer{code: code, files: map[string][]byte{}}
	s.cfg = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.ln, s.Addr = ln, ln.Addr().String()
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// SetNoStorage makes STOR fail like a printer with no USB stick.
func (s *FTPServer) SetNoStorage(v bool) { s.mu.Lock(); s.noStorage = v; s.mu.Unlock() }

// Put stores a file as if someone had put it on the USB stick.
func (s *FTPServer) Put(name string, data []byte) {
	s.mu.Lock()
	s.files[name] = append([]byte(nil), data...)
	s.mu.Unlock()
}

// FailDelete makes DELE of name answer 550 (n times; -1 for always).
func (s *FTPServer) FailDelete(name string) {
	s.mu.Lock()
	if s.noDelete == nil {
		s.noDelete = map[string]bool{}
	}
	s.noDelete[name] = true
	s.mu.Unlock()
}

// SetReply426 makes the server answer 426 after a complete upload.
func (s *FTPServer) SetReply426(v bool) { s.mu.Lock(); s.reply426 = v; s.mu.Unlock() }

// Files returns a copy of every stored file.
func (s *FTPServer) Files() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range s.files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// Logins returns every USER/PASS pair received.
func (s *FTPServer) Logins() []Connect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Connect(nil), s.logins...)
}

// Commands returns every command received, with PASS arguments removed.
func (s *FTPServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Resumed reports, per data connection, whether it resumed the TLS session.
func (s *FTPServer) Resumed() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.resumed...)
}

// Close stops the server.
func (s *FTPServer) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *FTPServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			s.serve(c.(*tls.Conn))
		}()
	}
}

func (s *FTPServer) serve(c *tls.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := c.Handshake(); err != nil {
		return
	}
	r := bufio.NewReader(c)
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(c, format+"\r\n", args...) }
	say("220 (vsFTPd 3.0.5)")
	var user string
	authed := false
	var dataLn net.Listener
	defer func() {
		if dataLn != nil {
			_ = dataLn.Close()
		}
	}()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		s.mu.Lock()
		if verb == "PASS" {
			s.commands = append(s.commands, "PASS ***")
		} else {
			s.commands = append(s.commands, line)
		}
		s.mu.Unlock()
		switch {
		case verb == "USER":
			user = arg
			say("331 Please specify the password.")
		case verb == "PASS":
			s.mu.Lock()
			s.logins = append(s.logins, Connect{Username: user, Password: arg})
			ok := user == "bblp" && arg == s.code
			s.mu.Unlock()
			if !ok {
				say("530 Login incorrect.")
				return
			}
			authed = true
			say("230 Login successful.")
		case !authed:
			say("530 Please login with USER and PASS.")
		case verb == "PBSZ":
			say("200 PBSZ set to 0.")
		case verb == "PROT":
			say("200 PROT now Private.")
		case verb == "TYPE":
			say("200 Switching to Binary mode.")
		case verb == "PWD":
			say(`257 "/" is the current directory`)
		case verb == "PASV":
			if dataLn != nil {
				_ = dataLn.Close()
			}
			dataLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				say("425 Can't open data connection.")
				continue
			}
			port := dataLn.Addr().(*net.TCPAddr).Port
			// A real printer names its own LAN address; the client ignores it.
			say("227 Entering Passive Mode (192,168,2,4,%d,%d).", port>>8, port&0xff)
		case verb == "STOR":
			s.mu.Lock()
			noStorage := s.noStorage
			s.mu.Unlock()
			if noStorage {
				say("553 Could not create file.")
				continue
			}
			if dataLn == nil {
				say("425 Use PORT or PASV first.")
				continue
			}
			say("150 Ok to send data.")
			s.receive(dataLn, strings.TrimPrefix(arg, "/"), say)
			_ = dataLn.Close()
			dataLn = nil
		case verb == "LIST":
			if dataLn == nil {
				say("425 Use PORT or PASV first.")
				continue
			}
			say("150 Here comes the directory listing.")
			s.list(dataLn, say)
			_ = dataLn.Close()
			dataLn = nil
		case verb == "SIZE":
			s.mu.Lock()
			f, ok := s.files[strings.TrimPrefix(arg, "/")]
			s.mu.Unlock()
			if !ok {
				say("550 Could not get file size.")
			} else {
				say("213 %d", len(f))
			}
		case verb == "DELE":
			name := strings.TrimPrefix(arg, "/")
			s.mu.Lock()
			_, ok := s.files[name]
			if ok && s.noDelete[name] {
				ok = false
			} else {
				delete(s.files, name)
			}
			s.mu.Unlock()
			if ok {
				say("250 Delete operation successful.")
			} else {
				say("550 Delete operation failed.")
			}
		case verb == "QUIT":
			say("221 Goodbye.")
			return
		default:
			say("500 Unknown command.")
		}
	}
}

func (s *FTPServer) receive(ln net.Listener, name string, say func(string, ...any)) {
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	raw, err := ln.Accept()
	if err != nil {
		say("425 Failed to establish connection.")
		return
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	tc := tls.Server(raw, s.cfg)
	if err := tc.Handshake(); err != nil {
		say("522 SSL connection failed.")
		return
	}
	resumed := tc.ConnectionState().DidResume
	s.mu.Lock()
	s.resumed = append(s.resumed, resumed)
	s.mu.Unlock()
	if !resumed {
		say("522 SSL connection failed: session reuse required")
		return
	}
	data, err := io.ReadAll(io.LimitReader(tc, 64<<20))
	if err != nil && len(data) == 0 {
		say("426 Failure reading network stream.")
		return
	}
	s.mu.Lock()
	s.files[name] = data
	r426 := s.reply426
	s.mu.Unlock()
	if r426 {
		say("426 Failure writing network stream.")
		return
	}
	say("226 Transfer complete.")
}

// list sends the files in vsftpd 3.0.5's LIST format over a resumed data connection.
func (s *FTPServer) list(ln net.Listener, say func(string, ...any)) {
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	raw, err := ln.Accept()
	if err != nil {
		say("425 Failed to establish connection.")
		return
	}
	defer raw.Close()
	tc := tls.Server(raw, s.cfg)
	if err := tc.Handshake(); err != nil || !tc.ConnectionState().DidResume {
		say("522 SSL connection failed: session reuse required")
		return
	}
	s.mu.Lock()
	var b strings.Builder
	for name, data := range s.files {
		fmt.Fprintf(&b, "-rwxr-xr-x    1 103      107      %9d Nov 10 02:41 %s\r\n", len(data), name)
	}
	s.mu.Unlock()
	_, _ = tc.Write([]byte(b.String()))
	_ = tc.Close()
	say("226 Directory send OK.")
}

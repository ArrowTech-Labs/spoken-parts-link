package testutil

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"strings"
	"testing"
	"time"
)

func nalTypes(clip []byte) map[byte]int {
	m := map[byte]int{}
	for _, n := range SplitAnnexB(clip) {
		m[n[0]&0x1f]++
	}
	return m
}

func TestClips(t *testing.T) {
	for name, clip := range map[string][]byte{"checked in": TestClip(), "H264Clip": H264Clip(t)} {
		m := nalTypes(clip)
		if m[5] < 2 || m[7] < 1 || m[8] < 1 || m[1] < 2 {
			t.Fatalf("%s: NAL types %v", name, m)
		}
	}
	got := SplitAnnexB([]byte{0, 0, 0, 1, 0x67, 1, 0, 0, 1, 0x68, 2, 0, 0, 0, 0, 1, 0x65, 3})
	if len(got) != 3 || !bytes.Equal(got[0], []byte{0x67, 1}) || !bytes.Equal(got[1], []byte{0x68, 2}) || !bytes.Equal(got[2], []byte{0x65, 3}) {
		t.Fatalf("%q", got)
	}
}

func TestRTSPServerNeedsLogin(t *testing.T) {
	ca := NewCA(t, "Test Printer CA")
	s := NewRTSPServer(t, ca.Leaf(t, "SERIAL0001"), RTSPOptions{Code: "Abc12345"})
	c, err := tls.Dial("tcp", s.Addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	for i, req := range []string{
		"OPTIONS " + s.URL + " RTSP/1.0\r\nCSeq: 1\r\n\r\n",
		"DESCRIBE " + s.URL + " RTSP/1.0\r\nCSeq: 2\r\n\r\n",
	} {
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"RTSP/1.0 200", "RTSP/1.0 401"}[i]
		if !strings.HasPrefix(line, want) {
			t.Fatalf("%d: %q", i, line)
		}
		for {
			h, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if i == 1 && strings.HasPrefix(h, "WWW-Authenticate: Digest ") && !strings.Contains(h, "nonce=") {
				t.Fatalf("challenge %q", h)
			}
			if h == "\r\n" {
				break
			}
		}
	}
	if m := s.Methods(); len(m) != 2 || m[1] != "DESCRIBE" {
		t.Fatalf("%v", m)
	}
}

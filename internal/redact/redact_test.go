package redact

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestRedactReplacesEverySecret(t *testing.T) {
	r := New()
	r.Add("AbCd1234")
	r.Add("K7P4QX9M")
	got := r.Redact("code=AbCd1234 pair=K7P4QX9M again AbCd1234")
	if strings.Contains(got, "AbCd1234") || strings.Contains(got, "K7P4QX9M") {
		t.Fatalf("secret left in %q", got)
	}
	if strings.Count(got, Mask) != 3 {
		t.Fatalf("want 3 masks in %q", got)
	}
}

func TestRedactIgnoresShortValues(t *testing.T) {
	r := New()
	r.Add("ab")
	r.Add("")
	if r.Len() != 0 {
		t.Fatalf("short values registered")
	}
	if got := r.Redact("ab cd"); got != "ab cd" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactBytesAllEncodings(t *testing.T) {
	r := New()
	key := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb}
	r.AddBytes(key)
	for _, s := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key),
		hex.EncodeToString(key),
		strings.ToUpper(hex.EncodeToString(key)),
	} {
		if got := r.Redact("x " + s + " y"); strings.Contains(got, s) {
			t.Errorf("encoding %q not redacted: %q", s, got)
		}
	}
}

func TestRedactLongestFirst(t *testing.T) {
	r := New()
	r.Add("ABCDEF")
	r.Add("ABCDEFGH")
	if got := r.Redact("ABCDEFGH"); got != Mask {
		t.Fatalf("got %q", got)
	}
}

func TestNilRedactor(t *testing.T) {
	var r *Redactor
	r.Add("AbCd1234")
	if r.Redact("x") != "x" {
		t.Fatal("nil redactor changed text")
	}
}

func TestCapsBoundTheSet(t *testing.T) {
	r := New()
	r.Add("AbCd1234")
	for i := 0; i < 100; i++ {
		r.AddTransient(fmt.Sprintf("PAIR%04d", i))
	}
	if r.Len() != 1+MaxTransient {
		t.Fatalf("transient cap: %d", r.Len())
	}
	if r.Redact("PAIR0099") != Mask || r.Redact("PAIR0000") == Mask {
		t.Fatal("oldest transient values must be dropped first")
	}
	if r.Redact("AbCd1234") != Mask {
		t.Fatal("permanent value dropped by transient churn")
	}
	for i := 0; i < MaxPermanent+10; i++ {
		r.Add(fmt.Sprintf("CODE%06d", i))
	}
	if r.Len() > MaxPermanent+MaxTransient {
		t.Fatalf("permanent cap: %d", r.Len())
	}
}

func TestTransientThenPermanent(t *testing.T) {
	r := New()
	r.AddTransient("K7P4QX9M")
	r.Add("K7P4QX9M")
	for i := 0; i < 40; i++ {
		r.AddTransient(fmt.Sprintf("PAIR%04d", i))
	}
	if r.Redact("K7P4QX9M") != Mask {
		t.Fatal("a value made permanent was dropped")
	}
}

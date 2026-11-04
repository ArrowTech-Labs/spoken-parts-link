package logx

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
)

func TestEveryLineIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	r := redact.New()
	r.Add("AbCd1234")
	l := New(&buf, r, Debug)
	l.Info("saved AbCd1234", "code", "AbCd1234", "err", errors.New("printer said AbCd1234"))
	l.Writer(Warning).Write([]byte("library line AbCd1234\nsecond AbCd1234\n"))
	out := buf.String()
	if strings.Contains(out, "AbCd1234") {
		t.Fatalf("secret in log: %s", out)
	}
	if strings.Count(out, "\n") != 3 {
		t.Fatalf("want 3 lines, got %q", out)
	}
}

func TestLevels(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, redact.New(), Warning)
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	if got := buf.String(); strings.Contains(got, " d") || strings.Contains(got, " i") || !strings.Contains(got, "WARN w") || !strings.Contains(got, "ERROR e") {
		t.Fatalf("got %q", got)
	}
	if ParseLevel("debug") != Debug || ParseLevel("warning") != Warning || ParseLevel("error") != Error || ParseLevel("x") != Info {
		t.Fatal("ParseLevel")
	}
}

func TestNoNewlineInjection(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, redact.New(), Info)
	l.Info("a", "v", "x\nFAKE line")
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("newline injected: %q", buf.String())
	}
}

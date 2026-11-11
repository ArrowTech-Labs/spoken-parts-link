package hanotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/monitor"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/store"
)

const testID = "spokenparts-phone-0123456789abcdefABCDEF"

func newNotifier(t *testing.T, base string) (*Notifier, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets", "ha.enc")
	n := New(base, store.NewCodes(path), redact.New())
	key := make([]byte, 32)
	key[0] = 7
	if err := n.Unlock(key); err != nil {
		t.Fatal(err)
	}
	return n, path
}

func TestNotifyPostsToTheWebhook(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n, path := newNotifier(t, srv.URL)
	if ok, _ := n.Ready(); ok {
		t.Fatal("ready before set up")
	}
	if err := n.Set("short", monitor.Warning); err != ErrBadID {
		t.Fatalf("short id: %v", err)
	}
	if err := n.Set(testID, monitor.Critical); err != nil {
		t.Fatal(err)
	}
	if ok, min := n.Ready(); !ok || min != monitor.Critical {
		t.Fatalf("ready %v %v", ok, min)
	}
	err := n.Notify(context.Background(), monitor.Notice{Title: "Spoken Parts: H2D", Message: "Possible spaghetti", Severity: "critical", Key: "hms:x", Printer: "H2D", ImageURL: "https://spokenparts.com/f/1"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if gotPath != "/api/webhook/"+testID || got.Message != "Possible spaghetti" || got.ImageURL != "https://spokenparts.com/f/1" {
		t.Errorf("path %q payload %+v", gotPath, got)
	}
	mu.Unlock()
	// Stored encrypted: the id is not in the file.
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), testID) || len(b) == 0 {
		t.Fatal("webhook id stored in the clear")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := n.Clear(); err != nil || n.Configured() {
		t.Fatalf("clear %v", err)
	}
}

func TestNotifyErrorsDoNotNameTheID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	n, _ := newNotifier(t, srv.URL)
	_ = n.Set(testID, monitor.Warning)
	err := n.Notify(context.Background(), monitor.Notice{Message: "x"})
	if err == nil || strings.Contains(err.Error(), testID) {
		t.Fatalf("err %v", err)
	}
	srv.Close()
	err = n.Notify(context.Background(), monitor.Notice{Message: "x"})
	if err == nil || strings.Contains(err.Error(), testID) || strings.Contains(err.Error(), "webhook") {
		t.Fatalf("err %v", err)
	}
}

func TestRedactsTheID(t *testing.T) {
	red := redact.New()
	n := New("http://127.0.0.1:1", store.NewCodes(filepath.Join(t.TempDir(), "ha.enc")), red)
	_ = n.Unlock(make([]byte, 32))
	_ = n.Set(testID, monitor.Warning)
	if out := red.Redact("url /api/webhook/" + testID); strings.Contains(out, testID) {
		t.Fatalf("not redacted: %s", out)
	}
}

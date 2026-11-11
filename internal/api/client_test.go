package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/signer"
)

func TestParseServer(t *testing.T) {
	good := []string{"https://spokenparts.com", "https://spokenparts.com/", "http://localhost:8080", "http://127.0.0.1:1", "http://[::1]:9"}
	for _, s := range good {
		if _, err := ParseServer(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := []string{"http://spokenparts.com", "ftp://x", "https://user:pw@spokenparts.com", "https://spokenparts.com/?a=1", "http://192.0.2.1", "", "spokenparts.com"}
	for _, s := range bad {
		if _, err := ParseServer(s); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
}

func TestNextSyncClamp(t *testing.T) {
	cases := map[int]time.Duration{0: 30 * time.Second, -1: 30 * time.Second, 1: 5 * time.Second, 30: 30 * time.Second, 301: 300 * time.Second, 100000: 300 * time.Second}
	for in, want := range cases {
		if got := NextSync(in); got != want {
			t.Errorf("NextSync(%d)=%v want %v", in, got, want)
		}
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := 1; attempt < 20; attempt++ {
		for i := 0; i < 50; i++ {
			d := Backoff(attempt, time.Second, time.Minute)
			if d < 500*time.Millisecond || d > time.Minute {
				t.Fatalf("attempt %d: %v", attempt, d)
			}
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	if parseRetryAfter("12", now) != 12*time.Second {
		t.Fatal("seconds")
	}
	if parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now) != 30*time.Second {
		t.Fatal("date")
	}
	if parseRetryAfter("99999", now) != maxRetryWait {
		t.Fatal("cap")
	}
	if parseRetryAfter("junk", now) != 0 {
		t.Fatal("junk")
	}
}

// A fake server that checks the signature of every request.
func TestRequestsAreSignedPerContract(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := signer.Verify(pub, r.Method, r.URL.RequestURI(), r.Header.Get(signer.HeaderTimestamp), r.Header.Get(signer.HeaderNonce), body, r.Header.Get(signer.HeaderSignature)); err != nil {
			t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
		}
		if ua := r.Header.Get("User-Agent"); ua != "spoken-parts-link/9.9.9" {
			t.Errorf("user agent %q", ua)
		}
		if len(body) > 0 && r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		seen = append(seen, r.Method+" "+r.URL.Path+" link="+r.Header.Get(signer.HeaderLink))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/link/v1/pair":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"link_id":"6f1c3c3e-1f7a-4c62-9d1e-2b8f0c8d9a10","status":"pending","check_number":"0482","expires_at":"2026-11-04T09:15:00Z"}`))
		case "/api/link/v1/pair/status":
			_, _ = w.Write([]byte(`{"status":"active","check_number":"0482","site":{"id":4,"name":"Gilbert"},"company":{"name":"Spoken Parts"}}`))
		case "/api/link/v1/sync":
			var req SyncRequest
			if err := json.Unmarshal(body, &req); err != nil || req.Printers == nil {
				t.Errorf("sync body %s", body)
			}
			_, _ = w.Write([]byte(`{"next_sync_s":45,"paused":false,"status":"active","site":{"id":4,"name":"Gilbert"},"company":{"name":"Spoken Parts"},"sends":[],"cancel":[],"commands":[]}`))
		case "/api/link/v1/data-key":
			_, _ = w.Write([]byte(`{"data_key":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}`))
		}
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "9.9.9")
	ctx := context.Background()
	pr, err := c.Pair(ctx, priv, PairRequest{Code: "K7P4QX9M", PublicKey: base64.StdEncoding.EncodeToString(pub), Name: "Spoken Parts Link", Platform: "ha_addon", Version: "9.9.9"})
	if err != nil || pr.CheckNumber != "0482" {
		t.Fatalf("pair %v %+v", err, pr)
	}
	ps, err := c.PairStatus(ctx, priv, pr.LinkID)
	if err != nil || ps.Site == nil || ps.Site.Name != "Gilbert" {
		t.Fatalf("status %v %+v", err, ps)
	}
	sr, err := c.Sync(ctx, priv, pr.LinkID, SyncRequest{Version: "9.9.9"})
	if err != nil || sr.NextSyncS != 45 {
		t.Fatalf("sync %v %+v", err, sr)
	}
	dk, err := c.DataKey(ctx, priv, pr.LinkID)
	if err != nil || len(dk) != 32 {
		t.Fatalf("data key %v", err)
	}
	want := []string{
		"POST /api/link/v1/pair link=",
		"GET /api/link/v1/pair/status link=" + pr.LinkID,
		"POST /api/link/v1/sync link=" + pr.LinkID,
		"POST /api/link/v1/data-key link=" + pr.LinkID,
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests\n%v\nwant\n%v", seen, want)
	}
}

func TestErrorsAndRetryAfter(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/link/v1/sync":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"detail":"slow down","code":"throttled"}`))
		case "/api/link/v1/data-key":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"detail":"gone","code":"link_revoked"}`))
		case "/api/link/v1/pair/status":
			http.Redirect(w, r, "https://example.com/", http.StatusFound)
		}
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "1")
	_, err := c.Sync(context.Background(), priv, "id", SyncRequest{})
	if !HasCode(err, CodeThrottled) || RetryAfter(err) != 7*time.Second {
		t.Fatalf("throttle: %v %v", err, RetryAfter(err))
	}
	_, err = c.DataKey(context.Background(), priv, "id")
	if !HasCode(err, CodeLinkRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	_, err = c.PairStatus(context.Background(), priv, "id")
	var e *Error
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect must not be followed: %v %v", err, e)
	}
}

func TestFrameAndAlertsCalls(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var paths, types []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := signer.Verify(pub, r.Method, r.URL.RequestURI(), r.Header.Get(signer.HeaderTimestamp), r.Header.Get(signer.HeaderNonce), body, r.Header.Get(signer.HeaderSignature)); err != nil {
			t.Errorf("%s: %v", r.URL.Path, err)
		}
		paths = append(paths, r.URL.RequestURI())
		types = append(types, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/frames") {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"frame_id":"f","duplicate":false,"image_url":"https://x/y"}`))
			return
		}
		_, _ = w.Write([]byte(`{"accepted":1}`))
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "9.9.9")
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	id := "6f1c3c3e-1f7a-4c62-9d1e-2b8f0c8d9a10"
	layer := 12
	ans, err := c.Frame(context.Background(), priv, id, "0947CJ641600041", FrameMeta{ID: id, TakenAt: time.Unix(1762790000, 0), Reason: "alert", Printing: true, TaskID: "1427077046", Layer: &layer, AlertKey: "hms:0C00_0300_0003_0008"}, jpeg)
	if err != nil || ans.ImageURL != "https://x/y" {
		t.Fatalf("%v %+v", err, ans)
	}
	want := "/api/link/v1/printers/0947CJ641600041/frames?alert_key=hms%3A0C00_0300_0003_0008&id=" + id + "&layer=12&printing=1&reason=alert&task_id=1427077046&ts=1762790000"
	if paths[0] != want || types[0] != "image/jpeg" {
		t.Fatalf("frame call %q %q", paths[0], types[0])
	}
	if err := c.Alerts(context.Background(), priv, id, "0947CJ641600041", AlertBatch{Cleared: []string{"stall"}}); err != nil || types[1] != "application/json" {
		t.Fatalf("alerts %v", err)
	}
	for _, bad := range []struct {
		m FrameMeta
		b []byte
	}{
		{FrameMeta{ID: "x", Reason: "alert"}, jpeg},
		{FrameMeta{ID: id, Reason: "selfie"}, jpeg},
		{FrameMeta{ID: id, Reason: "alert"}, []byte("GIF89a")},
		{FrameMeta{ID: id, Reason: "alert"}, append([]byte{0xFF, 0xD8}, make([]byte, MaxFrame)...)},
	} {
		if _, err := c.Frame(context.Background(), priv, id, "0947CJ641600041", bad.m, bad.b); err == nil {
			t.Errorf("accepted %+v", bad.m)
		}
	}
	if err := c.Alerts(context.Background(), priv, id, "0947CJ641600041", AlertBatch{}); err == nil {
		t.Error("empty alerts call accepted")
	}
	if len(paths) != 2 {
		t.Errorf("bad calls reached the server: %q", paths)
	}
}

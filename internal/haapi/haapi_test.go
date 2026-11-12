package haapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-supervisor-token-0123456789abcdef" // made up for this test

type seen struct {
	method, path, query, auth, ctype string
	body                             []byte
}

func fakeHA(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var reqs []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), reqs...)
	}
}

func TestCallServiceRequestAndResponse(t *testing.T) {
	srv, reqs := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changed_states":[{"entity_id":"sensor.x"}],"service_response":{"version":"0.1.0","printers":[]}}`))
	})
	c := New(srv.URL+"/core/api/", testToken)
	out, err := c.CallService(context.Background(), Domain, "list_printers", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"version":"0.1.0","printers":[]}` {
		t.Fatalf("service_response not unwrapped: %s", out)
	}
	r := reqs()[0]
	if r.method != "POST" || r.path != "/core/api/services/spoken_parts_printers/list_printers" || r.query != "return_response" {
		t.Fatalf("request %+v", r)
	}
	if r.auth != "Bearer "+testToken || r.ctype != "application/json" || string(r.body) != "{}" {
		t.Fatalf("headers or body %+v", r)
	}
	data := map[string]any{"printer_id": "p", "pages": json.RawMessage(`[{"data_b64":"AAEC"}]`)}
	if _, err := c.CallService(context.Background(), Domain, "print_raster", data); err != nil {
		t.Fatal(err)
	}
	if got := string(reqs()[1].body); got != `{"pages":[{"data_b64":"AAEC"}],"printer_id":"p"}` {
		t.Fatalf("print body %s", got)
	}
}

func TestOnlyAllowedServices(t *testing.T) {
	srv, reqs := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {})
	c := New(srv.URL, testToken)
	for _, ds := range [][2]string{{"lock", "unlock"}, {"homeassistant", "restart"}, {Domain, "set_shutdown_timer"}, {Domain + "/../lock", "unlock"}, {"", ""}} {
		if _, err := c.CallService(context.Background(), ds[0], ds[1], nil); !errors.Is(err, ErrNotAllowed) || Code(err) != "refused" {
			t.Errorf("%v: %v", ds, err)
		}
	}
	if len(reqs()) != 0 {
		t.Fatal("a refused call reached Home Assistant")
	}
	if AllowedServices != [...]string{"list_printers", "print_raster"} {
		t.Fatal("allowed services changed")
	}
}

func TestNoToken(t *testing.T) {
	srv, reqs := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {})
	c := New(srv.URL, "  ")
	if c.HasToken() {
		t.Fatal("blank token counted")
	}
	_, err := c.CallService(context.Background(), Domain, "list_printers", nil)
	if !errors.Is(err, ErrNoToken) || Code(err) != "no_token" {
		t.Fatalf("%v", err)
	}
	if len(reqs()) != 0 {
		t.Fatal("called without a token")
	}
}

func TestNotInstalledAndOtherErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   string
	}{
		// What Home Assistant answers for a service it does not have
		// (HTTPBadRequest from ServiceNotFound: aiohttp's plain body).
		{400, "400: Bad Request", "not_installed"},
		{400, `{"message":"Service spoken_parts_printers.list_printers not found."}`, "not_installed"},
		{404, `{"message":"Not found"}`, "not_installed"},
		{400, `{"message":"Service does not support responses. Remove return_response from request."}`, "http_400"},
		{401, `{"message":"Unauthorized"}`, "http_401"},
		{500, `{"message":"Something broke"}`, "http_500"},
		{502, `bad gateway`, "http_502"},
	}
	for _, tc := range cases {
		srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		c := New(srv.URL, testToken)
		_, err := c.CallService(context.Background(), Domain, "list_printers", nil)
		if Code(err) != tc.code {
			t.Errorf("%d %q: code %q (%v)", tc.status, tc.body, Code(err), err)
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("error names the token: %v", err)
		}
	}
	var he *Error
	srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"` + strings.Repeat("x", 500) + `"}`))
	})
	_, err := New(srv.URL, testToken).CallService(context.Background(), Domain, "print_raster", nil)
	if !errors.As(err, &he) || len(he.Message) != 200 {
		t.Fatalf("long message not cut: %v", err)
	}
}

func TestBadAnswers(t *testing.T) {
	for _, body := range []string{`not json`, `{"changed_states":[]}`, `{"changed_states":[],"service_response":null}`, `[]`} {
		srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := New(srv.URL, testToken).CallService(context.Background(), Domain, "print_raster", nil)
		if err == nil || Code(err) != "bad_answer" {
			t.Errorf("%q: %v", body, err)
		}
	}
	srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"service_response":"` + strings.Repeat("A", maxAnswer) + `"}`))
	})
	if _, err := New(srv.URL, testToken).CallService(context.Background(), Domain, "list_printers", nil); err == nil {
		t.Fatal("oversized answer accepted")
	}
}

func TestTimeoutAndUnreachable(t *testing.T) {
	release := make(chan struct{})
	srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(srv.URL, testToken).CallService(ctx, Domain, "print_raster", nil)
	if !errors.Is(err, ErrTimeout) || Code(err) != "timeout" || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %v", err)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, err = New("http://"+addr, testToken).CallService(context.Background(), Domain, "list_printers", nil)
	if !errors.Is(err, ErrUnreachable) || Code(err) != "ha_unreachable" {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), addr) {
		t.Fatalf("error says too much: %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	var other []string
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other = append(other, r.Header.Get("Authorization")) }))
	defer o.Close()
	srv, _ := fakeHA(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, o.URL, http.StatusTemporaryRedirect) })
	_, err := New(srv.URL, testToken).CallService(context.Background(), Domain, "list_printers", nil)
	if err == nil || len(other) != 0 {
		t.Fatalf("redirect followed: %v %v", err, other)
	}
}

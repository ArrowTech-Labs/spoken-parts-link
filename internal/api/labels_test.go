package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/signer"
)

const labelJobID = "6f1c3c3e-1f7a-4c62-9d1e-2b8f0c8d9a10"

func TestLabelCallsAreSignedAndShaped(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var seen []string
	var bodies = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := signer.Verify(pub, r.Method, r.URL.RequestURI(), r.Header.Get(signer.HeaderTimestamp), r.Header.Get(signer.HeaderNonce), body, r.Header.Get(signer.HeaderSignature)); err != nil {
			t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
		}
		if r.Header.Get(signer.HeaderLink) != "link-1" {
			t.Errorf("link header %q", r.Header.Get(signer.HeaderLink))
		}
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		bodies[r.URL.Path] = string(body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/link/v1/label-printers":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/link/v1/label-jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":"` + labelJobID + `","printer_id":"nelko.p21:aa:bb","created_at":"x","expires_at":"y"},{"id":"../../sync","printer_id":"p"}]}`))
		case "/api/link/v1/label-jobs/" + labelJobID + "/claim":
			_, _ = w.Write([]byte(`{"id":"` + labelJobID + `","printer_id":"nelko.p21:aa:bb","driver":"nelko.p21","document_type":"spool_label","format":"raster",` +
				`"media":{"id":"14x40","kind":"die_cut","width_mm":14,"length_mm":40},"copies":1,"pages":[{"width":96,"height":320,"data_b64":"AAEC"}]}`))
		case "/api/link/v1/label-jobs/" + labelJobID + "/state":
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "0.5.0")
	ctx := context.Background()
	bat := 80
	if err := c.ReportLabelPrinters(ctx, priv, "link-1", LabelReport{Integration: LabelIntegration{Present: true, Version: "0.1.0"},
		Printers: []LabelPrinter{{ID: "nelko.p21:aa:bb", Name: "Label maker", Driver: "nelko.p21", State: "ready", Battery: &bat}}}); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	_ = json.Unmarshal([]byte(bodies["/api/link/v1/label-printers"]), &rep)
	integ, _ := rep["integration"].(map[string]any)
	ps, _ := rep["printers"].([]any)
	if integ["present"] != true || integ["version"] != "0.1.0" || integ["error"] != "" || len(ps) != 1 {
		t.Fatalf("report body %s", bodies["/api/link/v1/label-printers"])
	}
	p0 := ps[0].(map[string]any)
	for _, k := range []string{"id", "name", "driver", "transport", "address", "state", "last_error", "battery", "media", "last_seen"} {
		if _, ok := p0[k]; !ok {
			t.Errorf("printer lacks %s", k)
		}
	}
	// An empty report still sends a list, never null.
	if err := c.ReportLabelPrinters(ctx, priv, "link-1", LabelReport{Integration: LabelIntegration{Error: "not_installed"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies["/api/link/v1/label-printers"], `"printers":[]`) || !strings.Contains(bodies["/api/link/v1/label-printers"], `"present":false`) {
		t.Fatalf("empty report %s", bodies["/api/link/v1/label-printers"])
	}

	jobs, err := c.ListLabelJobs(ctx, priv, "link-1")
	if err != nil || len(jobs) != 1 || jobs[0].ID != labelJobID {
		t.Fatalf("jobs %v %+v (a bad id must be dropped)", err, jobs)
	}
	job, err := c.ClaimLabelJob(ctx, priv, "link-1", labelJobID)
	if err != nil || job.Format != "raster" || job.PrinterID != "nelko.p21:aa:bb" || string(job.Copies) != "1" {
		t.Fatalf("claim %v %+v", err, job)
	}
	if string(job.Pages) != `[{"width":96,"height":320,"data_b64":"AAEC"}]` || !strings.Contains(string(job.Media), `"14x40"`) {
		t.Fatalf("pages and media must stay as sent: %s %s", job.Pages, job.Media)
	}
	long := strings.Repeat("é", 300)
	if err := c.LabelJobState(ctx, priv, "link-1", labelJobID, LabelStateRequest{State: "failed", Code: "offline", Message: long}); err != nil {
		t.Fatal(err)
	}
	var st LabelStateRequest
	_ = json.Unmarshal([]byte(bodies["/api/link/v1/label-jobs/"+labelJobID+"/state"]), &st)
	if st.State != "failed" || st.Code != "offline" || len([]rune(st.Message)) != MaxLabelMessage {
		t.Fatalf("state body %+v", st)
	}
	if !strings.Contains(bodies["/api/link/v1/label-jobs/"+labelJobID+"/state"], `"bytes":0`) {
		t.Fatal("bytes must always be sent")
	}
	want := []string{
		"POST /api/link/v1/label-printers",
		"POST /api/link/v1/label-printers",
		"GET /api/link/v1/label-jobs?state=queued",
		"POST /api/link/v1/label-jobs/" + labelJobID + "/claim",
		"POST /api/link/v1/label-jobs/" + labelJobID + "/state",
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests\n%v\nwant\n%v", seen, want)
	}
}

func TestLabelCallsRefuseBadInput(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "0.5.0")
	ctx := context.Background()
	for _, id := range []string{"", "x", "../sync", labelJobID + "/../x", strings.ToUpper(labelJobID)} {
		if _, err := c.ClaimLabelJob(ctx, priv, "l", id); err == nil {
			t.Errorf("claim %q accepted", id)
		}
		if err := c.LabelJobState(ctx, priv, "l", id, LabelStateRequest{State: "printed"}); err == nil {
			t.Errorf("state %q accepted", id)
		}
	}
	for _, s := range []string{"", "claimed", "queued", "PRINTED"} {
		if err := c.LabelJobState(ctx, priv, "l", labelJobID, LabelStateRequest{State: s}); err == nil {
			t.Errorf("state %q accepted", s)
		}
	}
	if err := c.ReportLabelPrinters(ctx, priv, "l", LabelReport{Printers: make([]LabelPrinter, 101)}); err == nil {
		t.Error("oversized report accepted")
	}
	if calls != 0 {
		t.Fatalf("%d bad calls reached the server", calls)
	}
}

func TestClaimLabelJobAnswers(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	var answer func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		answer(w)
	}))
	defer srv.Close()
	u, _ := ParseServer(srv.URL)
	c := New(u, "0.5.0")
	ctx := context.Background()
	claim := func() error { _, err := c.ClaimLabelJob(ctx, priv, "l", labelJobID); return err }

	answer = func(w http.ResponseWriter) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"code":"not_queued"}`))
	}
	if err := claim(); !HasCode(err, CodeNotQueued) {
		t.Fatalf("409: %v", err)
	}
	// A 4x6 label at 203 dpi (~170 KB of base64) fits; more than 2 MB does not.
	page := func(n int) string {
		return `{"id":"` + labelJobID + `","printer_id":"p","format":"raster","pages":[{"width":812,"height":1218,"data_b64":"` + strings.Repeat("A", n) + `"}]}`
	}
	answer = func(w http.ResponseWriter) { _, _ = w.Write([]byte(page(170 << 10))) }
	if err := claim(); err != nil {
		t.Fatalf("4x6 page: %v", err)
	}
	answer = func(w http.ResponseWriter) { _, _ = w.Write([]byte(page(1500 << 10))) }
	if err := claim(); err != nil {
		t.Fatalf("1.5 MB job (over the 1 MB default limit) must fit: %v", err)
	}
	answer = func(w http.ResponseWriter) { _, _ = w.Write([]byte(page(MaxLabelJob))) }
	if err := claim(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("over 2 MB: %v", err)
	}
	if strings.Contains(func() string { err := claim(); return err.Error() }(), "AAAA") {
		t.Fatal("error quotes the payload")
	}
	answer = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"00000000-0000-4000-8000-000000000000","printer_id":"p","format":"raster","pages":[]}`))
	}
	if err := claim(); err == nil {
		t.Fatal("answer for another job accepted")
	}
	answer = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"` + labelJobID + `","printer_id":"p","format":"raster","pages":"AAAA"}`))
	}
	if err := claim(); err == nil {
		t.Fatal("pages that are not a list accepted")
	}
	// Other calls keep the 1 MB limit.
	answer = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jobs":[],"pad":"` + strings.Repeat("A", maxResponse) + `"}`))
	}
	if _, err := c.ListLabelJobs(ctx, priv, "l"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("list over 1 MB: %v", err)
	}
}

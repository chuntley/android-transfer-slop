package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testGUI(t *testing.T, deps guiDependencies) *guiServer {
	t.Helper()
	if deps.devices == nil {
		deps.devices = func(context.Context, string, time.Duration) ([]connectedDevice, error) {
			return []connectedDevice{{Serial: "phone", State: "device"}, {Serial: "locked", State: "unauthorized"}}, nil
		}
	}
	if deps.runner == nil {
		deps.runner = func(context.Context, config, io.Writer) error { return nil }
	}
	if deps.picker == nil {
		deps.picker = func(context.Context) (string, error) { return "", nil }
	}
	g, err := newGUIServer(context.Background(), config{adb: "trusted-adb", timeout: time.Minute}, "127.0.0.1:12345", deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	return g
}

func guiRequest(g *guiServer, method, route, body string) *http.Request {
	r := httptest.NewRequest(method, "http://"+g.host+route, strings.NewReader(body))
	r.Header.Set("X-Transfer-Token", g.token)
	r.Header.Set("Origin", "http://"+g.host)
	return r
}

func guiCall(g *guiServer, method, route, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	g.ServeHTTP(w, guiRequest(g, method, route, body))
	return w
}

func guiValidStart() guiStartRequest {
	return guiStartRequest{Serial: "phone", Sources: []string{"/sdcard/DCIM"}, Dest: "/tmp/android-transfer-test", BatchSize: 250, BatchBytes: 2 << 30}
}

func guiStartBody(t *testing.T, request guiStartRequest) string {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func guiAwaitState(t *testing.T, g *guiServer, expected string) guiStatus {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		w := guiCall(g, "GET", "/api/status", "")
		var status guiStatus
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.State == expected {
			return status
		}
		select {
		case <-deadline.C:
			t.Fatalf("state %q, want %q; error %q", status.State, expected, status.Error)
		case <-tick.C:
		}
	}
}

func TestGUIRejectsCrossSiteAndUnauthenticatedRequests(t *testing.T) {
	g := testGUI(t, guiDependencies{})
	for _, route := range []string{"/api/devices", "/api/browse", "/api/destination", "/api/start", "/api/stop", "/api/status", "/api/report", "/api/unknown"} {
		for _, change := range []struct {
			name  string
			apply func(*http.Request)
		}{
			{"host", func(r *http.Request) { r.Host = "attacker.example:12345" }},
			{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }},
			{"null-origin", func(r *http.Request) { r.Header.Set("Origin", "null") }},
			{"duplicate-origin", func(r *http.Request) { r.Header.Add("Origin", "https://attacker.example") }},
			{"missing-token", func(r *http.Request) { r.Header.Del("X-Transfer-Token") }},
			{"wrong-token", func(r *http.Request) { r.Header.Set("X-Transfer-Token", strings.Repeat("0", len(g.token))) }},
			{"duplicate-token", func(r *http.Request) { r.Header.Add("X-Transfer-Token", g.token) }},
		} {
			t.Run(route+"/"+change.name, func(t *testing.T) {
				r := guiRequest(g, "GET", route, "")
				change.apply(r)
				w := httptest.NewRecorder()
				g.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
	other := testGUI(t, guiDependencies{})
	r := guiRequest(g, "GET", "/api/status", "")
	r.Header.Set("X-Transfer-Token", other.token)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("another launch's token authorized a request")
	}
	for _, route := range []string{"/", "/gui.css", "/gui.js"} {
		r := guiRequest(g, "GET", route, "")
		r.Host = "attacker.example"
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("unprotected asset %s", route)
		}
	}
}

func TestGUIOnlyAllowsIntendedMethodsAndBoundedJSON(t *testing.T) {
	g := testGUI(t, guiDependencies{})
	for _, route := range []string{"/", "/gui.css", "/gui.js", "/api/devices", "/api/status", "/api/report"} {
		w := guiCall(g, "POST", route, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
			t.Fatalf("POST %s: %d", route, w.Code)
		}
	}
	for _, route := range []string{"/api/browse", "/api/destination", "/api/start", "/api/stop"} {
		w := guiCall(g, "GET", route, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Fatalf("GET %s: %d", route, w.Code)
		}
	}
	for _, route := range []string{"/api/browse", "/api/start"} {
		for _, body := range []string{"", "{", "null", "[]", "{} {}", `{"adb":"evil-executable"}`, `{"serial":5}`} {
			w := guiCall(g, "POST", route, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s body %q: %d", route, body, w.Code)
			}
		}
		w := guiCall(g, "POST", route, `{"serial":"`+strings.Repeat("x", guiBodyLimit)+`"}`)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized %s: %d", route, w.Code)
		}
	}
	for _, route := range []string{"/api/destination", "/api/stop"} {
		if w := guiCall(g, "POST", route, "{}"); w.Code != http.StatusBadRequest {
			t.Fatalf("unexpected accepted body: %s", route)
		}
	}
	if w := guiCall(g, "GET", "/api/status", strings.Repeat("x", guiBodyLimit+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status: %d", w.Code)
	}
}

func TestGUIValidatesStartBeforeRunning(t *testing.T) {
	var runs atomic.Int32
	g := testGUI(t, guiDependencies{runner: func(context.Context, config, io.Writer) error { runs.Add(1); return nil }})
	for _, test := range []struct {
		name   string
		change func(*guiStartRequest)
	}{
		{"no-device", func(r *guiStartRequest) { r.Serial = "" }},
		{"unknown-device", func(r *guiStartRequest) { r.Serial = "missing" }},
		{"unauthorized-device", func(r *guiStartRequest) { r.Serial = "locked" }},
		{"invalid-device", func(r *guiStartRequest) { r.Serial = "phone\nother" }},
		{"no-sources", func(r *guiStartRequest) { r.Sources = nil }},
		{"multiple-sources", func(r *guiStartRequest) { r.Sources = []string{"/sdcard/DCIM", "/sdcard/Pictures"} }},
		{"root-source", func(r *guiStartRequest) { r.Sources = []string{"/sdcard/.."} }},
		{"relative-source", func(r *guiStartRequest) { r.Sources = []string{"DCIM"} }},
		{"invalid-source", func(r *guiStartRequest) { r.Sources = []string{"/sdcard/DCIM\x00"} }},
		{"relative-destination", func(r *guiStartRequest) { r.Dest = "photos" }},
		{"invalid-destination", func(r *guiStartRequest) { r.Dest = "/tmp/photos\x00" }},
		{"zero-files", func(r *guiStartRequest) { r.BatchSize = 0 }},
		{"zero-bytes", func(r *guiStartRequest) { r.BatchBytes = 0 }},
		{"negative-batches", func(r *guiStartRequest) { r.MaxBatches = -1 }},
		{"conflicting-modes", func(r *guiStartRequest) { r.Verify, r.SafeDelete = true, true }},
		{"quick-conflicting-modes", func(r *guiStartRequest) { r.Verify, r.QuickDelete = true, true }},
		{"safe-and-quick-conflicting-modes", func(r *guiStartRequest) { r.SafeDelete, r.QuickDelete = true, true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := guiValidStart()
			test.change(&r)
			w := guiCall(g, "POST", "/api/start", guiStartBody(t, r))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if runs.Load() != 0 || g.snapshot().State != "idle" {
		t.Fatal("invalid request ran the engine or replaced status")
	}
}

func TestGUIStopWaitsForRunAndPreventsOverlap(t *testing.T) {
	started, canceled, release := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{})
	var runs atomic.Int32
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
		runs.Add(1)
		c.onProgress(transferProgress{Phase: "copying", Total: 4, Verified: 2, Copied: 1, Current: "/sdcard/DCIM/a.jpg"})
		fmt.Fprintln(out, "retained copy")
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		<-release
		return fmt.Errorf("transfer: %w", ctx.Err())
	}})
	t.Cleanup(func() { close(release) })
	body := guiStartBody(t, guiValidStart())
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	<-started
	for range 3 {
		if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusConflict {
			t.Fatalf("overlap: %d", w.Code)
		}
	}
	if w := guiCall(g, "POST", "/api/destination", ""); w.Code != http.StatusConflict {
		t.Fatal("picker overlapped transfer")
	}
	for range 3 {
		guiCall(g, "POST", "/api/stop", "")
	}
	<-canceled
	status := guiAwaitState(t, g, "stopping")
	if status.Progress.Copied != 1 || status.Logs[0] != "retained copy" {
		t.Fatalf("stop lost run status: %+v", status)
	}
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusConflict {
		t.Fatal("new run started before canceled run returned")
	}
	closed := make(chan struct{})
	go func() { g.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("shutdown returned before the engine finished")
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	status = guiAwaitState(t, g, "stopped")
	if status.Error != "" || runs.Load() != 1 {
		t.Fatalf("cancellation became failure or overlap: %+v", status)
	}
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusConflict {
		t.Fatal("server accepted work after shutdown")
	}
}

func TestGUIRunSurvivesRequestCancellationAndCanRestart(t *testing.T) {
	entered, release := make(chan context.Context, 2), make(chan struct{})
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
		entered <- ctx
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	t.Cleanup(func() { close(release) })
	body := guiStartBody(t, guiValidStart())
	requestCtx, cancel := context.WithCancel(context.Background())
	r := guiRequest(g, "POST", "/api/start", body).WithContext(requestCtx)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	cancel()
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	runCtx := <-entered
	if runCtx.Err() != nil {
		t.Fatal("closing request canceled transfer")
	}
	release <- struct{}{}
	guiAwaitState(t, g, "completed")
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	<-entered
	guiCall(g, "POST", "/api/stop", "")
	guiAwaitState(t, g, "stopped")
}

func TestGUIStatusIsolationReadOnlyAndBoundedLogs(t *testing.T) {
	var deviceCalls atomic.Int32
	g := testGUI(t, guiDependencies{devices: func(context.Context, string, time.Duration) ([]connectedDevice, error) {
		deviceCalls.Add(1)
		return nil, nil
	}})
	for i := range 10000 {
		fmt.Fprintf(g, "line %d\n", i)
	}
	_, _ = g.Write([]byte(strings.Repeat("x", guiLineLimit*4)))
	first := g.snapshot()
	if len(first.Logs) != guiLogLimit || first.Logs[0] != "line 9901" || len(first.Logs[len(first.Logs)-1]) != guiLineLimit {
		t.Fatalf("log retention failed: count=%d first=%q", len(first.Logs), first.Logs[0])
	}
	first.Logs[0] = "tampered"
	second := g.snapshot()
	if second.Logs[0] == "tampered" {
		t.Fatal("status exposes mutable backing slice")
	}
	_, _ = g.Write([]byte("ignored suffix\nnext"))
	if first.Logs[len(first.Logs)-1] != strings.Repeat("x", guiLineLimit-len(guiTruncation))+guiTruncation {
		t.Fatal("later writes changed an existing snapshot or truncation was unmarked")
	}
	for range 10 {
		w := guiCall(g, "GET", "/api/status", "")
		if w.Code != http.StatusOK || w.Body.Len() > (guiLogLimit*guiLineLimit*6)+1024 {
			t.Fatal("unbounded status response")
		}
	}
	if deviceCalls.Load() != 0 {
		t.Fatal("status polling executed a device command")
	}
}

func TestGUIReportsEngineFailureIncludingCleanupAfterStop(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
				fmt.Fprint(out, "last diagnostic")
				if stopped {
					<-ctx.Done()
					return errors.Join(ctx.Err(), errors.New("destination close failed"))
				}
				return errors.New("phone disconnected")
			}})
			if w := guiCall(g, "POST", "/api/start", guiStartBody(t, guiValidStart())); w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			if stopped {
				guiCall(g, "POST", "/api/stop", "")
			}
			status := guiAwaitState(t, g, "failed")
			expected := "phone disconnected"
			if stopped {
				expected = "destination close failed"
			}
			if !strings.Contains(status.Error, expected) || len(status.Logs) != 1 || status.Logs[0] != "last diagnostic" {
				t.Fatalf("failure lost: %+v", status)
			}
		})
	}
}

func TestGUIFolderChooserSerializationAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	g := testGUI(t, guiDependencies{picker: func(ctx context.Context) (string, error) { close(entered); <-ctx.Done(); return "", ctx.Err() }})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- guiCall(g, "POST", "/api/destination", "") }()
	<-entered
	if w := guiCall(g, "POST", "/api/destination", ""); w.Code != http.StatusConflict {
		t.Fatal("multiple native dialogs")
	}
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, guiValidStart())); w.Code != http.StatusConflict {
		t.Fatal("run overlapped native dialog")
	}
	g.close()
	select {
	case w := <-done:
		if w.Code != http.StatusBadRequest {
			t.Fatalf("picker cancellation: %d", w.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("picker leaked")
	}
	cancelled := testGUI(t, guiDependencies{})
	w := guiCall(cancelled, "POST", "/api/destination", "")
	var response struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusOK || response.Path != "" {
		t.Fatalf("chooser cancel must return empty path: %s", w.Body.String())
	}
}

func TestGUIBrowseIsBoundedAndUsesTrustedExecutable(t *testing.T) {
	var calls int
	g := testGUI(t, guiDependencies{browse: func(ctx context.Context, binary, serial, folder string, timeout time.Duration) ([]string, error) {
		calls++
		if binary != "trusted-adb" || serial != "phone" || folder != "/sdcard" || timeout > guiBrowseTimeout {
			t.Fatalf("invalid browse configuration: %q %q %q %s", binary, serial, folder, timeout)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > guiBrowseTimeout {
			t.Fatal("browse context is unbounded")
		}
		return []string{"/sdcard/DCIM"}, nil
	}})
	for _, body := range []string{`{"serial":"phone","path":"relative"}`, `{"serial":"","path":"/sdcard"}`, `{"serial":"phone","path":"/sdcard","adb":"evil"}`} {
		if w := guiCall(g, "POST", "/api/browse", body); w.Code != http.StatusBadRequest {
			t.Fatal(w.Body.String())
		}
	}
	w := guiCall(g, "POST", "/api/browse", `{"serial":"phone","path":"/sdcard/./"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/sdcard/DCIM") || calls != 1 {
		t.Fatalf("browse: %s, calls %d", w.Body.String(), calls)
	}
}

func TestGUISimultaneousStartsCannotRaceDeviceValidation(t *testing.T) {
	validating := make(chan struct{}, 2)
	release := make(chan struct{})
	var runs atomic.Int32
	g := testGUI(t, guiDependencies{
		devices: func(context.Context, string, time.Duration) ([]connectedDevice, error) {
			validating <- struct{}{}
			<-release
			return []connectedDevice{{Serial: "phone", State: "device"}}, nil
		},
		runner: func(ctx context.Context, c config, out io.Writer) error {
			runs.Add(1)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	body := guiStartBody(t, guiValidStart())
	responses := make(chan int, 2)
	for range 2 {
		go func() { responses <- guiCall(g, "POST", "/api/start", body).Code }()
	}
	for range 2 {
		select {
		case <-validating:
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not reach concurrent device validation")
		}
	}
	close(release)
	first, second := <-responses, <-responses
	if !((first == http.StatusOK && second == http.StatusConflict) || (second == http.StatusOK && first == http.StatusConflict)) {
		t.Fatalf("concurrent responses: %d, %d", first, second)
	}
	guiCall(g, "POST", "/api/stop", "")
	guiAwaitState(t, g, "stopped")
	if runs.Load() != 1 {
		t.Fatalf("engine ran %d times", runs.Load())
	}
}

func TestGUIAssetsDenyEmbeddingAndExposeOnlyLaunchToken(t *testing.T) {
	g := testGUI(t, guiDependencies{})
	for _, route := range []string{"/", "/gui.css", "/gui.js"} {
		r := guiRequest(g, "GET", route, "")
		r.Header.Del("X-Transfer-Token")
		r.Header.Del("Origin")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("asset %s: %d", route, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("unsafe asset headers: %v", w.Header())
		}
		policy := w.Header().Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(policy, directive) {
				t.Fatalf("missing security directive %q", directive)
			}
		}
		if strings.Contains(policy, "'unsafe-inline'") || strings.Contains(policy, "'unsafe-eval'") {
			t.Fatal("CSP allows inline execution")
		}
		if route == "/" && (!strings.Contains(w.Body.String(), g.token) || strings.Contains(w.Body.String(), "{{.Token}}")) {
			t.Fatal("launch token was not rendered")
		}
		if route != "/" && strings.Contains(w.Body.String(), g.token) {
			t.Fatal("token leaked into a static asset")
		}
	}
}

func TestGUIVerifyReportRetainsFullOutputDuringAndAfterRun(t *testing.T) {
	var output strings.Builder
	for i := range guiLogLimit * 3 {
		fmt.Fprintf(&output, "MISMATCH /sdcard/DCIM/photo-%d.jpg %s\n", i, strings.Repeat("x", guiLineLimit+10))
	}
	output.WriteString("unterminated diagnostic")
	prefix := output.String()
	const suffix = "\nChecked 300; mismatched 300; unprocessed 0\n"
	written, finish := make(chan struct{}), make(chan struct{})
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
		if _, err := io.WriteString(out, prefix); err != nil {
			return err
		}
		close(written)
		select {
		case <-finish:
			fmt.Fprint(out, suffix)
			return errors.New("verification found mismatches")
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	request := guiValidStart()
	request.Verify = true
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	<-written
	status := guiAwaitState(t, g, "running")
	if !status.ReportAvailable || len(status.Logs) != guiLogLimit {
		t.Fatalf("report not available or recent log unbounded: %+v", status)
	}
	for _, line := range status.Logs {
		if len(line) > guiLineLimit {
			t.Fatal("full report escaped into bounded recent logs")
		}
	}
	w := guiCall(g, "GET", "/api/report", "")
	if w.Code != http.StatusOK || w.Body.String() != prefix {
		t.Fatalf("in-progress report lost output: code=%d bytes=%d want=%d", w.Code, w.Body.Len(), len(prefix))
	}
	if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Content-Length") != fmt.Sprint(len(prefix)) {
		t.Fatalf("unsafe or unbounded report response: %v", w.Header())
	}
	if w := guiCall(g, "GET", "/api/report", "{}"); w.Code != http.StatusBadRequest {
		t.Fatal("report accepted a request body")
	}
	close(finish)
	status = guiAwaitState(t, g, "failed")
	if !status.ReportAvailable || !strings.Contains(status.Error, "mismatches") {
		t.Fatalf("runner failure discarded report or error: %+v", status)
	}
	w = guiCall(g, "GET", "/api/report", "")
	if w.Code != http.StatusOK || w.Body.String() != prefix+suffix {
		t.Fatalf("completed report lost output: code=%d bytes=%d", w.Code, w.Body.Len())
	}
}

type guiPausedReportWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *guiPausedReportWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestGUIReportResetAndShutdownPreserveExistingDownload(t *testing.T) {
	dir := t.TempDir()
	paths := make(chan string, 2)
	const reportText = "MISSING /sdcard/DCIM/missing.jpg\n"
	g := testGUI(t, guiDependencies{
		createReport: func() (*os.File, error) {
			file, err := os.CreateTemp(dir, "report-*")
			if err == nil {
				paths <- file.Name()
			}
			return file, err
		},
		runner: func(ctx context.Context, c config, out io.Writer) error {
			if c.verify {
				_, err := io.WriteString(out, reportText)
				return err
			}
			_, err := io.WriteString(out, "copy output must not become a report\n")
			return err
		},
	})
	request := guiValidStart()
	request.Verify = true
	body := guiStartBody(t, request)
	if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusNotFound {
		t.Fatal("report was available before verification")
	}
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	guiAwaitState(t, g, "completed")
	firstPath := <-paths
	invalid := request
	invalid.BatchSize = 0
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, invalid)); w.Code != http.StatusBadRequest {
		t.Fatal("invalid start was accepted")
	}
	if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusOK || w.Body.String() != reportText {
		t.Fatal("rejected start discarded the report")
	}
	download := &guiPausedReportWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(download.release) })
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(download, guiRequest(g, "GET", "/api/report", ""))
		close(done)
	}()
	select {
	case <-download.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("download never started")
	}
	request.Verify = false
	copyBody := guiStartBody(t, request)
	started := make(chan *httptest.ResponseRecorder, 1)
	go func() { started <- guiCall(g, "POST", "/api/start", copyBody) }()
	select {
	case w := <-started:
		if w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow download blocked a new run")
	}
	if status := guiAwaitState(t, g, "completed"); status.ReportAvailable {
		t.Fatal("copy run retained a verification report")
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next accepted run did not remove report: %v", err)
	}
	if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusNotFound {
		t.Fatal("copy output was offered as a verification report")
	}
	if w := guiCall(g, "POST", "/api/start", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	guiAwaitState(t, g, "completed")
	secondPath := <-paths
	g.close()
	if _, err := os.Stat(secondPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown did not remove report: %v", err)
	}
	if g.snapshot().ReportAvailable {
		t.Fatal("shutdown advertised a deleted report")
	}
	download.release <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not finish after reset and shutdown")
	}
	if download.Code != http.StatusOK || download.Body.String() != reportText {
		t.Fatal("reset or shutdown interrupted an already-open download")
	}
}

func TestGUIReportCaptureErrorsRemainVisibleWithoutStoppingRunner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		createFailure bool
		runnerFailure bool
		stop          bool
	}{
		{name: "create", createFailure: true},
		{name: "write-and-engine-error", runnerFailure: true},
		{name: "write-and-cancel", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			written, finish := make(chan struct{}), make(chan struct{})
			g := testGUI(t, guiDependencies{
				createReport: func() (*os.File, error) {
					if tc.createFailure {
						return nil, errors.New("temporary directory is not writable")
					}
					file, err := os.CreateTemp(dir, "report-*")
					if err == nil {
						err = file.Close()
					}
					return file, err
				},
				runner: func(ctx context.Context, c config, out io.Writer) error {
					for i := range guiLogLimit * 2 {
						if _, err := fmt.Fprintf(out, "checked %d\n", i); err != nil {
							return err
						}
					}
					close(written)
					select {
					case <-finish:
						if tc.runnerFailure {
							return errors.New("phone disconnected")
						}
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			request := guiValidStart()
			request.Verify = true
			if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			select {
			case <-written:
			case <-time.After(2 * time.Second):
				t.Fatal("report capture error stopped runner output")
			}
			status := guiAwaitState(t, g, "running")
			if status.ReportAvailable || !strings.Contains(status.Error, "report unavailable") || status.Logs[len(status.Logs)-1] != fmt.Sprintf("checked %d", guiLogLimit*2-1) {
				t.Fatalf("report failure was hidden or stopped verification: %+v", status)
			}
			if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusNotFound {
				t.Fatal("incomplete capture was downloadable")
			}
			state := "failed"
			if tc.stop {
				guiCall(g, "POST", "/api/stop", "")
				state = "stopped"
			} else {
				close(finish)
			}
			status = guiAwaitState(t, g, state)
			if !strings.Contains(status.Error, "report unavailable") || (tc.runnerFailure && !strings.Contains(status.Error, "phone disconnected")) {
				t.Fatalf("report or runner error was discarded: %+v", status)
			}
			g.close()
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("failed capture leaked a temporary file: %v, %v", files, err)
			}
		})
	}
}

func TestGUIReportFlushAndReadFailuresDisableDownload(t *testing.T) {
	for _, flushFailure := range []bool{true, false} {
		t.Run(fmt.Sprintf("flush=%v", flushFailure), func(t *testing.T) {
			dir := t.TempDir()
			var report *os.File
			g := testGUI(t, guiDependencies{
				createReport: func() (*os.File, error) {
					var err error
					report, err = os.CreateTemp(dir, "report-*")
					return report, err
				},
				runner: func(ctx context.Context, c config, out io.Writer) error {
					if _, err := io.WriteString(out, "checked photo.jpg\n"); err != nil {
						return err
					}
					if flushFailure {
						return report.Close()
					}
					return nil
				},
			})
			request := guiValidStart()
			request.Verify = true
			if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			operation := "flush temporary report"
			if !flushFailure {
				guiAwaitState(t, g, "completed")
				if err := os.Remove(report.Name()); err != nil {
					t.Fatal(err)
				}
				if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusInternalServerError {
					t.Fatalf("unreadable report returned %d", w.Code)
				}
				operation = "open temporary report"
			}
			status := guiAwaitState(t, g, "failed")
			if status.ReportAvailable || !strings.Contains(status.Error, operation) {
				t.Fatalf("report failure was not visible: %+v", status)
			}
			if w := guiCall(g, "GET", "/api/report", ""); w.Code != http.StatusNotFound {
				t.Fatal("failed report remained downloadable")
			}
		})
	}
}

func TestGUISafeDeleteRequiresExplicitModeAndFreshVerification(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, cfg config, out io.Writer) error {
		return runWithDevice(ctx, cfg, out, d)
	}})
	request := guiValidStart()
	request.Dest = c.dest
	start := func(state string) guiStatus {
		t.Helper()
		if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
			t.Fatalf("start status %d: %s", w.Code, w.Body.String())
		}
		return guiAwaitState(t, g, state)
	}
	start("completed") // Transfer creates a verified destination, not permission to delete.
	request.Verify = true
	start("completed")
	if len(d.files) != 1 || len(d.attempts) != 0 {
		t.Fatal("transfer or verify-only deleted source files")
	}
	if err := os.WriteFile(savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("corrupted after verification"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Verify, request.SafeDelete = false, true
	status := start("failed")
	if status.Progress.Deleted != 0 || status.Progress.Mismatched != 1 || len(d.files) != 1 || len(d.attempts) != 0 {
		t.Fatalf("previous transfer/verification authorized a stale deletion: %+v", status)
	}
	report := guiCall(g, "GET", "/api/report", "")
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), "/sdcard/DCIM/photo.jpg") || !strings.Contains(report.Body.String(), "MISMATCH") {
		t.Fatalf("retained source missing from deletion report: %d %s", report.Code, report.Body.String())
	}
	request.SafeDelete = false
	start("completed") // Explicit transfer repairs the local corruption.
	request.SafeDelete = true
	status = start("completed")
	if status.Progress.Deleted != 1 || len(d.files) != 0 {
		t.Fatalf("fresh matching copy was not safely deleted: %+v", status)
	}
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("irreplaceable photo"))
}

func TestGUIQuickDeleteUsesMetadataChecksAndPreservesDestination(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	target := savedPath(c, "/sdcard/DCIM/photo.jpg")
	replacement := bytes.Repeat([]byte{'q'}, len(d.files["/sdcard/DCIM/photo.jpg"]))
	if err := os.WriteFile(target, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, time.Unix(d.mtime, 0), time.Unix(d.mtime, 0)); err != nil {
		t.Fatal(err)
	}
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, cfg config, out io.Writer) error {
		return runWithDevice(ctx, cfg, out, d)
	}})
	request := guiValidStart()
	request.Dest = c.dest
	request.QuickDelete = true
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
		t.Fatalf("start status %d: %s", w.Code, w.Body.String())
	}
	status := guiAwaitState(t, g, "completed")
	if !status.Settings.QuickDelete || status.Progress.Verified != 1 || status.Progress.Deleted != 1 || len(d.files) != 0 {
		t.Fatalf("quick delete was not reported or completed: %+v", status)
	}
	assertContent(t, target, replacement)
	report := guiCall(g, "GET", "/api/report", "")
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), "contents were not hashed") {
		t.Fatalf("quick deletion disclosure missing: %d %s", report.Code, report.Body.String())
	}
}

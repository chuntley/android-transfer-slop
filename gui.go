package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed gui.html gui.css gui.js
var guiAssets embed.FS

const (
	guiBodyLimit       = 64 << 10
	guiLogLimit        = 100
	guiLineLimit       = 1024
	guiStatusTextLimit = 4096
	guiTruncation      = "... [truncated]"
	guiBrowseTimeout   = 30 * time.Second
)

type guiDependencies struct {
	runner       func(context.Context, config, io.Writer) error
	picker       func(context.Context) (string, error)
	devices      func(context.Context, string, time.Duration) ([]connectedDevice, error)
	browse       func(context.Context, string, string, string, time.Duration) ([]string, error)
	createReport func() (*os.File, error)
}

type guiStatus struct {
	State           string           `json:"state"`
	Progress        transferProgress `json:"progress"`
	Error           string           `json:"error"`
	Logs            []string         `json:"logs"`
	RunID           uint64           `json:"runId"`
	Settings        *guiStartRequest `json:"settings,omitempty"`
	ReportAvailable bool             `json:"reportAvailable"`
}

type guiServer struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	config                   config
	host, token              string
	html                     []byte
	deps                     guiDependencies
	mu                       sync.Mutex
	status                   guiStatus
	pending                  string
	report                   *os.File
	reportSize               int64
	reportErr                error
	active, picking, closing bool
	runCancel                context.CancelFunc
	work                     sync.WaitGroup
}

func newGUIServer(ctx context.Context, c config, host string, deps guiDependencies) (*guiServer, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret[:])
	tmpl, err := template.ParseFS(guiAssets, "gui.html")
	if err != nil {
		return nil, err
	}
	var html bytes.Buffer
	if err := tmpl.Execute(&html, struct{ Token string }{token}); err != nil {
		return nil, err
	}
	if deps.runner == nil {
		deps.runner = run
	}
	if deps.createReport == nil {
		deps.createReport = func() (*os.File, error) {
			return os.CreateTemp("", "android-transfer-slop-report-*.txt")
		}
	}
	if deps.picker == nil {
		deps.picker = pickDestination
	}
	if deps.devices == nil {
		deps.devices = listDevices
	}
	if deps.browse == nil {
		deps.browse = func(ctx context.Context, binary, serial, folder string, timeout time.Duration) ([]string, error) {
			d := newADB(binary, serial, timeout)
			if _, err := d.Serial(ctx); err != nil {
				return nil, err
			}
			return d.ListDirectories(ctx, folder)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	return &guiServer{ctx: ctx, cancel: cancel, config: c, host: host, token: token, html: html.Bytes(), deps: deps, status: guiStatus{State: "idle", Logs: []string{}}}, nil
}

func serveGUI(ctx context.Context, c config, out io.Writer) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	g, err := newGUIServer(ctx, c, listener.Addr().String(), guiDependencies{})
	if err != nil {
		listener.Close()
		return err
	}
	defer g.close()
	server := &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return g.ctx }}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	url := "http://" + g.host + "/"
	fmt.Fprintln(out, "Android Transfer SLOP: "+url)
	if !c.noOpen {
		if err := openGUIBrowser(g.ctx, url); err != nil {
			fmt.Fprintf(out, "Could not open a browser (%v). Open %s manually.\n", err, url)
		}
	}
	select {
	case <-ctx.Done():
		err = nil
	case err = <-served:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	g.close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		server.Close()
		return errors.Join(err, shutdownErr)
	}
	return err
}

func openGUIBrowser(ctx context.Context, url string) error {
	binary := "xdg-open"
	if runtime.GOOS == "darwin" {
		binary = "open"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, url)
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func pickDestination(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("native folder selection is available on macOS; enter an absolute destination path manually")
	}
	const script = `try
return POSIX path of (choose folder with prompt "Choose the local Android Transfer SLOP destination")
on error number -128
return ""
end try`
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script)
	cmd.WaitDelay = time.Second
	var stdout, stderr adbLimitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("folder chooser: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// Remove only AppleScript's output newline; spaces can be part of a folder name.
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

func (g *guiServer) close() {
	g.mu.Lock()
	g.closing = true
	g.cancel()
	if g.runCancel != nil {
		g.runCancel()
		g.status.State = "stopping"
	}
	g.mu.Unlock()
	g.work.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.discardReport(); err != nil {
		g.failReport(err)
	}
}

func guiJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func guiError(w http.ResponseWriter, code int, err error) {
	guiJSON(w, code, map[string]string{"error": err.Error()})
}

func guiDecode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, guiBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(target)
	if err == nil {
		var extra any
		if next := dec.Decode(&extra); next != io.EOF {
			if next == nil {
				err = errors.New("request must contain one JSON object")
			} else {
				err = next
			}
		}
	}
	if err != nil {
		code := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		guiError(w, code, fmt.Errorf("invalid request: %w", err))
		return false
	}
	return true
}

func guiEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, guiBodyLimit)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(data)) != 0 {
		if err == nil {
			err = errors.New("this endpoint does not accept a request body")
		}
		code := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		guiError(w, code, err)
		return false
	}
	return true
}

func (g *guiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	if r.Host != g.host {
		guiError(w, http.StatusForbidden, errors.New("invalid host"))
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || (len(origins) == 1 && origins[0] != "http://"+g.host) {
		guiError(w, http.StatusForbidden, errors.New("invalid origin"))
		return
	}
	api := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api"
	if api {
		tokens := r.Header.Values("X-Transfer-Token")
		if len(tokens) != 1 || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(g.token)) != 1 {
			guiError(w, http.StatusForbidden, errors.New("invalid transfer token"))
			return
		}
	}
	method := "GET"
	switch r.URL.Path {
	case "/", "/gui.css", "/gui.js", "/api/devices", "/api/status", "/api/report":
	case "/api/browse", "/api/destination", "/api/start", "/api/stop":
		method = "POST"
	default:
		guiError(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		guiError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	switch r.URL.Path {
	case "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(g.html)
	case "/gui.css", "/gui.js":
		asset, err := guiAssets.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			guiError(w, http.StatusInternalServerError, err)
			return
		}
		contentType := "text/css; charset=utf-8"
		if r.URL.Path == "/gui.js" {
			contentType = "text/javascript; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(asset)
	case "/api/status":
		if guiEmptyBody(w, r) {
			guiJSON(w, http.StatusOK, g.snapshot())
		}
	case "/api/report":
		if guiEmptyBody(w, r) {
			g.handleReport(w, r)
		}
	case "/api/devices":
		if !guiEmptyBody(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), g.commandTimeout())
		defer cancel()
		devices, err := g.deps.devices(ctx, g.config.adb, g.commandTimeout())
		if err != nil {
			guiError(w, http.StatusBadGateway, err)
			return
		}
		if devices == nil {
			devices = []connectedDevice{}
		}
		guiJSON(w, http.StatusOK, map[string]any{"devices": devices})
	case "/api/browse":
		g.handleBrowse(w, r)
	case "/api/destination":
		g.handleDestination(w, r)
	case "/api/start":
		g.handleStart(w, r)
	case "/api/stop":
		if !guiEmptyBody(w, r) {
			return
		}
		g.mu.Lock()
		if g.runCancel != nil {
			g.runCancel()
			g.status.State = "stopping"
		}
		g.mu.Unlock()
		guiJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (g *guiServer) commandTimeout() time.Duration {
	if g.config.timeout <= 0 || g.config.timeout > guiBrowseTimeout {
		return guiBrowseTimeout
	}
	return g.config.timeout
}

func validGUISerial(serial string) bool {
	return serial != "" && len(serial) <= 1024 && strings.IndexFunc(serial, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func (g *guiServer) handleBrowse(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Serial string `json:"serial"`
		Path   string `json:"path"`
	}
	if !guiDecode(w, r, &request) {
		return
	}
	if !validGUISerial(request.Serial) || !path.IsAbs(request.Path) || strings.ContainsAny(request.Path, "\x00\r\n") {
		guiError(w, http.StatusBadRequest, errors.New("select a device and an absolute device folder"))
		return
	}
	request.Path = path.Clean(request.Path)
	ctx, cancel := context.WithTimeout(r.Context(), g.commandTimeout())
	defer cancel()
	folders, err := g.deps.browse(ctx, g.config.adb, request.Serial, request.Path, g.commandTimeout())
	if err != nil {
		guiError(w, http.StatusBadGateway, err)
		return
	}
	if folders == nil {
		folders = []string{}
	}
	guiJSON(w, http.StatusOK, map[string]any{"path": request.Path, "folders": folders})
}

func (g *guiServer) handleDestination(w http.ResponseWriter, r *http.Request) {
	if !guiEmptyBody(w, r) {
		return
	}
	g.mu.Lock()
	if g.closing || g.active || g.picking {
		g.mu.Unlock()
		guiError(w, http.StatusConflict, errors.New("a transfer or folder chooser is already active"))
		return
	}
	g.picking = true
	g.work.Add(1)
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.picking = false; g.mu.Unlock(); g.work.Done() }()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	dest, err := g.deps.picker(ctx)
	if err != nil {
		guiError(w, http.StatusBadRequest, err)
		return
	}
	guiJSON(w, http.StatusOK, map[string]string{"path": dest})
}

type guiStartRequest struct {
	Serial      string   `json:"serial"`
	Sources     []string `json:"sources"`
	Dest        string   `json:"dest"`
	BatchSize   int      `json:"batchSize"`
	BatchBytes  int64    `json:"batchBytes"`
	MaxBatches  int      `json:"maxBatches"`
	Verify      bool     `json:"verify"`
	SafeDelete  bool     `json:"safeDelete"`
	QuickDelete bool     `json:"quickDelete"`
}

func (g *guiServer) handleStart(w http.ResponseWriter, r *http.Request) {
	var request guiStartRequest
	if !guiDecode(w, r, &request) {
		return
	}
	if (request.Verify && (request.SafeDelete || request.QuickDelete)) || (request.SafeDelete && request.QuickDelete) {
		guiError(w, http.StatusBadRequest, errors.New("choose verification, safe source deletion, or quick source deletion, not multiple modes"))
		return
	}
	if !validGUISerial(request.Serial) || len(request.Sources) != 1 || !filepath.IsAbs(request.Dest) || strings.ContainsAny(request.Dest, "\x00\r\n") || request.BatchSize < 1 || request.BatchBytes < 1 || request.MaxBatches < 0 {
		guiError(w, http.StatusBadRequest, errors.New("select a device and exactly one source folder, enter an absolute destination, and use positive batch limits and a nonnegative batch count"))
		return
	}
	c := g.config
	c.sources = nil
	if err := c.sources.Set(request.Sources[0]); err != nil {
		guiError(w, http.StatusBadRequest, err)
		return
	}
	c.serial, c.dest = request.Serial, filepath.Clean(request.Dest)
	c.batchSize, c.batchBytes, c.maxBatches, c.verify = request.BatchSize, request.BatchBytes, request.MaxBatches, request.Verify
	c.safeDelete, c.quickDelete = request.SafeDelete, request.QuickDelete
	g.mu.Lock()
	if g.closing || g.active || g.picking {
		g.mu.Unlock()
		guiError(w, http.StatusConflict, errors.New("a transfer or folder chooser is already active"))
		return
	}
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), g.commandTimeout())
	devices, err := g.deps.devices(ctx, c.adb, g.commandTimeout())
	cancel()
	if err != nil {
		guiError(w, http.StatusBadGateway, err)
		return
	}
	selected := false
	for _, device := range devices {
		if device.Serial == c.serial && device.State == "device" {
			selected = true
			break
		}
	}
	if !selected {
		guiError(w, http.StatusBadRequest, errors.New("selected device is not connected and authorized; refresh the device list"))
		return
	}
	g.mu.Lock()
	if g.closing || g.active || g.picking {
		g.mu.Unlock()
		guiError(w, http.StatusConflict, errors.New("a transfer or folder chooser is already active"))
		return
	}
	if err := g.discardReport(); err != nil {
		g.failReport(err)
		g.mu.Unlock()
		guiError(w, http.StatusInternalServerError, err)
		return
	}
	runCtx, runCancel := context.WithCancel(g.ctx)
	g.active, g.runCancel = true, runCancel
	request.Sources = append([]string(nil), c.sources...)
	request.Dest = c.dest
	g.status = guiStatus{State: "running", Logs: []string{}, RunID: g.status.RunID + 1, Settings: &request}
	g.pending = ""
	g.reportErr = nil
	if c.verify || c.safeDelete || c.quickDelete {
		var err error
		g.report, err = g.deps.createReport()
		if err != nil {
			g.failReport(fmt.Errorf("create temporary report: %w", err))
		} else {
			g.status.ReportAvailable = true
		}
	}
	c.onProgress = func(progress transferProgress) {
		progress.Phase = guiBoundText(progress.Phase)
		progress.Current = guiBoundText(progress.Current)
		g.mu.Lock()
		g.status.Progress = progress
		g.mu.Unlock()
	}
	g.work.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.work.Done()
		defer runCancel()
		err := g.deps.runner(runCtx, c, g)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.pending != "" {
			g.appendLog(g.pending)
			g.pending = ""
		}
		if g.report != nil && g.reportErr == nil {
			if syncErr := g.report.Sync(); syncErr != nil {
				g.failReport(fmt.Errorf("flush temporary report: %w", syncErr))
			}
		}
		g.active, g.runCancel = false, nil
		g.status.State = "completed"
		if runCtx.Err() != nil && (err == nil || guiCancellationOnly(err)) {
			g.status.State = "stopped"
			err = nil
		} else if err != nil {
			g.status.State = "failed"
		}
		if g.reportErr != nil {
			err = errors.Join(err, g.reportErr)
			if g.status.State == "completed" {
				g.status.State = "failed"
			}
		}
		if err != nil {
			g.status.Error = guiBoundText(err.Error())
		}
	}()
	guiJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (g *guiServer) handleReport(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if !g.status.ReportAvailable || g.closing {
		g.mu.Unlock()
		guiError(w, http.StatusNotFound, errors.New("no run report is available"))
		return
	}
	// Open a separate reader before releasing mu. An accepted restart or shutdown
	// can then unlink the report without interrupting an existing download.
	report, err := os.Open(g.report.Name())
	if err != nil {
		g.failReport(fmt.Errorf("open temporary report: %w", err))
		g.mu.Unlock()
		guiError(w, http.StatusInternalServerError, err)
		return
	}
	size, runID := g.reportSize, g.status.RunID
	reportKind := "verify"
	if g.status.Settings != nil && (g.status.Settings.SafeDelete || g.status.Settings.QuickDelete) {
		reportKind = "delete"
	}
	g.mu.Unlock()
	defer report.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="android-transfer-slop-%s-%d.txt"`, reportKind, runID))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	// Bound the download to the bytes captured at request time, even while the
	// runner appends. Content-Length makes an interrupted download detectable.
	_, _ = io.Copy(w, io.NewSectionReader(report, 0, size))
}

// discardReport and failReport are called only under mu.
func (g *guiServer) discardReport() error {
	g.status.ReportAvailable = false
	if g.report == nil {
		return nil
	}
	closeErr := g.report.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	removeErr := os.Remove(g.report.Name())
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		g.report, g.reportSize = nil, 0
	}
	return errors.Join(closeErr, removeErr)
}

func (g *guiServer) failReport(err error) {
	if g.reportErr != nil {
		return
	}
	g.reportErr = fmt.Errorf("verification report unavailable; resolve the temporary-file error and run verification again: %w", err)
	g.status.ReportAvailable = false
	text := g.reportErr.Error()
	if g.status.Error != "" {
		text = g.status.Error + "\n" + text
	}
	g.status.Error = guiBoundText(text)
	if g.status.State == "completed" {
		g.status.State = "failed"
	}
	line := g.reportErr.Error()
	if len(line) > guiLineLimit {
		line = line[:guiLineLimit-len(guiTruncation)] + guiTruncation
	}
	g.appendLog(line)
}

func guiBoundText(text string) string {
	if len(text) > guiStatusTextLimit {
		return text[:guiStatusTextLimit-len(guiTruncation)] + guiTruncation
	}
	return text
}

// A cleanup failure joined to cancellation is still a real failure.
func guiCancellationOnly(err error) bool {
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !guiCancellationOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return guiCancellationOnly(wrapped.Unwrap())
	}
	return false
}

func (g *guiServer) snapshot() guiStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	status := g.status
	if status.Settings != nil {
		settings := *status.Settings
		settings.Sources = append([]string(nil), settings.Sources...)
		status.Settings = &settings
	}
	status.Logs = make([]string, len(g.status.Logs), guiLogLimit+1)
	copy(status.Logs, g.status.Logs)
	if g.pending != "" {
		status.Logs = append(status.Logs, g.pending)
	}
	if len(status.Logs) > guiLogLimit {
		status.Logs = status.Logs[len(status.Logs)-guiLogLimit:]
	}
	return status
}

// appendLog is called only under mu. Memory is bounded even for an unterminated line.
func (g *guiServer) appendLog(line string) {
	if len(g.status.Logs) == guiLogLimit {
		copy(g.status.Logs, g.status.Logs[1:])
		g.status.Logs = g.status.Logs[:guiLogLimit-1]
	}
	g.status.Logs = append(g.status.Logs, line)
}

func (g *guiServer) Write(p []byte) (int, error) {
	n := len(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.report != nil && g.reportErr == nil {
		written, err := g.report.Write(p)
		g.reportSize += int64(written)
		if err == nil && written != n {
			err = io.ErrShortWrite
		}
		if err != nil {
			g.failReport(fmt.Errorf("write temporary report: %w", err))
		}
	}
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		remaining := guiLineLimit - len(g.pending)
		if len(part) > remaining {
			if remaining > 0 {
				g.pending += string(part[:remaining])
			}
			if !strings.HasSuffix(g.pending, guiTruncation) {
				g.pending = g.pending[:guiLineLimit-len(guiTruncation)] + guiTruncation
			}
		} else {
			g.pending += string(part)
		}
		if end < 0 {
			break
		}
		g.appendLog(g.pending)
		g.pending = ""
		p = p[end+1:]
	}
	return n, nil
}

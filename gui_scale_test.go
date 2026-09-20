package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGUIProgressFloodRemainsControllable(t *testing.T) {
	flooded := make(chan struct{})
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
		for i := range 50000 {
			if err := ctx.Err(); err != nil {
				return err
			}
			c.report(transferProgress{Phase: "transferring", Total: 50000, Verified: i + 1, Copied: i + 1, Current: fmt.Sprintf("/sdcard/DCIM/photo-%d.jpg", i)})
			fmt.Fprintf(out, "Verified photo-%d.jpg\n", i)
		}
		close(flooded)
		<-ctx.Done()
		return ctx.Err()
	}})
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, guiValidStart())); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	// Poll while the worker floods progress; responses must stay bounded and
	// the worker must never hold the control-plane mutex for its whole run.
	for range 100 {
		w := guiCall(g, "GET", "/api/status", "")
		if w.Code != http.StatusOK || w.Body.Len() > 120000 {
			t.Fatalf("invalid or unbounded status: code=%d bytes=%d", w.Code, w.Body.Len())
		}
	}
	select {
	case <-flooded:
	case <-time.After(10 * time.Second):
		t.Fatal("progress worker stalled while status was polled")
	}
	status := g.snapshot()
	if status.Progress.Verified != 50000 || len(status.Logs) != 100 || !strings.Contains(status.Logs[99], "49999") {
		t.Fatalf("large-run progress not retained correctly: %+v", status.Progress)
	}
	if w := guiCall(g, "POST", "/api/stop", ""); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	guiAwaitState(t, g, "stopped")
}

func BenchmarkGUIStatusAfter50000Files(b *testing.B) {
	g, err := newGUIServer(context.Background(), config{}, "127.0.0.1:12345", guiDependencies{})
	if err != nil {
		b.Fatal(err)
	}
	defer g.close()
	for i := range 50000 {
		fmt.Fprintf(g, "Verified /sdcard/DCIM/photo-%05d.jpg\n", i)
	}
	b.ReportAllocs()
	for b.Loop() {
		w := guiCall(g, "GET", "/api/status", "")
		if w.Code != http.StatusOK || w.Body.Len() > 120000 {
			b.Fatal("invalid status response")
		}
	}
}

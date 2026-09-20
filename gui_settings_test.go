package main

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestGUIRejectsTransferFlagsInsteadOfIgnoringThem(t *testing.T) {
	for _, option := range [][]string{
		{"-dest", "/tmp/photos"}, {"-source", "/sdcard/Pictures"}, {"-serial", "phone"},
		{"-batch-size", "10"}, {"-batch-bytes", "1024"}, {"-max-batches", "1"}, {"-verify"},
	} {
		t.Run(option[0], func(t *testing.T) {
			if _, err := parseConfig(append([]string{"-gui"}, option...), io.Discard); err == nil {
				t.Fatal("GUI silently accepted a setting that the form would replace")
			}
		})
	}
	if _, err := parseConfig([]string{"-gui", "-no-open", "-adb", "/custom/adb", "-timeout", "30m"}, io.Discard); err != nil {
		t.Fatalf("GUI server options should remain supported: %v", err)
	}
}

func TestGUIRetainsAcceptedRunSettingsForReload(t *testing.T) {
	g := testGUI(t, guiDependencies{runner: func(ctx context.Context, c config, out io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	request := guiValidStart()
	request.Sources = []string{"/sdcard/Pictures/"}
	request.Dest = "/tmp/photos/../chosen"
	request.BatchSize, request.BatchBytes, request.MaxBatches, request.Verify = 10, 1024, 1, true
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	first := guiAwaitState(t, g, "running")
	want := request
	want.Sources, want.Dest = []string{"/sdcard/Pictures"}, "/tmp/chosen"
	if first.RunID == 0 || !reflect.DeepEqual(first.Settings, &want) {
		t.Fatalf("reload cannot restore accepted settings: %+v", first)
	}
	snapshot := g.snapshot()
	snapshot.Settings.Sources[0] = "/changed/by/caller"
	snapshot.Settings.Dest = "/changed/by/caller"
	if !reflect.DeepEqual(g.snapshot().Settings, &want) {
		t.Fatal("caller mutated retained settings")
	}
	guiCall(g, "POST", "/api/stop", "")
	stopped := guiAwaitState(t, g, "stopped")
	if !reflect.DeepEqual(stopped.Settings, &want) || stopped.RunID != first.RunID {
		t.Fatal("stopping discarded settings needed to resume")
	}
	if w := guiCall(g, "POST", "/api/start", guiStartBody(t, request)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	next := guiAwaitState(t, g, "running")
	if next.RunID <= first.RunID {
		t.Fatal("browser cannot distinguish restarted run from previous run")
	}
	guiCall(g, "POST", "/api/stop", "")
	guiAwaitState(t, g, "stopped")
}

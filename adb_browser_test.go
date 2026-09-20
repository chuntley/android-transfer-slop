package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDeviceDiscoveryIncludesAuthorizationState(t *testing.T) {
	d, _ := setupADBFixture(t, adbFixture{Output: "List of devices attached\nzzz unauthorized usb:1\naaa device product:test model:Galaxy_S23 device:test\n"}, 15*time.Second)
	got, err := listDevices(context.Background(), d.binary, 15*time.Second)
	want := []connectedDevice{{Serial: "aaa", State: "device", Model: "Galaxy S23"}, {Serial: "zzz", State: "unauthorized"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("devices=%+v, err=%v", got, err)
	}
}

func TestDeviceDiscoveryReportsMissingAndMalformedOutput(t *testing.T) {
	for _, data := range []string{"unexpected", "s device\ns device\n"} {
		d, _ := setupADBFixture(t, adbFixture{Output: data}, 15*time.Second)
		if _, err := listDevices(context.Background(), d.binary, 15*time.Second); err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("expected malformed device output for %q, got %v", data, err)
		}
	}
	d, _ := setupADBFixture(t, adbFixture{Output: "List of devices attached\n\n"}, 15*time.Second)
	got, err := listDevices(context.Background(), d.binary, 15*time.Second)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty discovery=%v %v", got, err)
	}
	if _, err := listDevices(context.Background(), filepath.Join(t.TempDir(), "missing-adb"), time.Second); err == nil {
		t.Fatal("missing executable not reported")
	}
}

func TestDirectoryBrowserListsOnlyImmediateRealFolders(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "photos '\n雪")
	if err := os.MkdirAll(filepath.Join(child, "grandchild"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(child, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	d, _ := boundADBFixture(t, adbFixture{Shell: true})
	got, err := d.ListDirectories(context.Background(), root)
	if err != nil || !reflect.DeepEqual(got, []string{child}) {
		t.Fatalf("folders=%v, err=%v", got, err)
	}
	if _, err := d.ListDirectories(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing folder accepted")
	}
	alias := filepath.Join(t.TempDir(), "sdcard")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	got, err = d.ListDirectories(context.Background(), alias)
	if err != nil || !reflect.DeepEqual(got, []string{filepath.Join(alias, filepath.Base(child))}) {
		t.Fatalf("alias folders=%v, err=%v", got, err)
	}
}

func TestDirectoryBrowserRejectsEscapingOrMalformedListings(t *testing.T) {
	for _, output := range []string{"/photos/child", "\x00", "/outside/x\x00", "/photos/child/nested\x00", "/photos\x00", "relative\x00"} {
		d, _ := boundADBFixture(t, adbFixture{Output: output})
		if _, err := d.ListDirectories(context.Background(), "/photos"); err == nil {
			t.Fatalf("accepted listing %q", output)
		}
	}
	d, _ := boundADBFixture(t, adbFixture{Output: "/photos/b\x00/photos/a\x00/photos/a\x00"})
	got, err := d.ListDirectories(context.Background(), "/photos")
	if err != nil || !reflect.DeepEqual(got, []string{"/photos/a", "/photos/b"}) {
		t.Fatalf("deduplication=%v %v", got, err)
	}
	if _, err := d.ListDirectories(context.Background(), "/photos/../escape"); err == nil {
		t.Fatal("unclean browser path accepted")
	}
}

func TestGUIConfigNeedsNoDestinationAtLaunch(t *testing.T) {
	c, err := parseConfig([]string{"-gui", "-no-open"}, io.Discard)
	if err != nil || !c.gui || !c.noOpen {
		t.Fatalf("gui launch config=%+v, %v", c, err)
	}
	if _, err := parseConfig([]string{"-dest", "out", "-no-open"}, io.Discard); err == nil {
		t.Fatal("no-open without GUI accepted")
	}
}

func TestTransferProgressCountsOnlyVerifiedFiles(t *testing.T) {
	c, dev := testConfig(t), testDevice()
	dev.files = map[string][]byte{"/sdcard/DCIM/a": []byte("first"), "/sdcard/DCIM/b": []byte("second")}
	dev.copyOverride = func(source, target string) error {
		if source == "/sdcard/DCIM/b" {
			return errors.New("disconnected")
		}
		return os.WriteFile(target, dev.files[source], 0600)
	}
	var last transferProgress
	c.onProgress = func(p transferProgress) { last = p }
	if err := runWithDevice(context.Background(), c, io.Discard, dev); err == nil {
		t.Fatal("expected failure")
	}
	if last.Total != 2 || last.Verified != 1 || last.Copied != 1 || last.Current != "/sdcard/DCIM/b" {
		t.Fatalf("progress misrepresents failed copy: %+v", last)
	}
}

func TestSourceFolderCanContainQuotesAndNewlines(t *testing.T) {
	source := "/sdcard/Pictures/family '\n雪"
	c, err := parseConfig([]string{"-dest", t.TempDir(), "-source", source}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	dev := testDevice()
	dev.files = map[string][]byte{source + "/photo.jpg": []byte("original")}
	mustRun(t, c, dev)
	assertContent(t, savedPath(c, source+"/photo.jpg"), []byte("original"))
}

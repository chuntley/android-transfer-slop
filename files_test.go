package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDestinationLockExcludesConcurrentTransfers(t *testing.T) {
	dir := t.TempDir()
	first, err := openDestination(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := openDestination(dir, true); err == nil {
		second.Close()
		t.Fatal("second writer obtained lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := openDestination(dir, true)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	third.Close()
}

func TestDestinationRefusesLockSymlink(t *testing.T) {
	dir, outside := t.TempDir(), filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".android-transfer.lock")); err != nil {
		t.Fatal(err)
	}
	if d, err := openDestination(dir, true); err == nil {
		d.Close()
		t.Fatal("followed lock symlink")
	}
	assertContent(t, outside, []byte("keep"))
}

func TestDestinationRejectsSymlinkParentAndFile(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "parent"}[parent], func(t *testing.T) {
			c, dev := testConfig(t), testDevice()
			outside := t.TempDir()
			precious := filepath.Join(outside, "precious")
			if err := os.WriteFile(precious, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if parent {
				dev.files = map[string][]byte{"/sdcard/DCIM/nested/photo.jpg": []byte("photo")}
				if err := os.Symlink(outside, filepath.Join(c.dest, "nested")); err != nil {
					t.Fatal(err)
				}
			} else {
				target := savedPath(c, "/sdcard/DCIM/photo.jpg")
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(precious, target); err != nil {
					t.Fatal(err)
				}
			}
			if err := runWithDevice(context.Background(), c, io.Discard, dev); err == nil {
				t.Fatal("accepted symlink destination")
			}
			assertContent(t, precious, []byte("keep"))
		})
	}
}

func TestDestinationRejectsFileAsParent(t *testing.T) {
	c, dev := testConfig(t), testDevice()
	dev.files = map[string][]byte{"/sdcard/DCIM/nested/photo.jpg": []byte("photo")}
	blocked := filepath.Join(c.dest, "nested")
	if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runWithDevice(context.Background(), c, io.Discard, dev); err == nil {
		t.Fatal("accepted nondirectory parent")
	}
	assertContent(t, blocked, []byte("keep"))
}

func TestDestinationRejectsDirectoryAsPhoto(t *testing.T) {
	c, dev := testConfig(t), testDevice()
	target := savedPath(c, "/sdcard/DCIM/photo.jpg")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(target, "keep")
	if err := os.WriteFile(precious, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runWithDevice(context.Background(), c, io.Discard, dev); err == nil {
		t.Fatal("replaced directory with photo")
	}
	assertContent(t, precious, []byte("keep"))
}

func TestDestinationCaseCollisionNeverAdoptsDifferentName(t *testing.T) {
	c, dev := testConfig(t), testDevice()
	target := savedPath(c, "/sdcard/DCIM/PHOTO.jpg")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	data := dev.files["/sdcard/DCIM/photo.jpg"]
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(savedPath(c, "/sdcard/DCIM/photo.jpg")); errors.Is(err, os.ErrNotExist) {
		t.Skip("filesystem is case-sensitive")
	}
	if err := runWithDevice(context.Background(), c, io.Discard, dev); err == nil {
		t.Fatal("case-colliding file accepted")
	}
	assertContent(t, target, data)
}

func TestLocalHashRefusesFIFOWithoutBlocking(t *testing.T) {
	d, err := openDestination(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := syscall.Mkfifo(filepath.Join(d.root.Name(), "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.hash(context.Background(), "pipe", false); err == nil {
		t.Fatal("FIFO treated as archive")
	}
}

func TestLocalHashHonorsCancellation(t *testing.T) {
	d, err := openDestination(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.root.WriteFile("file", []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.hash(ctx, "file", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestPublicationNeverReplacesExistingFile(t *testing.T) {
	d, err := openDestination(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.root.WriteFile("temp", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.root.WriteFile("target", []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.publish("temp", "target"); err == nil {
		t.Fatal("publication overwrote target")
	}
	assertContent(t, filepath.Join(d.root.Name(), "target"), []byte("old"))
}

func TestLocalPathRejectsTraversalAndMetadataCollisions(t *testing.T) {
	for _, value := range []string{"", "/", "relative", "/sdcard/../escape", "/sdcard//double", "/sdcard/nul\x00", "/sdcard", "/sdcard-other/file", "/sdcard/.android-transfer.lock", "/sdcard/.android-transfer-part-x/file"} {
		t.Run(value, func(t *testing.T) {
			if _, err := localPath(value, []string{"/sdcard"}); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
}

func TestCLIRejectsInvalidConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{}, {"-dest", "out", "extra"}, {"-dest", "out", "-batch-size", "0"},
		{"-dest", "out", "-batch-bytes", "-1"}, {"-dest", "out", "-max-batches", "-1"},
		{"-dest", "out", "-timeout", "0s"}, {"-dest", "out", "-source", "relative"},
		{"-dest", "out", "-source", "/"}, {"-dest", "out", "-source", "/sdcard\x00"},
		{"-dest", "out", "-refresh"}, {"-dest", "out", "-delete"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args, io.Discard); err == nil {
				t.Fatalf("accepted %v", args)
			}
		})
	}
}

func TestCLIParsesSourcesAndBatchControls(t *testing.T) {
	c, err := parseConfig([]string{"-dest", "out", "-source", "/sdcard/DCIM/", "-source", "/sdcard/Pictures", "-batch-size", "12", "-batch-bytes", "4096", "-max-batches", "3"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.dest != "out" || len(c.sources) != 2 || c.sources[0] != "/sdcard/DCIM" || c.batchSize != 12 || c.batchBytes != 4096 || c.maxBatches != 3 {
		t.Fatalf("incorrect user configuration: %+v", c)
	}
}

func TestCLIHelpDoesNotRequireDestination(t *testing.T) {
	if _, err := parseConfig([]string{"-help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help failed: %v", err)
	}
}

func TestVerificationDoesNotCreateMissingArchive(t *testing.T) {
	c := testConfig(t)
	c.dest = filepath.Join(c.dest, "missing")
	c.verify = true
	if err := runWithDevice(context.Background(), c, io.Discard, nil); err == nil {
		t.Fatal("accepted missing archive")
	}
	if _, err := os.Stat(c.dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verification created archive directory")
	}
}

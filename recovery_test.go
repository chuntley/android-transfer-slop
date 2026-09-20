package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResumeRepairsSameSizeSameTimeCorruption(t *testing.T) {
	c, d := testConfig(t), testDevice()
	mustRun(t, c, d)
	file := savedPath(c, "/sdcard/DCIM/photo.jpg")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte("irreplaceable phOto")
	if err := os.WriteFile(file, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c, d)
	assertContent(t, file, d.files["/sdcard/DCIM/photo.jpg"])
	if d.pulls != 2 {
		t.Fatal("same-metadata corruption was not repaired")
	}
}

func TestResumeDetectsSameSizeSourceEditWithoutTimestampChange(t *testing.T) {
	c, d := testConfig(t), testDevice()
	mustRun(t, c, d)
	d.files["/sdcard/DCIM/photo.jpg"] = []byte("irreplaceable phOto")
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("irreplaceable phOto"))
}

func TestDiskFullLeavesCompletedCopiesIntact(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{"/sdcard/DCIM/a": []byte("first"), "/sdcard/DCIM/b": []byte("second")}
	d.copyOverride = func(source, target string) error {
		if source == "/sdcard/DCIM/b" {
			if err := os.WriteFile(target, []byte("part"), 0600); err != nil {
				return err
			}
			return syscall.ENOSPC
		}
		return os.WriteFile(target, d.files[source], 0600)
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected disk full, got %v", err)
	}
	assertContent(t, savedPath(c, "/sdcard/DCIM/a"), []byte("first"))
	if _, err := os.Stat(savedPath(c, "/sdcard/DCIM/b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disk-full file published")
	}
	d.copyOverride = nil
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/b"), []byte("second"))
}

func TestVerificationDoesNotCreatePhotoDirectories(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{"/sdcard/DCIM/nested/photo.jpg": []byte("photo")}
	c.verify = true
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("missing destination accepted")
	}
	if _, err := os.Stat(filepath.Join(c.dest, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verify created photo directory")
	}
	if d.pulls != 0 {
		t.Fatal("verify copied files")
	}
}

func TestRestartIgnoresUnownedTemporaryFilesAndKeepsExtraLocalFiles(t *testing.T) {
	c, d := testConfig(t), testDevice()
	stale := filepath.Join(c.dest, ".android-transfer-part-old")
	extra := filepath.Join(c.dest, "unrelated.jpg")
	if err := os.WriteFile(stale, []byte("old partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("other archive"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c, d)
	assertContent(t, stale, []byte("old partial"))
	assertContent(t, extra, []byte("other archive"))
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), d.files["/sdcard/DCIM/photo.jpg"])
}

func TestMissingSourceNeverPublishesPartialCopy(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.copyOverride = func(source, target string) error {
		if err := os.WriteFile(target, d.files[source], 0600); err != nil {
			return err
		}
		delete(d.files, source)
		return nil
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected vanished source error, got %v", err)
	}
	assertNoPublished(t, c)
}

func TestVerifyChecksAllFilesRegardlessOfBatchLimit(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{"/sdcard/DCIM/a": []byte("a"), "/sdcard/DCIM/b": []byte("b")}
	mustRun(t, c, d)
	if err := os.WriteFile(savedPath(c, "/sdcard/DCIM/b"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	c.verify, c.batchSize, c.maxBatches = true, 1, 1
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("verification silently stopped at batch limit")
	}
}

func TestFailedReplacementKeepsExistingCopy(t *testing.T) {
	for _, failure := range []string{"disconnect", "disk full", "bad hash", "source changed", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			target := savedPath(c, "/sdcard/DCIM/photo.jpg")
			original := []byte("existing copy")
			if err := os.WriteFile(target, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.copyOverride = func(source, temp string) error {
				assertContent(t, target, original)
				if err := os.WriteFile(temp, d.files[source], 0600); err != nil {
					return err
				}
				switch failure {
				case "disconnect":
					return errors.New("USB disconnected")
				case "disk full":
					return syscall.ENOSPC
				case "bad hash":
					return os.WriteFile(temp, []byte("corrupted"), 0600)
				case "source changed":
					d.mtime++
				case "cancel":
					cancel()
				}
				return nil
			}
			if err := runWithDevice(ctx, c, io.Discard, d); err == nil {
				t.Fatal("accepted failed replacement")
			}
			assertContent(t, target, original)
			matches, err := filepath.Glob(filepath.Join(c.dest, ".android-transfer-part-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("replacement temporary file not cleaned: %v, %v", matches, err)
			}
			d.copyOverride = nil
			mustRun(t, c, d)
			assertContent(t, target, d.files["/sdcard/DCIM/photo.jpg"])
		})
	}
}

func TestReplacementRefusesChangedDestination(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			target := savedPath(c, "/sdcard/DCIM/photo.jpg")
			if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "other")
			if err := os.WriteFile(other, []byte("other writer"), 0600); err != nil {
				t.Fatal(err)
			}
			d.copyOverride = func(source, temp string) error {
				if err := os.Remove(target); err != nil {
					return err
				}
				if kind == "symlink" {
					if err := os.Symlink(other, target); err != nil {
						return err
					}
				} else if err := os.Link(other, target); err != nil {
					return err
				}
				return os.WriteFile(temp, d.files[source], 0600)
			}
			if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
				t.Fatal("replaced a concurrently changed destination")
			}
			assertContent(t, target, []byte("other writer"))
			assertContent(t, other, []byte("other writer"))
		})
	}
}

func TestReplacementCountsTowardBatchLimits(t *testing.T) {
	for _, limits := range []struct {
		name  string
		files int
		bytes int64
	}{{"file limit", 1, 100}, {"byte limit", 10, 4}} {
		t.Run(limits.name, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			d.files = map[string][]byte{
				"/sdcard/DCIM/a-match":   []byte("1234"),
				"/sdcard/DCIM/b-repair":  []byte("5678"),
				"/sdcard/DCIM/c-repair":  []byte("9012"),
				"/sdcard/DCIM/d-missing": []byte("3456"),
			}
			for _, name := range []string{"a-match", "b-repair", "c-repair"} {
				content := []byte("wrong")
				if name == "a-match" {
					content = d.files["/sdcard/DCIM/"+name]
				}
				if err := os.WriteFile(filepath.Join(c.dest, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			c.batchSize, c.batchBytes, c.maxBatches = limits.files, limits.bytes, 1
			mustRun(t, c, d)
			assertContent(t, filepath.Join(c.dest, "b-repair"), []byte("5678"))
			assertContent(t, filepath.Join(c.dest, "c-repair"), []byte("wrong"))
			if _, err := os.Stat(filepath.Join(c.dest, "d-missing")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("batch limit copied missing file: %v", err)
			}
			mustRun(t, c, d)
			assertContent(t, filepath.Join(c.dest, "c-repair"), []byte("9012"))
			mustRun(t, c, d)
			assertContent(t, filepath.Join(c.dest, "d-missing"), []byte("3456"))
			if d.pulls != 3 {
				t.Fatalf("matching copies consumed transfer batches: %d pulls", d.pulls)
			}
		})
	}
}

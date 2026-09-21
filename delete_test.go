package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type deletingMemoryDevice struct {
	*memoryDevice
	attempts      []string
	beforeRemove  func(string)
	removeErr     error
	removeOnError bool
}

func (d *deletingMemoryDevice) RemoveVerified(ctx context.Context, source string, expected fingerprint) error {
	d.attempts = append(d.attempts, source)
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.beforeRemove != nil {
		d.beforeRemove(source)
	}
	actual, err := d.Inspect(ctx, source)
	if err != nil {
		return err
	}
	if actual != expected {
		return errSourceChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.removeErr != nil {
		if d.removeOnError {
			delete(d.files, source)
		}
		return d.removeErr
	}
	delete(d.files, source)
	return nil
}

func seedDeleteCopies(t *testing.T, c config, d *memoryDevice) {
	t.Helper()
	for source, data := range d.files {
		target := savedPath(c, source)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func runDeleteTest(t *testing.T, ctx context.Context, c config, d device) (transferProgress, string, error) {
	t.Helper()
	var latest transferProgress
	previous := c.onProgress
	c.safeDelete = true
	c.onProgress = func(p transferProgress) {
		latest = p
		if previous != nil {
			previous(p)
		}
	}
	var out bytes.Buffer
	err := runWithDevice(ctx, c, &out, d)
	return latest, out.String(), err
}

func TestSafeDeleteRemovesOnlyMatchingFilesWithoutChangingDestination(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	d.files["/sdcard/DCIM/album/empty"] = nil
	seedDeleteCopies(t, c, d.memoryDevice)
	before, err := os.Stat(savedPath(c, "/sdcard/DCIM/photo.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if err != nil {
		t.Fatalf("delete: %v\n%s", err, report)
	}
	if len(d.files) != 0 || d.pulls != 0 || progress.Checked != 2 || progress.Verified != 2 || progress.Deleted != 2 || progress.Errors != 0 {
		t.Fatalf("matching deletion failed: files=%v; pulls=%d; progress=%#v", d.files, d.pulls, progress)
	}
	if d.inspections != 2 {
		t.Fatalf("safe delete performed redundant source inspections: %d, want 2", d.inspections)
	}
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("irreplaceable photo"))
	assertContent(t, savedPath(c, "/sdcard/DCIM/album/empty"), nil)
	after, err := os.Stat(savedPath(c, "/sdcard/DCIM/photo.jpg"))
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("destination was replaced or rewritten: %v", err)
	}
}

func TestSafeDeleteRetainsMissingAndMismatchedCopiesAndContinues(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	d.files = map[string][]byte{
		"/sdcard/DCIM/a-missing":              []byte("missing"),
		"/sdcard/DCIM/b-missing-parent/photo": []byte("missing parent"),
		"/sdcard/DCIM/c-mismatch":             []byte("original"),
		"/sdcard/DCIM/z-match":                []byte("matching"),
	}
	seedDeleteCopies(t, c, d.memoryDevice)
	if err := os.Remove(savedPath(c, "/sdcard/DCIM/a-missing")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(savedPath(c, "/sdcard/DCIM/b-missing-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savedPath(c, "/sdcard/DCIM/c-mismatch"), []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if err == nil || progress.Checked != 4 || progress.Deleted != 1 || progress.Verified != 1 || progress.Missing != 2 || progress.Changed != 1 || progress.Errors != 0 {
		t.Fatalf("unsafe retention accounting: error=%v; progress=%#v\n%s", err, progress, report)
	}
	if len(d.files) != 3 || len(d.attempts) != 2 || d.attempts[0] != "/sdcard/DCIM/c-mismatch" || d.attempts[1] != "/sdcard/DCIM/z-match" || d.pulls != 0 {
		t.Fatalf("missing/mismatched files were removed or copied: files=%v; attempts=%v; pulls=%d", d.files, d.attempts, d.pulls)
	}
	assertContent(t, savedPath(c, "/sdcard/DCIM/c-mismatch"), []byte("modified"))
	if _, err := os.Stat(savedPath(c, "/sdcard/DCIM/b-missing-parent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deletion created missing destination directory: %v", err)
	}
	if !strings.Contains(report, "retained: 3;") || !strings.Contains(report, "not processed: 0") {
		t.Fatalf("retained files not accounted for: %s", report)
	}
}

func TestSafeDeleteRejectsChangedSourceAtBothChecks(t *testing.T) {
	for _, finalCheck := range []bool{false, true} {
		name := "before-final-removal"
		if !finalCheck {
			name = "before-local-hash"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			d := &deletingMemoryDevice{memoryDevice: testDevice()}
			d.files["/sdcard/DCIM/z-match"] = []byte("safe")
			seedDeleteCopies(t, c, d.memoryDevice)
			changed := bytes.Repeat([]byte("x"), len(d.files["/sdcard/DCIM/photo.jpg"]))
			mutate := func(source string) {
				if source == "/sdcard/DCIM/photo.jpg" {
					d.files[source] = changed
					if !finalCheck {
						d.mtime++
					}
				}
			}
			if finalCheck {
				d.beforeRemove = mutate
			} else {
				mutated := false
				c.onProgress = func(p transferProgress) {
					if p.Phase != "deleting" {
						return
					}
					if p.Current == "/sdcard/DCIM/photo.jpg" && !mutated {
						mutate(p.Current)
						mutated = true
					}
					if p.Current == "/sdcard/DCIM/z-match" {
						d.mtime = 1234
					}
				}
			}
			progress, report, err := runDeleteTest(t, context.Background(), c, d)
			if err == nil || progress.Changed != 1 || progress.Deleted != 1 || progress.Checked != 2 || progress.Errors != 0 {
				t.Fatalf("changed source was not retained: error=%v; progress=%#v\n%s", err, progress, report)
			}
			if !bytes.Equal(d.files["/sdcard/DCIM/photo.jpg"], changed) || len(d.files) != 1 || d.pulls != 0 {
				t.Fatalf("source mutation was deleted: %v", d.files)
			}
			assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("irreplaceable photo"))
		})
	}
}

func TestSafeDeleteRehashesLocalBytesBeforeRemoval(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	target := savedPath(c, "/sdcard/DCIM/photo.jpg")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("x"), int(before.Size()))
	if err := os.WriteFile(target, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if err == nil || progress.Deleted != 0 || progress.Changed != 1 || len(d.attempts) != 1 || len(d.files) != 1 {
		t.Fatalf("stale local content authorized deletion: error=%v; progress=%#v; attempts=%v\n%s", err, progress, d.attempts, report)
	}
	assertContent(t, target, corrupt)
}

func TestSafeDeleteRejectsDestinationSymlinks(t *testing.T) {
	for _, symlinkParent := range []bool{false, true} {
		name := "file"
		if symlinkParent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			d := &deletingMemoryDevice{memoryDevice: testDevice()}
			d.files = map[string][]byte{"/sdcard/DCIM/album/photo": []byte("matching")}
			seedDeleteCopies(t, c, d.memoryDevice)
			target := savedPath(c, "/sdcard/DCIM/album/photo")
			link := target
			if symlinkParent {
				link = filepath.Dir(target)
			}
			if err := os.Rename(link, link+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(link)+"-real", link); err != nil {
				t.Fatal(err)
			}
			progress, report, err := runDeleteTest(t, context.Background(), c, d)
			if err == nil || progress.Errors != 1 || progress.Deleted != 0 || len(d.attempts) != 0 || len(d.files) != 1 || d.pulls != 0 {
				t.Fatalf("symlink authorized deletion: error=%v; progress=%#v\n%s", err, progress, report)
			}
			assertContent(t, target, []byte("matching"))
		})
	}
}

func TestSafeDeleteCancellationStopsBeforeRemoval(t *testing.T) {
	for _, duringRemoval := range []bool{false, true} {
		name := "between-files"
		if duringRemoval {
			name = "source-removal"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			d := &deletingMemoryDevice{memoryDevice: testDevice()}
			d.files = map[string][]byte{"/sdcard/DCIM/a": []byte("first"), "/sdcard/DCIM/b": []byte("second"), "/sdcard/DCIM/c": []byte("third")}
			seedDeleteCopies(t, c, d.memoryDevice)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if duringRemoval {
				d.beforeRemove = func(source string) {
					if source == "/sdcard/DCIM/b" {
						cancel()
					}
				}
			} else {
				c.onProgress = func(p transferProgress) {
					if p.Deleted == 1 {
						cancel()
					}
				}
			}
			progress, report, err := runDeleteTest(t, ctx, c, d)
			checked, failures, attempts := 1, 0, 1
			if duringRemoval {
				checked, failures, attempts = 2, 1, 2
			}
			if !errors.Is(err, context.Canceled) || progress.Checked != checked || progress.Deleted != 1 || progress.Errors != failures || len(d.files) != 2 || len(d.attempts) != attempts || d.pulls != 0 {
				t.Fatalf("cancellation lost files or counts: error=%v; progress=%#v; files=%v\n%s", err, progress, d.files, report)
			}
			assertContent(t, savedPath(c, "/sdcard/DCIM/b"), []byte("second"))
		})
	}
}

func TestSafeDeleteCommandFailureStopsAndReportsUncertainty(t *testing.T) {
	for _, removed := range []bool{false, true} {
		name := "before-removal"
		if removed {
			name = "lost-acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			d := &deletingMemoryDevice{memoryDevice: testDevice(), removeErr: errors.New("device disconnected"), removeOnError: removed}
			d.files = map[string][]byte{"/sdcard/DCIM/a": []byte("first"), "/sdcard/DCIM/b": []byte("second")}
			seedDeleteCopies(t, c, d.memoryDevice)
			progress, report, err := runDeleteTest(t, context.Background(), c, d)
			if !errors.Is(err, d.removeErr) || progress.Checked != 1 || progress.Verified != 0 || progress.Deleted != 0 || progress.Errors != 1 || len(d.attempts) != 1 || d.pulls != 0 {
				t.Fatalf("command failure lost uncertainty: error=%v; progress=%#v\n%s", err, progress, report)
			}
			if _, exists := d.files["/sdcard/DCIM/b"]; !exists {
				t.Fatal("deletion continued after device failure")
			}
			if !strings.Contains(report, "deletion unconfirmed: 1;") || !strings.Contains(report, "retained: 0;") || !strings.Contains(report, "not processed: 1") {
				t.Fatalf("report claims an unconfirmed deletion was retained or completed: %s", report)
			}
			assertContent(t, savedPath(c, "/sdcard/DCIM/a"), []byte("first"))
		})
	}
}

func TestSafeDeleteRequiresConditionalDeletionCapability(t *testing.T) {
	c, d := testConfig(t), testDevice()
	seedDeleteCopies(t, c, d)
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if err == nil || progress.Checked != 0 || progress.Deleted != 0 || len(d.files) != 1 || d.inspections != 0 || d.pulls != 0 {
		t.Fatalf("read-only device accepted for deletion: error=%v; progress=%#v\n%s", err, progress, report)
	}
}

func TestSafeDeleteIncompleteInventoryNeverAuthorizesDeletion(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	d.scanErr = io.ErrUnexpectedEOF
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if !errors.Is(err, d.scanErr) || progress.Checked != 0 || progress.Deleted != 0 || len(d.attempts) != 0 || d.inspections != 0 || d.pulls != 0 || len(d.files) != 1 {
		t.Fatalf("partial inventory authorized deletion: error=%v; progress=%#v\n%s", err, progress, report)
	}
	if !strings.Contains(report, "not processed: unknown") {
		t.Fatalf("partial inventory claimed complete counts: %s", report)
	}
}

func TestSafeDeleteSourceInspectionFailureIsNotAMissingCopy(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	d.files["/sdcard/DCIM/z-unprocessed"] = []byte("second")
	seedDeleteCopies(t, c, d.memoryDevice)
	d.inspectErr = os.ErrNotExist
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if !errors.Is(err, os.ErrNotExist) || progress.Checked != 1 || progress.Missing != 0 || progress.Errors != 1 || progress.Deleted != 0 || len(d.attempts) != 1 || len(d.files) != 2 {
		t.Fatalf("source error was treated as a missing local copy or continued: error=%v; progress=%#v\n%s", err, progress, report)
	}
}

func TestSafeDeleteDoesNotCreateMissingDestination(t *testing.T) {
	c := testConfig(t)
	c.dest = filepath.Join(c.dest, "not-created")
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if !errors.Is(err, os.ErrNotExist) || progress.Deleted != 0 || d.inspections != 0 || d.pulls != 0 || len(d.attempts) != 0 || len(d.files) != 1 {
		t.Fatalf("missing destination authorized deletion or copying: error=%v; progress=%#v\n%s", err, progress, report)
	}
	if _, err := os.Stat(c.dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("safe deletion created destination: %v", err)
	}
}

func TestSafeDeleteRejectsDisplacedDestinationRoot(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	displaced := c.dest + "-displaced"
	t.Cleanup(func() { os.RemoveAll(displaced) })
	moved := false
	c.onProgress = func(p transferProgress) {
		if moved || p.Phase != "deleting" || p.Current != "/sdcard/DCIM/photo.jpg" {
			return
		}
		moved = true
		if err := os.Rename(c.dest, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(c.dest, 0700); err != nil {
			t.Fatal(err)
		}
	}
	progress, report, err := runDeleteTest(t, context.Background(), c, d)
	if err == nil || progress.Deleted != 0 || len(d.attempts) != 0 || len(d.files) != 1 {
		t.Fatalf("displaced destination authorized deletion: error=%v; progress=%#v\n%s", err, progress, report)
	}
	assertContent(t, filepath.Join(displaced, "photo.jpg"), []byte("irreplaceable photo"))
}

func TestSafeDeleteConflictingModeNeverTouchesSource(t *testing.T) {
	c := testConfig(t)
	c.verify = true
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	_, _, err := runDeleteTest(t, context.Background(), c, d)
	if err == nil || len(d.attempts) != 0 || len(d.files) != 1 || d.scans != 0 {
		t.Fatal("conflicting verify/delete mode reached device operations")
	}
}

func TestSafeDeleteDestinationLockPreventsConcurrentRemoval(t *testing.T) {
	c := testConfig(t)
	d := &deletingMemoryDevice{memoryDevice: testDevice()}
	seedDeleteCopies(t, c, d.memoryDevice)
	locked, err := openDestination(c.dest, false)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	_, _, err = runDeleteTest(t, context.Background(), c, d)
	if err == nil || len(d.attempts) != 0 || len(d.files) != 1 || d.scans != 0 {
		t.Fatal("deletion proceeded while another run held the destination lock")
	}
}

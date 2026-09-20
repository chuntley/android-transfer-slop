package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type memoryDevice struct {
	files                                   map[string][]byte
	mtime                                   int64
	scans, pulls, inspections               int
	serialErr, scanErr, inspectErr, pullErr error
	afterInventory                          func()
	beforeInspect                           func(int, string)
	copyOverride                            func(string, string) error
}

func testDevice() *memoryDevice {
	return &memoryDevice{files: map[string][]byte{"/sdcard/DCIM/photo.jpg": []byte("irreplaceable photo")}, mtime: 1234}
}
func (d *memoryDevice) Serial(context.Context) (string, error) { return "samsung-test", d.serialErr }
func (d *memoryDevice) Inventory(_ context.Context, _ []string, report func(int, string)) ([]entry, error) {
	d.scans++
	var result []entry
	for path, data := range d.files {
		result = append(result, entry{Path: path, Size: int64(len(data)), ModTime: d.mtime})
		if report != nil {
			report(len(result), path)
		}
	}
	if d.afterInventory != nil {
		d.afterInventory()
	}
	return result, d.scanErr
}
func (d *memoryDevice) Inspect(_ context.Context, path string) (fingerprint, error) {
	d.inspections++
	if d.beforeInspect != nil {
		d.beforeInspect(d.inspections, path)
	}
	if d.inspectErr != nil {
		return fingerprint{}, d.inspectErr
	}
	data, exists := d.files[path]
	if !exists {
		return fingerprint{}, os.ErrNotExist
	}
	h := sha256.Sum256(data)
	return fingerprint{Size: int64(len(data)), ModTime: d.mtime, SHA256: hex.EncodeToString(h[:])}, nil
}
func (d *memoryDevice) Pull(_ context.Context, source, target string) error {
	d.pulls++
	if d.copyOverride != nil {
		return d.copyOverride(source, target)
	}
	if d.pullErr != nil {
		return d.pullErr
	}
	return os.WriteFile(target, d.files[source], 0600)
}
func testConfig(t *testing.T) config {
	t.Helper()
	return config{dest: t.TempDir(), sources: sources{"/sdcard/DCIM"}, batchSize: 250, batchBytes: 2 << 30, timeout: time.Second}
}
func savedPath(c config, path string) string {
	return filepath.Join(c.dest, filepath.FromSlash(strings.TrimPrefix(path, c.sources[0]+"/")))
}
func mustRun(t *testing.T, c config, d device) string {
	t.Helper()
	var out bytes.Buffer
	if err := runWithDevice(context.Background(), c, &out, d); err != nil {
		t.Fatalf("run: %v\n%s", err, &out)
	}
	return out.String()
}

func TestTransferScanProgressStaysCurrentBeforeInventoryReturns(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = make(map[string][]byte)
	for i := range 1000 {
		d.files[fmt.Sprintf("/sdcard/DCIM/%04d.jpg", i)] = nil
	}
	d.scanErr = errors.New("incomplete inventory")
	var latest transferProgress
	c.onProgress = func(p transferProgress) { latest = p }
	d.afterInventory = func() {
		// The producer may pause indefinitely after a burst. Status must already
		// reflect every discovered file, without waiting for another record or EOF.
		if latest.Scanned != len(d.files) {
			t.Errorf("scan status is stale before inventory returns: got %d, want %d", latest.Scanned, len(d.files))
		}
		if _, ok := d.files[latest.Current]; !ok {
			t.Errorf("latest discovered path missing: %q", latest.Current)
		}
		if latest.Phase != "scanning" || latest.Total != 0 || latest.Verified != 0 || latest.Copied != 0 {
			t.Errorf("incomplete scan advertised transfer progress: %#v", latest)
		}
	}
	err := runWithDevice(context.Background(), c, io.Discard, d)
	if !errors.Is(err, d.scanErr) || d.pulls != 0 || d.inspections != 0 {
		t.Fatalf("partial inventory used: error=%v, pulls=%d, inspections=%d", err, d.pulls, d.inspections)
	}
	if latest.Scanned != len(d.files) || latest.Phase != "scanning" {
		t.Fatalf("failed scan lost discovery progress: %#v", latest)
	}
}

func TestTransferScanTimeoutExplainsRecovery(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.scanErr = context.DeadlineExceeded
	err := runWithDevice(context.Background(), c, io.Discard, d)
	if !errors.Is(err, d.scanErr) || !strings.Contains(err.Error(), "inventory scan stopped after 1s without device activity") {
		t.Fatalf("scan timeout guidance missing: %v", err)
	}
}

func TestTransferClearsScanPathBeforeCopying(t *testing.T) {
	c, d := testConfig(t), testDevice()
	var transition *transferProgress
	c.onProgress = func(p transferProgress) {
		if p.Phase == "transferring" && transition == nil {
			transition = &p
			if d.pulls != 0 {
				t.Error("transfer started before scan transition")
			}
		}
	}
	mustRun(t, c, d)
	if transition == nil || transition.Current != "" || transition.Scanned != 1 || transition.Total != 1 {
		t.Fatalf("invalid scan-to-transfer transition: %#v", transition)
	}
}

func assertContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("content %q: got %q, error %v; want %q", path, actual, err, expected)
	}
}
func assertNoPublished(t *testing.T, c config) {
	t.Helper()
	if _, err := os.Lstat(savedPath(c, "/sdcard/DCIM/photo.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected publication: %v", err)
	}
}

func TestTransferCopiesExactBytesAndReverifiesOnResume(t *testing.T) {
	c, d := testConfig(t), testDevice()
	original := bytes.Clone(d.files["/sdcard/DCIM/photo.jpg"])
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), original)
	mustRun(t, c, d)
	if d.scans != 2 || d.pulls != 1 {
		t.Fatalf("unsafe resume: scans=%d pulls=%d", d.scans, d.pulls)
	}
	if !bytes.Equal(original, d.files["/sdcard/DCIM/photo.jpg"]) {
		t.Fatal("source changed")
	}
}

func TestTransferPreservesOnlySelectedFolderContents(t *testing.T) {
	c, d := testConfig(t), testDevice()
	c.sources = sources{"/sdcard/DCIM/Camera"}
	d.files = map[string][]byte{
		"/sdcard/DCIM/Camera/photo.jpg":          []byte("photo"),
		"/sdcard/DCIM/Camera/trip/day1/clip.mp4": []byte("video"),
	}
	mustRun(t, c, d)
	assertContent(t, filepath.Join(c.dest, "photo.jpg"), []byte("photo"))
	assertContent(t, filepath.Join(c.dest, "trip", "day1", "clip.mp4"), []byte("video"))
	for _, ancestor := range []string{"sdcard", "DCIM", "Camera"} {
		if _, err := os.Lstat(filepath.Join(c.dest, ancestor)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source ancestor %q preserved: %v", ancestor, err)
		}
	}
	mustRun(t, c, d)
	c.verify = true
	mustRun(t, c, d)
	if d.pulls != 2 {
		t.Fatalf("resume or verification recopied files: %d pulls", d.pulls)
	}
}

func TestTransferRejectsCollidingSourceContentsBeforeCopying(t *testing.T) {
	c, d := testConfig(t), testDevice()
	c.sources = sources{"/sdcard/DCIM", "/sdcard/Pictures"}
	d.files["/sdcard/Pictures/photo.jpg"] = []byte("irreplaceable photo")
	err := runWithDevice(context.Background(), c, io.Discard, d)
	if err == nil || !strings.Contains(err.Error(), "same destination") {
		t.Fatalf("expected source collision, got %v", err)
	}
	if d.pulls != 0 {
		t.Fatal("copied files before rejecting ambiguous destination")
	}
	assertNoPublished(t, c)
}

func TestTransferOverlappingRootsUseMostSpecificSelection(t *testing.T) {
	for _, roots := range []sources{
		{"/sdcard/DCIM", "/sdcard/DCIM/Camera"},
		{"/sdcard/DCIM/Camera", "/sdcard/DCIM"},
	} {
		c, d := testConfig(t), testDevice()
		c.sources = roots
		d.files = map[string][]byte{
			"/sdcard/DCIM/Camera/photo.jpg": []byte("photo"),
			"/sdcard/DCIM/other/clip.mp4":   []byte("video"),
		}
		mustRun(t, c, d)
		assertContent(t, filepath.Join(c.dest, "photo.jpg"), []byte("photo"))
		assertContent(t, filepath.Join(c.dest, "other", "clip.mp4"), []byte("video"))
		c.verify = true
		mustRun(t, c, d)
	}
}

func TestTransferInterruptedCopyCanResume(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.copyOverride = func(_, target string) error {
		if err := os.WriteFile(target, []byte("partial"), 0600); err != nil {
			return err
		}
		return errors.New("USB disconnected")
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("expected interrupted copy error")
	}
	assertNoPublished(t, c)
	matches, err := filepath.Glob(filepath.Join(c.dest, ".android-transfer-part-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files not cleaned: %v %v", matches, err)
	}
	d.copyOverride = nil
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), d.files["/sdcard/DCIM/photo.jpg"])
}

func TestTransferRejectsCorruptOrTruncatedCopy(t *testing.T) {
	for _, payload := range []string{"irreplaceable phOto", "short", ""} {
		t.Run(payload, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			d.copyOverride = func(_, target string) error { return os.WriteFile(target, []byte(payload), 0600) }
			if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
				t.Fatal("expected verification failure")
			}
			assertNoPublished(t, c)
		})
	}
}

func TestTransferReplacesMismatchedCopyWithSource(t *testing.T) {
	c, d := testConfig(t), testDevice()
	file := savedPath(c, "/sdcard/DCIM/photo.jpg")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("different photo")
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c, d)
	assertContent(t, file, d.files["/sdcard/DCIM/photo.jpg"])
	mustRun(t, c, d)
	if d.pulls != 1 {
		t.Fatal("replacement was not copied exactly once")
	}
}

func TestTransferAdoptsMatchingExistingCopy(t *testing.T) {
	c, d := testConfig(t), testDevice()
	file := savedPath(c, "/sdcard/DCIM/photo.jpg")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, d.files["/sdcard/DCIM/photo.jpg"], 0600); err != nil {
		t.Fatal(err)
	}
	// Matching content must be skipped even when local timestamps differ.
	stamp := time.Unix(4567, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, c, d)
	if d.pulls != 0 {
		t.Fatal("matching saved copy recopied")
	}
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("matching saved copy replaced or modified")
	}
	assertContent(t, file, d.files["/sdcard/DCIM/photo.jpg"])
}

func TestTransferRejectsSourceChangedDuringCopy(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.copyOverride = func(p, target string) error {
		if err := os.WriteFile(target, d.files[p], 0600); err != nil {
			return err
		}
		d.files[p] = []byte("irreplaceable phOto")
		return nil
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("expected changing source error")
	}
	assertNoPublished(t, c)
}

func TestTransferRejectsStaleInventory(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.beforeInspect = func(n int, _ string) {
		if n == 1 {
			d.mtime++
		}
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("expected stale inventory error")
	}
	assertNoPublished(t, c)
}

func TestTransferPublicationRaceDoesNotOverwrite(t *testing.T) {
	c, d := testConfig(t), testDevice()
	target := savedPath(c, "/sdcard/DCIM/photo.jpg")
	d.beforeInspect = func(n int, _ string) {
		if n == 1 {
			if err := os.WriteFile(target, []byte("other writer"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
		t.Fatal("expected no-clobber failure")
	}
	assertContent(t, target, []byte("other writer"))
}

func TestTransferCancellationNeverPublishes(t *testing.T) {
	c, d := testConfig(t), testDevice()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.copyOverride = func(source, target string) error {
		err := os.WriteFile(target, d.files[source], 0600)
		cancel()
		return err
	}
	if err := runWithDevice(ctx, c, io.Discard, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	assertNoPublished(t, c)
}

func TestTransferBatchesResumeAndAdvance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		bytes int64
	}{
		{"file limit", 1, 100}, {"byte limit", 20, 4}, {"oversized file", 20, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			d.files = map[string][]byte{"/sdcard/DCIM/a": []byte("1234"), "/sdcard/DCIM/b": []byte("5678"), "/sdcard/DCIM/c": []byte("9012")}
			c.batchSize, c.batchBytes, c.maxBatches = tc.size, tc.bytes, 1
			for i := 1; i <= 3; i++ {
				mustRun(t, c, d)
				if d.pulls != i {
					t.Fatalf("run %d: got %d copies", i, d.pulls)
				}
				for _, name := range []string{"a", "b", "c"}[:i] {
					p := "/sdcard/DCIM/" + name
					assertContent(t, savedPath(c, p), d.files[p])
				}
			}
		})
	}
}

func TestTransferResumeDiscoversNewFiles(t *testing.T) {
	c, d := testConfig(t), testDevice()
	mustRun(t, c, d)
	d.files["/sdcard/DCIM/new.jpg"] = []byte("new photo")
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/new.jpg"), []byte("new photo"))
	if d.pulls != 2 {
		t.Fatal("resume recopied unchanged files")
	}
}

func TestTransferUpdatesCopyWhenSourceChangedBetweenRuns(t *testing.T) {
	c, d := testConfig(t), testDevice()
	mustRun(t, c, d)
	d.files["/sdcard/DCIM/photo.jpg"] = []byte("edited image")
	d.mtime++
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/photo.jpg"), []byte("edited image"))
}

func TestTransferVerificationDetectsMissingAndCorruptCopies(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt", true: "missing"}[missing], func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			mustRun(t, c, d)
			c.verify = true
			mustRun(t, c, d)
			file := savedPath(c, "/sdcard/DCIM/photo.jpg")
			var err error
			if missing {
				err = os.Remove(file)
			} else {
				err = os.WriteFile(file, []byte("corrupted"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
				t.Fatal("expected verification failure")
			}
			if d.pulls != 1 {
				t.Fatal("verification copied a file")
			}
			if missing {
				assertNoPublished(t, c)
			} else {
				assertContent(t, file, []byte("corrupted"))
			}
		})
	}
}

func TestVerifyContinuesThroughCategorizedFailuresWithoutWriting(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{
		"/sdcard/DCIM/a-missing":              []byte("missing"),
		"/sdcard/DCIM/b-missing-parent/photo": []byte("missing parent"),
		"/sdcard/DCIM/c-mismatch":             []byte("original"),
		"/sdcard/DCIM/d-changed":              []byte("before"),
		"/sdcard/DCIM/e-device-error":         []byte("device error"),
		"/sdcard/DCIM/f-not-regular":          []byte("not regular"),
		"/sdcard/DCIM/g-device-changed":       []byte("changed while hashing"),
		"/sdcard/DCIM/z-match":                []byte("final match"),
	}
	mustRun(t, c, d)
	for _, source := range []string{"/sdcard/DCIM/a-missing", "/sdcard/DCIM/b-missing-parent/photo", "/sdcard/DCIM/f-not-regular"} {
		if err := os.Remove(savedPath(c, source)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(savedPath(c, "/sdcard/DCIM/b-missing-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(savedPath(c, "/sdcard/DCIM/f-not-regular"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savedPath(c, "/sdcard/DCIM/c-mismatch"), []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		t.Helper()
		files := make(map[string]string)
		err := filepath.Walk(c.dest, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			var data []byte
			if info.Mode().IsRegular() {
				data, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
			files[path] = fmt.Sprintf("%v|%s|%q", info.Mode(), info.ModTime(), data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshot()
	d.pulls, d.inspections = 0, 0
	d.beforeInspect = func(_ int, path string) {
		d.inspectErr = nil
		switch path {
		case "/sdcard/DCIM/d-changed":
			d.files[path] = []byte("source changed after inventory")
		case "/sdcard/DCIM/e-device-error":
			// A missing device file is not a missing local copy.
			d.inspectErr = os.ErrNotExist
		case "/sdcard/DCIM/g-device-changed":
			d.inspectErr = fmt.Errorf("device inspection: %w", errSourceChanged)
		}
	}
	var latest transferProgress
	c.verify, c.batchSize, c.maxBatches = true, 1, 1
	c.onProgress = func(p transferProgress) {
		latest = p
		if p.Checked != p.Verified+p.Missing+p.Mismatched+p.Changed+p.Errors {
			t.Errorf("inconsistent verification accounting: %#v", p)
		}
	}
	var out bytes.Buffer
	if err := runWithDevice(context.Background(), c, &out, d); err == nil {
		t.Fatal("categorized failures returned success")
	}
	if latest.Total != 8 || latest.Checked != 8 || latest.Verified != 1 || latest.Missing != 2 ||
		latest.Mismatched != 1 || latest.Changed != 2 || latest.Errors != 2 || latest.Current != "/sdcard/DCIM/z-match" {
		t.Fatalf("verification did not account for every file: %#v\n%s", latest, &out)
	}
	for source := range d.files {
		if strings.Count(out.String(), fmt.Sprintf("%q", source)) != 1 {
			t.Errorf("report must contain one result for %q:\n%s", source, &out)
		}
	}
	if !strings.Contains(out.String(), "not processed: 0") {
		t.Fatalf("report lacks complete scan accounting:\n%s", &out)
	}
	if d.pulls != 0 || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("verification modified local files or pulled device files")
	}
}

func TestVerifyCancellationReportsPartialCounts(t *testing.T) {
	for _, cancelDuringFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("during-file-%t", cancelDuringFile), func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			d.files = map[string][]byte{
				"/sdcard/DCIM/a": []byte("first"),
				"/sdcard/DCIM/b": []byte("second"),
				"/sdcard/DCIM/c": []byte("unprocessed"),
			}
			mustRun(t, c, d)
			d.pulls, d.inspections = 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var latest transferProgress
			c.verify = true
			c.onProgress = func(p transferProgress) {
				latest = p
				if !cancelDuringFile && p.Checked == 1 {
					cancel()
				}
			}
			if cancelDuringFile {
				d.beforeInspect = func(_ int, path string) {
					if path == "/sdcard/DCIM/b" {
						cancel()
					}
				}
			}
			var out bytes.Buffer
			err := runWithDevice(ctx, c, &out, d)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			checked, failures := 1, 0
			if cancelDuringFile {
				checked, failures = 2, 1
			}
			if latest.Checked != checked || latest.Verified != 1 || latest.Errors != failures ||
				latest.Missing != 0 || latest.Mismatched != 0 || latest.Changed != 0 || d.inspections != checked || d.pulls != 0 {
				t.Fatalf("verification continued after cancellation or lost counts: %#v; inspections=%d pulls=%d\n%s",
					latest, d.inspections, d.pulls, &out)
			}
			if !strings.Contains(out.String(), "Verification incomplete:") ||
				!strings.Contains(out.String(), fmt.Sprintf("not processed: %d", 3-checked)) {
				t.Fatalf("partial report does not identify unprocessed files:\n%s", &out)
			}
		})
	}
}

func TestVerifyIncompleteInventoryDoesNotClaimCompleteCounts(t *testing.T) {
	c, d := testConfig(t), testDevice()
	c.verify = true
	d.scanErr = errors.New("inventory interrupted")
	var latest transferProgress
	c.onProgress = func(p transferProgress) { latest = p }
	var out bytes.Buffer
	err := runWithDevice(context.Background(), c, &out, d)
	if !errors.Is(err, d.scanErr) || latest.Checked != 0 || d.pulls != 0 || d.inspections != 0 {
		t.Fatalf("incomplete inventory was used: progress=%#v; error=%v", latest, err)
	}
	if !strings.Contains(out.String(), "Verification incomplete:") ||
		!strings.Contains(out.String(), "not processed: unknown") {
		t.Fatalf("report claims complete inventory accounting:\n%s", &out)
	}
}

func TestTransferUnusualNamesAndEmptyFiles(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{
		"/sdcard/DCIM/space quote' and newline\n.jpg": []byte("exact"),
		"/sdcard/DCIM/写真/$(touch BAD).jpg":            []byte("unicode"),
		"/sdcard/DCIM/empty":                          {},
	}
	mustRun(t, c, d)
	for path, data := range d.files {
		assertContent(t, savedPath(c, path), data)
	}
	c.verify = true
	mustRun(t, c, d)
}

func TestTransferRejectsEscapingInventory(t *testing.T) {
	for _, path := range []string{"/sdcard/DCIM/../../outside", "/other/file", "/sdcard/DCIM-other/file", "/.android-transfer.lock"} {
		t.Run(path, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			d.files = map[string][]byte{path: []byte("no")}
			if err := runWithDevice(context.Background(), c, io.Discard, d); err == nil {
				t.Fatal("accepted unsafe inventory")
			}
			if d.pulls != 0 {
				t.Fatal("unsafe path copied")
			}
		})
	}
}

func TestTransferStopsOnDeviceErrors(t *testing.T) {
	for _, stage := range []string{"serial", "inventory", "inspect", "pull"} {
		t.Run(stage, func(t *testing.T) {
			c, d := testConfig(t), testDevice()
			boom := errors.New("device failure")
			switch stage {
			case "serial":
				d.serialErr = boom
			case "inventory":
				d.scanErr = boom
			case "inspect":
				d.inspectErr = boom
			case "pull":
				d.pullErr = boom
			}
			if err := runWithDevice(context.Background(), c, io.Discard, d); !errors.Is(err, boom) {
				t.Fatalf("lost device error: %v", err)
			}
			assertNoPublished(t, c)
		})
	}
}

func TestTransferEmptyInventoryCanDiscoverFilesLater(t *testing.T) {
	c, d := testConfig(t), testDevice()
	d.files = map[string][]byte{}
	mustRun(t, c, d)
	mustRun(t, c, d)
	if d.pulls != 0 {
		t.Fatal("empty inventory copied files")
	}
	d.files["/sdcard/DCIM/new"] = []byte("later")
	mustRun(t, c, d)
	assertContent(t, savedPath(c, "/sdcard/DCIM/new"), []byte("later"))
}

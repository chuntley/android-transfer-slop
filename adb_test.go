package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type adbFixture struct {
	SerialOutput string
	SerialExit   int
	Output       string
	Prefix       string
	Release      string
	Stderr       string
	Exit         int
	Delay        time.Duration
	Shell        bool
	Log          string
	Tools        string
}

// The test executable acts as adb, and optionally as portable stat/sha256sum
// fixtures, so tests exercise real process arguments, cancellation, and framing.
func TestADBHelperProcess(t *testing.T) {
	if os.Getenv("ANDROID_TRANSFER_ADB_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(90)
	}
	args = args[1:]
	if tool := os.Getenv("ANDROID_TRANSFER_ADB_TOOL"); tool != "" {
		switch tool {
		case "stat":
			if len(args) != 4 || args[0] != "-c" || args[1] != "%s %Y" || args[2] != "--" {
				os.Exit(91)
			}
			info, err := os.Lstat(args[3])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("%d %d\n", info.Size(), info.ModTime().Unix())
		case "sha256sum":
			h := sha256.New()
			if _, err := io.Copy(h, os.Stdin); err != nil {
				os.Exit(1)
			}
			fmt.Printf("%x  -\n", h.Sum(nil))
		default:
			os.Exit(92)
		}
		os.Exit(0)
	}
	data, err := os.ReadFile(os.Getenv("ANDROID_TRANSFER_ADB_FIXTURE"))
	if err != nil {
		os.Exit(93)
	}
	var fixture adbFixture
	if json.Unmarshal(data, &fixture) != nil {
		os.Exit(94)
	}
	log, err := os.OpenFile(fixture.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(95)
	}
	if json.NewEncoder(log).Encode(args) != nil || log.Close() != nil {
		os.Exit(96)
	}
	if len(args) > 0 && args[len(args)-1] == "get-serialno" {
		fmt.Fprint(os.Stdout, fixture.SerialOutput)
		fmt.Fprint(os.Stderr, fixture.Stderr)
		os.Exit(fixture.SerialExit)
	}
	if fixture.Delay > 0 {
		time.Sleep(fixture.Delay)
	}
	if fixture.Shell {
		if len(args) != 5 || args[0] != "-s" || args[2] != "shell" || args[3] != "-T" {
			os.Exit(97)
		}
		cmd := exec.Command("/bin/sh", "-c", args[4])
		cmd.Env = append(os.Environ(), "PATH="+fixture.Tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	fmt.Fprint(os.Stdout, fixture.Prefix)
	if fixture.Release != "" {
		for {
			if _, err := os.Stat(fixture.Release); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				os.Exit(98)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	fmt.Fprint(os.Stdout, fixture.Output)
	fmt.Fprint(os.Stderr, fixture.Stderr)
	os.Exit(fixture.Exit)
}

func setupADBFixture(t *testing.T, fixture adbFixture, timeout time.Duration) (*adbDevice, string) {
	t.Helper()
	dir := t.TempDir()
	fixture.Log = filepath.Join(dir, "calls.jsonl")
	fixture.Tools = filepath.Join(dir, "tools")
	if err := os.Mkdir(fixture.Tools, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Quote independently of the production quoting function under test.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	invocation := quote(executable) + " -test.run='^TestADBHelperProcess$' -- \"$@\"\n"
	binary := filepath.Join(dir, "fake adb")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec "+invocation), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stat", "sha256sum"} {
		body := "#!/bin/sh\nexport ANDROID_TRANSFER_ADB_TOOL=" + name + "\nexec " + invocation
		if err := os.WriteFile(filepath.Join(fixture.Tools, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(dir, "fixture.json")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANDROID_TRANSFER_ADB_HELPER", "1")
	t.Setenv("ANDROID_TRANSFER_ADB_FIXTURE", config)
	t.Setenv("ANDROID_TRANSFER_ADB_TOOL", "")
	// Race-instrumented helper subprocesses need no one-second exit grace period.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return newADB(binary, "", timeout), fixture.Log
}

func boundADBFixture(t *testing.T, fixture adbFixture) (*adbDevice, string) {
	t.Helper()
	fixture.SerialOutput = "phone-123\n"
	d, log := setupADBFixture(t, fixture, 15*time.Second)
	if _, err := d.Serial(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, log
}

func adbCalls(t *testing.T, log string) [][]string {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls [][]string
	decoder := json.NewDecoder(f)
	for {
		var args []string
		err := decoder.Decode(&args)
		if errors.Is(err, io.EOF) {
			return calls
		}
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, args)
	}
}

func TestADBSerialResolutionAndBinding(t *testing.T) {
	for _, selected := range []string{"", "chosen-device"} {
		t.Run("selector="+selected, func(t *testing.T) {
			d, log := setupADBFixture(t, adbFixture{SerialOutput: "resolved-device\n"}, 15*time.Second)
			d.requestedSerial = selected
			for range 2 {
				serial, err := d.Serial(context.Background())
				if err != nil || serial != "resolved-device" {
					t.Fatalf("serial = %q, error = %v", serial, err)
				}
			}
			source := "/photos/quote' space\n雪.jpg"
			local := filepath.Join(t.TempDir(), "local destination")
			if err := d.Pull(context.Background(), source, local); err != nil {
				t.Fatal(err)
			}
			first := []string{"get-serialno"}
			if selected != "" {
				first = []string{"-s", selected, "get-serialno"}
			}
			want := [][]string{first, {"-s", "resolved-device", "pull", "-a", "-Z", source, local}}
			if got := adbCalls(t, log); !reflect.DeepEqual(got, want) {
				t.Fatalf("adb calls = %#v, want %#v", got, want)
			}
		})
	}
}

func TestADBRejectsUnresolvedSerials(t *testing.T) {
	for _, serial := range []string{"", " \n", "unknown\n", "(null)", "offline", "unauthorized", "a\nb\n", "two devices", "serial\x00bad"} {
		t.Run(fmt.Sprintf("%q", serial), func(t *testing.T) {
			d, _ := setupADBFixture(t, adbFixture{SerialOutput: serial}, 15*time.Second)
			if _, err := d.Serial(context.Background()); err == nil {
				t.Fatal("accepted unresolved or ambiguous serial")
			}
			if err := d.Pull(context.Background(), "/photos/a", "/tmp/local"); err == nil {
				t.Fatal("unresolved device was allowed to pull")
			}
		})
	}
}

func TestADBSerialCommandFailure(t *testing.T) {
	d, _ := setupADBFixture(t, adbFixture{SerialExit: 1, Stderr: "error: more than one device/emulator"}, 15*time.Second)
	if _, err := d.Serial(context.Background()); err == nil || !strings.Contains(err.Error(), "more than one device") {
		t.Fatalf("lost useful resolution failure: %v", err)
	}
}

func TestADBRequiresBinding(t *testing.T) {
	d := newADB("must-not-execute", "requested-but-not-resolved", time.Second)
	if _, err := d.Inventory(context.Background(), []string{"/photos"}, nil); err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("inventory error = %v", err)
	}
	if _, err := d.Inspect(context.Background(), "/photos/a"); err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("inspect error = %v", err)
	}
	if err := d.Pull(context.Background(), "/photos/a", "/tmp/local"); err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("pull error = %v", err)
	}
}

func TestADBInventoryBinaryFramingAndDeduplication(t *testing.T) {
	a := "/photos/a ' \"\n雪.jpg"
	b := "/photos/sub/b.jpg"
	d, log := boundADBFixture(t, adbFixture{Output: b + "\x002 20\n" + a + "\x001 10\n" + b + "\x002 20\n"})
	got, err := d.Inventory(context.Background(), []string{"/photos/sub", "/photos", "/photos"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{Path: a, Size: 1, ModTime: 10}, {Path: b, Size: 2, ModTime: 20}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory = %#v, want %#v", got, want)
	}
	calls := adbCalls(t, log)
	if len(calls) != 2 || !reflect.DeepEqual(calls[1][:4], []string{"-s", "phone-123", "shell", "-T"}) || len(calls[1]) != 5 {
		t.Fatalf("unbound or split shell command: %#v", calls)
	}
	if strings.Count(calls[1][4], "find ") != 1 {
		t.Fatalf("overlapping roots walked repeatedly: %s", calls[1][4])
	}
}

func TestADBInventoryReportsBeforeCommandCompletes(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	a, b := "/photos/a '\n雪.jpg", "/photos/b.jpg"
	d, _ := boundADBFixture(t, adbFixture{
		Prefix:  a + "\x001 10\n" + b + "\x002 ",
		Output:  "20\n" + a + "\x001 10\n",
		Release: release,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var counts []int
	var current []string
	got, err := d.Inventory(ctx, []string{"/photos"}, func(count int, path string) {
		counts = append(counts, count)
		current = append(current, path)
		if count == 1 {
			// The helper cannot finish until a callback releases it. Whole-output
			// buffering would time out instead of reaching this callback.
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Error(err)
			}
		}
	})
	want := []entry{{Path: a, Size: 1, ModTime: 10}, {Path: b, Size: 2, ModTime: 20}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("streamed inventory = %#v, %v; want %#v", got, err, want)
	}
	if !reflect.DeepEqual(counts, []int{1, 2}) || !reflect.DeepEqual(current, []string{a, b}) {
		t.Fatalf("discovery progress = %v, %q", counts, current)
	}
}

func TestADBInventoryDiscardsStreamedRecordsOnFailure(t *testing.T) {
	for _, mode := range []string{"command error", "cancel", "timeout", "malformed while running", "truncated metadata"} {
		t.Run(mode, func(t *testing.T) {
			release := filepath.Join(t.TempDir(), "release")
			fixture := adbFixture{Prefix: "/photos/a\x001 2\n", Release: release}
			switch mode {
			case "command error":
				fixture.Exit, fixture.Stderr = 1, "traversal failed"
			case "malformed while running":
				fixture.Prefix += "/photos/b\x00bad metadata\n"
			case "truncated metadata":
				fixture.Output = "/photos/b\x002"
			}
			d, _ := boundADBFixture(t, fixture)
			if mode == "timeout" {
				d.timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var count int
			var current string
			got, err := d.Inventory(ctx, []string{"/photos"}, func(n int, path string) {
				count, current = n, path
				switch mode {
				case "cancel":
					cancel()
				case "timeout", "malformed while running":
					// Leave the producer blocked to exercise termination.
				default:
					if err := os.WriteFile(release, nil, 0600); err != nil {
						t.Error(err)
					}
				}
			})
			if err == nil || got != nil {
				t.Fatalf("partial inventory accepted: %#v, %v", got, err)
			}
			if count != 1 || current != "/photos/a" {
				t.Fatalf("valid discovery lost or malformed entry reported: %d, %q", count, current)
			}
			switch mode {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case "timeout":
				if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					t.Fatalf("configured command timeout not retained: %v", err)
				}
			case "command error":
				if !strings.Contains(err.Error(), "traversal failed") {
					t.Fatalf("command diagnostic lost: %v", err)
				}
			default:
				if errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "malformed") {
					t.Fatalf("malformed producer did not stop promptly: %v", err)
				}
			}
		})
	}
}

func TestADBInventoryRejectsMalformedOrConflictingRecords(t *testing.T) {
	cases := map[string]string{
		"no NUL":                "/photos/a 1 2\n",
		"empty path":            "\x001 2\n",
		"missing newline":       "/photos/a\x001 2",
		"missing mtime":         "/photos/a\x001\n",
		"negative size":         "/photos/a\x00-1 2\n",
		"nondecimal mtime":      "/photos/a\x001 nan\n",
		"overflow size":         "/photos/a\x009223372036854775808 2\n",
		"overflow mtime":        "/photos/a\x001 9223372036854775808\n",
		"extra field":           "/photos/a\x001 2 3\n",
		"relative path":         "photos/a\x001 2\n",
		"escape path":           "/photos/../a\x001 2\n",
		"outside root":          "/photos-other/a\x001 2\n",
		"conflicting duplicate": "/photos/a\x001 2\n/photos/a\x002 2\n",
		"trailing garbage":      "/photos/a\x001 2\ngarbage",
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			d, _ := boundADBFixture(t, adbFixture{Output: output})
			if got, err := d.Inventory(context.Background(), []string{"/photos"}, nil); err == nil || got != nil {
				t.Fatalf("malformed inventory accepted: %#v, %v", got, err)
			}
		})
	}
}

func TestADBActualShellHandlesHostileNamesWithoutMutation(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "photos '\n雪")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	// If quoting breaks, this attempts to create a sentinel outside the source.
	sentinel := filepath.Join(parent, "unexpected-write")
	names := []string{"ordinary.jpg", "line\nbreak.jpg", "quote'\"雪.jpg", "$(printf injected).jpg", "nested/inside.jpg"}
	content := []byte("photo data\x00\xff")
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "ordinary.jpg"), filepath.Join(root, "link.jpg")); err != nil {
		t.Fatal(err)
	}
	d, _ := boundADBFixture(t, adbFixture{Shell: true})
	got, err := d.Inventory(context.Background(), []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("regular files = %#v, want %d", got, len(names))
	}
	expectedHash := sha256.Sum256(content)
	for _, item := range got {
		fp, err := d.Inspect(context.Background(), item.Path)
		if err != nil {
			t.Fatal(err)
		}
		if fp.Size != int64(len(content)) || fp.ModTime != item.ModTime || fp.SHA256 != hex.EncodeToString(expectedHash[:]) {
			t.Fatalf("fingerprint = %#v", fp)
		}
		after, err := os.ReadFile(item.Path)
		if err != nil || string(after) != string(content) {
			t.Fatalf("source content changed: %q, %v", after, err)
		}
	}
	if _, err := d.Inspect(context.Background(), filepath.Join(root, "link.jpg")); err == nil {
		t.Fatal("inspect followed a symlink")
	}
	if _, err := d.Inspect(context.Background(), root); err == nil {
		t.Fatal("inspect accepted a directory")
	}
	injected := root + "'; touch " + sentinel + "; #"
	if _, err := d.Inventory(context.Background(), []string{injected}, nil); err == nil {
		t.Fatal("nonexistent hostile root was accepted")
	}
	if _, err := d.Inspect(context.Background(), injected); err == nil {
		t.Fatal("nonexistent hostile source was accepted")
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell injection created sentinel: %v", err)
	}
}

func TestADBActualShellMissingRootsAndEmptyTrees(t *testing.T) {
	root := t.TempDir()
	d, _ := boundADBFixture(t, adbFixture{Shell: true})
	entries, err := d.Inventory(context.Background(), []string{root}, nil)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty directory = %#v, %v", entries, err)
	}
	// Missing nested roots must not be hidden by overlapping-root pruning.
	if _, err := d.Inventory(context.Background(), []string{root, filepath.Join(root, "missing")}, nil); err == nil {
		t.Fatal("missing overlapping root was silently ignored")
	}
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inventory(context.Background(), []string{file}, nil); err == nil {
		t.Fatal("file accepted as directory root")
	}
}

func TestADBInspectValidatesDigestAndStability(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	cases := []struct {
		name   string
		output string
		valid  bool
	}{
		{"valid", "0 -1\n" + hash + "  -\n0 -1\n", true},
		{"uppercase normalized", "2 3\n" + strings.ToUpper(hash) + "  -\n2 3\n", true},
		{"size changed", "2 3\n" + hash + "  -\n4 3\n", false},
		{"mtime changed", "2 3\n" + hash + "  -\n2 4\n", false},
		{"nonhex digest", "2 3\n" + strings.Repeat("z", 64) + "  -\n2 3\n", false},
		{"short digest", "2 3\nabc  -\n2 3\n", false},
		{"unexpected filename", "2 3\n" + hash + "  filename\n2 3\n", false},
		{"bad first stat", "2 nope\n" + hash + "  -\n2 3\n", false},
		{"bad last stat", "2 3\n" + hash + "  -\n-2 3\n", false},
		{"truncated", "2 3\n" + hash + "  -\n", false},
		{"extra output", "2 3\n" + hash + "  -\n2 3\nextra\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := boundADBFixture(t, adbFixture{Output: tc.output})
			fp, err := d.Inspect(context.Background(), "/photos/anything")
			if tc.valid {
				if err != nil || fp.SHA256 != hash {
					t.Fatalf("fingerprint = %#v, error = %v", fp, err)
				}
			} else if err == nil {
				t.Fatalf("accepted invalid fingerprint: %#v", fp)
			}
		})
	}
}

func TestADBErrorsDiscardPartialOutputAndBoundDiagnostics(t *testing.T) {
	for _, stderr := range []string{"permission denied", strings.Repeat("failure!", 3000)} {
		t.Run(fmt.Sprint(len(stderr)), func(t *testing.T) {
			d, _ := boundADBFixture(t, adbFixture{Output: "/photos/a\x001 2\n", Stderr: stderr, Exit: 1})
			items, err := d.Inventory(context.Background(), []string{"/photos"}, nil)
			if err == nil || items != nil {
				t.Fatalf("partial inventory accepted: %#v, %v", items, err)
			}
			if len(err.Error()) > adbErrorLimit+200 || !strings.Contains(err.Error(), stderr[:min(len(stderr), 8)]) {
				t.Fatalf("unhelpful or unbounded failure (%d bytes)", len(err.Error()))
			}
			if _, err := d.Inspect(context.Background(), "/photos/a"); err == nil {
				t.Fatal("inspect command failure ignored")
			}
			if err := d.Pull(context.Background(), "/photos/a", filepath.Join(t.TempDir(), "partial")); err == nil {
				t.Fatal("pull command failure ignored")
			}
		})
	}
}

func TestADBStderrDoesNotCorruptBinaryInventory(t *testing.T) {
	d, _ := boundADBFixture(t, adbFixture{Output: "/photos/a\x001 2\n", Stderr: "adb daemon diagnostic\n"})
	got, err := d.Inventory(context.Background(), []string{"/photos"}, nil)
	if err != nil || !reflect.DeepEqual(got, []entry{{Path: "/photos/a", Size: 1, ModTime: 2}}) {
		t.Fatalf("stderr mixed into protocol: %#v, %v", got, err)
	}
}

func TestADBContextAndCommandTimeout(t *testing.T) {
	for _, mode := range []string{"already cancelled", "cancelled while running", "deadline", "command timeout"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := boundADBFixture(t, adbFixture{Delay: 10 * time.Second})
			ctx := context.Background()
			want := context.DeadlineExceeded
			if mode == "already cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			} else if mode == "cancelled while running" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
				want = context.Canceled
			} else if mode == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			} else {
				d.timeout = 50 * time.Millisecond
			}
			start := time.Now()
			err := d.Pull(ctx, "/photos/a", filepath.Join(t.TempDir(), "partial"))
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("cancelled adb subprocess did not exit promptly")
			}
		})
	}
}

func TestADBCommandTimeoutResetsOnActivity(t *testing.T) {
	d := newADB("/bin/sh", "", 300*time.Millisecond)
	out, err := d.run(context.Background(), "-c", "printf first; sleep 0.2; printf second; sleep 0.2; printf third")
	if err != nil || string(out) != "firstsecondthird" {
		t.Fatalf("active command timed out: output=%q, error=%v", out, err)
	}
}

func TestADBRejectsUnsafePathFormsBeforeExecution(t *testing.T) {
	d := newADB("must-not-execute", "", time.Second)
	for _, p := range []string{"relative", "/photos/../outside", "/photos//a", "/photos/a\x00bad"} {
		if _, err := d.Inventory(context.Background(), []string{p}, nil); err == nil || strings.Contains(err.Error(), "serial") {
			t.Fatalf("inventory did not reject path %q: %v", p, err)
		}
		if _, err := d.Inspect(context.Background(), p); err == nil || strings.Contains(err.Error(), "serial") {
			t.Fatalf("inspect did not reject path %q: %v", p, err)
		}
		if err := d.Pull(context.Background(), p, "/tmp/local"); err == nil || strings.Contains(err.Error(), "serial") {
			t.Fatalf("pull did not reject path %q: %v", p, err)
		}
	}
}

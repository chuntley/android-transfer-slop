package main

import (
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

// Extend the existing real-shell fixture without depending on host stat syntax.
func TestADBDeleteToolHelper(t *testing.T) {
	tool := os.Getenv("ANDROID_TRANSFER_DELETE_TOOL")
	if tool == "" {
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
	fault := os.Getenv("ANDROID_TRANSFER_DELETE_FAULT")
	if fault == tool+"-fail" {
		os.Exit(1)
	}
	if fault == tool+"-malformed" {
		fmt.Println("malformed output")
		os.Exit(0)
	}
	switch tool {
	case "stat":
		if len(args) != 4 || args[0] != "-c" || args[1] != "%s %Y %d %i %f %Z" || args[2] != "--" {
			os.Exit(91)
		}
		info, err := os.Lstat(args[3])
		if err != nil {
			os.Exit(1)
		}
		stat := reflect.ValueOf(info.Sys()).Elem()
		ctime := stat.FieldByName("Ctim")
		if !ctime.IsValid() {
			ctime = stat.FieldByName("Ctimespec")
		}
		if !ctime.IsValid() {
			os.Exit(92)
		}
		fmt.Printf("%d %d %v %v %x %d\n", info.Size(), info.ModTime().Unix(), stat.FieldByName("Dev").Interface(), stat.FieldByName("Ino").Interface(), stat.FieldByName("Mode").Interface(), ctime.FieldByName("Sec").Int())
	case "sha256sum":
		h := sha256.New()
		if _, err := io.Copy(h, os.Stdin); err != nil {
			os.Exit(1)
		}
		source := os.Getenv("ANDROID_TRANSFER_DELETE_SOURCE")
		if fault == "sha256sum-invalid-digest" {
			fmt.Printf("%s  -\n", strings.Repeat("z", 64))
			os.Exit(0)
		}
		if fault == "replace-identity" || fault == "change-mtime" {
			info, err := os.Stat(source)
			if err != nil {
				os.Exit(1)
			}
			if fault == "replace-identity" {
				data, err := os.ReadFile(source)
				if err != nil {
					os.Exit(1)
				}
				tmp := source + ".replacement"
				if os.WriteFile(tmp, data, info.Mode().Perm()) != nil || os.Chtimes(tmp, info.ModTime(), info.ModTime()) != nil || os.Rename(tmp, source) != nil {
					os.Exit(1)
				}
			} else if os.Chtimes(source, info.ModTime(), info.ModTime().Add(2*time.Second)) != nil {
				os.Exit(1)
			}
		}
		fmt.Printf("%x  -\n", h.Sum(nil))
	default:
		os.Exit(93)
	}
	if fault == tool+"-extra-newline" {
		fmt.Println()
	}
	os.Exit(0)
}

func deleteADBFixture(t *testing.T, fault string) (*adbDevice, string) {
	t.Helper()
	d, log := boundADBFixture(t, adbFixture{Shell: true})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	for _, name := range []string{"stat", "sha256sum"} {
		body := "#!/bin/sh\nexport ANDROID_TRANSFER_DELETE_TOOL=" + name + "\nexec " + quote(executable) + " -test.run='^TestADBDeleteToolHelper$' -- \"$@\"\n"
		if err := os.WriteFile(filepath.Join(filepath.Dir(log), "tools", name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANDROID_TRANSFER_DELETE_FAULT", fault)
	t.Setenv("ANDROID_TRANSFER_DELETE_TOOL", "")
	return d, log
}

func deleteTestSource(t *testing.T, name string) (string, fingerprint, []byte) {
	t.Helper()
	source := filepath.Join(t.TempDir(), name)
	data := []byte("photo bytes\x00\xff")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return source, fingerprint{Size: info.Size(), ModTime: info.ModTime().Unix(), SHA256: hex.EncodeToString(hash[:])}, data
}

func assertDeleteSourceRetained(t *testing.T, source string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(source)
	if err != nil || string(got) != string(data) {
		t.Fatalf("source not retained intact: %q, %v", got, err)
	}
}

func TestADBRemoveIfMetadataMatchesChecksOnlyMetadata(t *testing.T) {
	source, expected, data := deleteTestSource(t, "quick source.jpg")
	d, _ := deleteADBFixture(t, "sha256sum-fail")
	if err := d.RemoveIfMetadataMatches(context.Background(), source, expected.Size, expected.ModTime); err != nil {
		t.Fatalf("metadata-only removal: %v", err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source remains after metadata-only removal: %v", err)
	}

	source, expected, data = deleteTestSource(t, "quick mismatch.jpg")
	if err := d.RemoveIfMetadataMatches(context.Background(), source, expected.Size+1, expected.ModTime); !errors.Is(err, errSourceChanged) {
		t.Fatalf("size mismatch error = %v", err)
	}
	assertDeleteSourceRetained(t, source, data)
}

func TestADBRemoveIfMetadataMatchesRetainsOnDeviceCheckFailures(t *testing.T) {
	for _, fault := range []string{"stat-fail", "stat-malformed", "stat-extra-newline"} {
		t.Run(fault, func(t *testing.T) {
			source, expected, data := deleteTestSource(t, "quick failure.jpg")
			d, _ := deleteADBFixture(t, fault)
			if err := d.RemoveIfMetadataMatches(context.Background(), source, expected.Size, expected.ModTime); err == nil {
				t.Fatal("metadata failure was accepted")
			}
			assertDeleteSourceRetained(t, source, data)
		})
	}
}

func TestADBRemoveVerifiedExactFileAndHostileNames(t *testing.T) {
	for _, name := range []string{"ordinary.jpg", "-option.jpg", "quote'\"雪\n$(printf injected);*.jpg"} {
		t.Run(name, func(t *testing.T) {
			source, expected, _ := deleteTestSource(t, name)
			neighbor := filepath.Join(filepath.Dir(source), "unrelated.jpg")
			if err := os.WriteFile(neighbor, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			d, log := deleteADBFixture(t, "")
			// Uppercase expected digests are valid hexadecimal too.
			expected.SHA256 = strings.ToUpper(expected.SHA256)
			if err := d.RemoveVerified(context.Background(), source, expected); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("verified source remains: %v", err)
			}
			assertDeleteSourceRetained(t, neighbor, []byte("keep"))
			calls := adbCalls(t, log)
			if len(calls) != 2 || len(calls[1]) != 5 || calls[1][0] != "-s" || calls[1][1] != "phone-123" {
				t.Fatalf("deletion did not bind to resolved device: %#v", calls)
			}
		})
	}
}

func TestADBRemoveVerifiedRetainsFingerprintMismatches(t *testing.T) {
	for _, field := range []string{"size", "mtime", "hash"} {
		t.Run(field, func(t *testing.T) {
			source, expected, data := deleteTestSource(t, "source.jpg")
			switch field {
			case "size":
				expected.Size++
			case "mtime":
				expected.ModTime++
			case "hash":
				expected.SHA256 = strings.Repeat("0", 64)
			}
			d, _ := deleteADBFixture(t, "")
			if err := d.RemoveVerified(context.Background(), source, expected); !errors.Is(err, errSourceChanged) {
				t.Fatalf("mismatch error = %v", err)
			}
			assertDeleteSourceRetained(t, source, data)
		})
	}
}

func TestADBRemoveVerifiedRetainsChangedAndFailedSources(t *testing.T) {
	for _, fault := range []string{"stat-fail", "stat-malformed", "stat-extra-newline", "sha256sum-fail", "sha256sum-malformed", "sha256sum-invalid-digest", "sha256sum-extra-newline", "replace-identity", "change-mtime", "rm-fail"} {
		t.Run(fault, func(t *testing.T) {
			source, expected, data := deleteTestSource(t, "source.jpg")
			d, log := deleteADBFixture(t, fault)
			t.Setenv("ANDROID_TRANSFER_DELETE_SOURCE", source)
			if fault == "rm-fail" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(log), "tools", "rm"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := d.RemoveVerified(context.Background(), source, expected)
			changed := fault == "replace-identity" || fault == "change-mtime"
			if err == nil || errors.Is(err, errSourceChanged) != changed {
				t.Fatalf("error = %v, want changed=%v", err, changed)
			}
			assertDeleteSourceRetained(t, source, data)
		})
	}
}

func TestADBRemoveVerifiedRejectsNonregularSources(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "missing"} {
		t.Run(kind, func(t *testing.T) {
			source, expected, data := deleteTestSource(t, "source.jpg")
			candidate := filepath.Join(filepath.Dir(source), "candidate")
			switch kind {
			case "symlink":
				if err := os.Symlink(source, candidate); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(candidate, 0700); err != nil {
					t.Fatal(err)
				}
			}
			d, _ := deleteADBFixture(t, "")
			if err := d.RemoveVerified(context.Background(), candidate, expected); err == nil {
				t.Fatal("accepted nonregular source")
			}
			assertDeleteSourceRetained(t, source, data)
			if kind != "missing" {
				if _, err := os.Lstat(candidate); err != nil {
					t.Fatalf("nonregular source removed: %v", err)
				}
			}
		})
	}
}

func TestADBRemoveVerifiedRejectsInvalidInputsBeforeExecution(t *testing.T) {
	source, expected, data := deleteTestSource(t, "source.jpg")
	d, log := deleteADBFixture(t, "")
	for _, candidate := range []string{"relative", "/photos/../outside", "/photos//a", "/photos/a\x00bad"} {
		if err := d.RemoveVerified(context.Background(), candidate, expected); err == nil {
			t.Fatalf("accepted path %q", candidate)
		}
	}
	for _, digest := range []string{"", "abc", strings.Repeat("z", 64), "'" + strings.Repeat("0", 63)} {
		bad := expected
		bad.SHA256 = digest
		if err := d.RemoveVerified(context.Background(), source, bad); err == nil {
			t.Fatalf("accepted digest %q", digest)
		}
	}
	bad := expected
	bad.Size = -1
	if err := d.RemoveVerified(context.Background(), source, bad); err == nil {
		t.Fatal("accepted negative size")
	}
	if calls := adbCalls(t, log); len(calls) != 1 {
		t.Fatalf("invalid input executed ADB: %#v", calls)
	}
	assertDeleteSourceRetained(t, source, data)
}

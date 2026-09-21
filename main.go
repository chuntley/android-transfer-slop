package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

type sources []string

func (s *sources) String() string { return strings.Join(*s, ", ") }
func (s *sources) Set(value string) error {
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) || path.Clean(value) == "/" {
		return fmt.Errorf("source must be an absolute device directory other than /: %q", value)
	}
	*s = append(*s, path.Clean(value))
	return nil
}

type config struct {
	adb, serial, dest string
	sources           sources
	batchSize         int
	batchBytes        int64
	maxBatches        int
	timeout           time.Duration
	verify            bool
	safeDelete        bool // GUI-only, explicitly confirmed destructive operation
	quickDelete       bool // GUI-only, explicitly confirmed metadata-only destructive operation
	gui, noOpen       bool
	onProgress        func(transferProgress)
}

func parseConfig(args []string, out io.Writer) (config, error) {
	var c config
	f := flag.NewFlagSet("android-transfer-slop", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&c.dest, "dest", "", "required destination directory on this computer")
	f.Var(&c.sources, "source", "absolute device directory; repeat for multiple roots (default /sdcard/DCIM)")
	f.StringVar(&c.serial, "serial", "", "ADB device serial (required when multiple devices are connected)")
	f.StringVar(&c.adb, "adb", "adb", "path to the Android SDK adb executable")
	f.IntVar(&c.batchSize, "batch-size", 250, "maximum files per batch")
	f.Int64Var(&c.batchBytes, "batch-bytes", 2<<30, "maximum source bytes per batch; one oversized file gets its own batch")
	f.IntVar(&c.maxBatches, "max-batches", 0, "stop successfully after this many batches; 0 processes all files")
	f.DurationVar(&c.timeout, "timeout", defaultADBTimeout, "idle timeout for each ADB command")
	f.BoolVar(&c.verify, "verify", false, "compare every selected phone file with its local copy; copy nothing")
	f.BoolVar(&c.gui, "gui", false, "open the local browser interface")
	f.BoolVar(&c.noOpen, "no-open", false, "with -gui, print the URL without opening a browser")
	f.Usage = func() {
		fmt.Fprintln(out, `Usage: android-transfer-slop -gui
       android-transfer-slop -dest DIRECTORY [options]

Copies regular files through ADB, never MTP. Transfer and verification never modify
device files. GUI Safe Source Delete is a separate, confirmed destructive action
that requires fresh matching destination hashes. GUI Quick Source Delete is a
separate, explicitly weaker action that checks only path, size, and modification
time and does not hash contents. Keep both folders idle.
Enable USB debugging and authorize this computer on the unlocked phone first.
In GUI mode, choose transfer settings in the interface; only -adb, -timeout,
and -no-open configure its launch.

Contents are copied relative to the selected source folder, e.g. with -source /sdcard/DCIM:
  /sdcard/DCIM/Camera/photo.jpg -> DIRECTORY/Camera/photo.jpg
With -source /sdcard/DCIM/Camera, the destination is DIRECTORY/photo.jpg.
Multiple roots share the destination; overlapping roots use the most specific root.
Distinct source files mapping to the same destination path stop the run before copying.
All regular files in selected roots are included, including videos and sidecars.
Cloud-only media, private app storage, and Secure Folder are not exported.

Each new or replacement file is copied to a temporary local file, flushed, reread,
and hashed. Its hash must match a fresh device SHA-256 before publication; device
size and mtime must stay stable across hashing and match the inventory.
The phone is authoritative: matching copies are skipped; mismatched regular files
are atomically replaced only after a fresh copy is verified. Failed or interrupted
replacements leave the existing copy intact. Preserve wanted local edits elsewhere.
Keep the source folders idle during transfer. Hashing proves byte equality,
not that an original image or video is decodable.

Rerun the same command after interruption. Every run inventories the phone and
rehashes existing copies before skipping them. No database or cached success
state is used. Incomplete individual files restart. Use -verify to compare all
selected phone files with local copies without copying or replacing anything;
the phone must be connected. Extra local files are left alone.
The destination is locked against another instance; do not modify it during a run.

Batch limits count new copies and replacements. -max-batches allows incremental runs.
Any transfer error stops the run; already verified copies remain available.
Destination storage must support advisory locking and durable file operations (macOS/Linux).

Example:
  android-transfer-slop -dest /Volumes/Photos/Samsung -source /sdcard/DCIM -source /sdcard/Pictures

Options:`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 || (!c.gui && c.dest == "") {
		return c, errors.New("use -gui or provide -dest; positional arguments are not supported (use -help)")
	}
	if c.batchSize < 1 || c.batchBytes < 1 || c.maxBatches < 0 || c.timeout <= 0 {
		return c, errors.New("batch-size, batch-bytes, and timeout must be positive; max-batches must be nonnegative")
	}
	if c.noOpen && !c.gui {
		return c, errors.New("-no-open requires -gui")
	}
	if c.gui {
		var unsupported []string
		f.Visit(func(option *flag.Flag) {
			switch option.Name {
			case "dest", "source", "serial", "batch-size", "batch-bytes", "max-batches", "verify":
				unsupported = append(unsupported, "-"+option.Name)
			}
		})
		if len(unsupported) > 0 {
			return c, fmt.Errorf("with -gui, choose transfer settings in the interface instead of passing %s", strings.Join(unsupported, ", "))
		}
	}
	if len(c.sources) == 0 {
		c.sources = sources{"/sdcard/DCIM"}
	}
	return c, nil
}

func main() {
	c, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if c.gui {
			err = serveGUI(ctx, c, os.Stdout)
		} else {
			err = run(ctx, c, os.Stdout)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

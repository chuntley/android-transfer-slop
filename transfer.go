package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

var (
	errLocalMissing    = errors.New("local copy is missing")
	errContentMismatch = errors.New("local content does not match source SHA-256; destination not replaced")
	errSourceChanged   = errors.New("source changed since inventory or while hashing; keep source folders idle and rerun")
)

func run(ctx context.Context, c config, out io.Writer) error {
	return runWithDevice(ctx, c, out, newADB(c.adb, c.serial, c.timeout))
}

func runWithDevice(ctx context.Context, c config, out io.Writer, dev device) (result error) {
	if c.verify && (c.safeDelete || c.quickDelete) {
		return errors.New("verification and source deletion cannot run together")
	}
	if c.safeDelete && c.quickDelete {
		return errors.New("safe and quick source deletion cannot run together")
	}
	progress := transferProgress{Phase: "scanning"}
	inventoryComplete := false
	if c.safeDelete || c.quickDelete {
		defer func() {
			if !inventoryComplete {
				mode := "Safe Source Delete"
				if c.quickDelete {
					mode = "Quick Source Delete"
				}
				fmt.Fprintf(out, "%s incomplete: deleted: 0; not processed: unknown (inventory incomplete). No source files deleted. Error: %v\n", mode, result)
			}
		}()
	}
	if c.verify {
		defer func() {
			unprocessed := "unknown (inventory incomplete)"
			state := "incomplete"
			if inventoryComplete {
				unprocessed = fmt.Sprint(progress.Total - progress.Checked)
				if result == nil && progress.Checked == progress.Total {
					state = "complete"
				}
			}
			summary := fmt.Sprintf("Verification %s: checked: %d; verified: %d; missing: %d; mismatched: %d; source changed: %d; errors: %d; not processed: %s.",
				state, progress.Checked, progress.Verified, progress.Missing, progress.Mismatched, progress.Changed, progress.Errors, unprocessed)
			if result != nil {
				fmt.Fprintf(out, "Verification stopped: %q\n", result.Error())
				result = fmt.Errorf("%s: %w", summary, result)
			} else if progress.Checked != progress.Verified {
				result = errors.New(summary)
			}
			fmt.Fprintf(out, "%s No device files modified; no local copies written.\n", summary)
		}()
	}
	c.report(progress)
	dest, err := openDestination(c.dest, !c.verify && !c.safeDelete && !c.quickDelete)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, dest.Close()) }()
	serial, err := dev.Serial(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Device %q: scanning selected folders (read-only)...\n", serial)
	entries, err := dev.Inventory(ctx, c.sources, func(scanned int, current string) {
		progress.Scanned, progress.Current = scanned, current
		c.report(progress)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("inventory scan stopped after %s without device activity; increase -timeout and keep the phone awake: %w", c.timeout, err)
		}
		return err
	}
	var paths map[string]string
	if len(c.sources) > 1 {
		paths = make(map[string]string, len(entries))
	}
	for _, entry := range entries {
		rel, err := localPath(entry.Path, c.sources)
		if err != nil {
			return err
		}
		if entry.Size < 0 {
			return fmt.Errorf("invalid inventory size: %q", entry.Path)
		}
		if paths != nil {
			if previous, exists := paths[rel]; exists && previous != entry.Path {
				return fmt.Errorf("sources %q and %q map to the same destination %q; use separate destinations", previous, entry.Path, rel)
			}
			paths[rel] = entry.Path
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if c.safeDelete || c.quickDelete {
		mode := "Safe Source Delete"
		if c.quickDelete {
			mode = "Quick Source Delete"
		}
		fmt.Fprintf(out, "%s inventory: %d files. Destination files will not be copied or replaced.\n", mode, len(entries))
	} else {
		fmt.Fprintf(out, "Inventory: %d files. Existing copies will be hash-verified; transfer mode replaces mismatches with verified source copies.\n", len(entries))
	}
	progress.Total = len(entries)
	inventoryComplete = true
	progress.Current = ""
	progress.Phase = "transferring"
	if c.verify {
		progress.Phase = "verifying"
	}
	if c.safeDelete || c.quickDelete {
		progress.Phase = "deleting"
	}
	c.report(progress)
	if c.safeDelete || c.quickDelete {
		return deleteFiles(ctx, c, out, dev, dest, entries, &progress, c.quickDelete)
	}
	if c.verify {
		return verifyFiles(ctx, c, out, dev, dest, entries, &progress)
	}
	batch, count, copiedCount, verifiedCount := 0, 0, 0, 0
	var used int64
	for _, file := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := localPath(file.Path, c.sources) // validated above before any copies
		progress.Current = file.Path
		c.report(progress)
		existing, err := checkCopy(ctx, dev, dest, file, rel)
		copied := errors.Is(err, errLocalMissing) || errors.Is(err, errContentMismatch)
		if err != nil && !copied {
			return fmt.Errorf("%q: %w", file.Path, err)
		}
		if copied {
			if count == 0 || count >= c.batchSize || file.Size > c.batchBytes-used {
				if c.maxBatches > 0 && batch >= c.maxBatches {
					break
				}
				batch++
				count, used = 0, 0
				fmt.Fprintf(out, "Batch %d\n", batch)
			}
			if err := copyFile(ctx, dev, dest, file, rel, existing); err != nil {
				return fmt.Errorf("%q: %w", file.Path, err)
			}
		}
		verifiedCount++
		action := "skipped (SHA-256 matches)"
		if copied {
			count++
			used += file.Size
			copiedCount++
			action = "copied and verified"
			if existing != nil {
				action = "replaced mismatch and verified"
			}
		}
		fmt.Fprintf(out, "  %s %q (%d bytes)\n", action, file.Path, file.Size)
		progress.Verified = verifiedCount
		progress.Checked = verifiedCount
		progress.Copied = copiedCount
		c.report(progress)
	}
	fmt.Fprintf(out, "Copied: %d; skipped: %d; verified: %d; not processed: %d. No device files modified.\n", copiedCount, verifiedCount-copiedCount, verifiedCount, len(entries)-verifiedCount)
	return nil
}

func verifyFiles(ctx context.Context, c config, out io.Writer, dev device, dest *destination, entries []entry, progress *transferProgress) error {
	for _, file := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.Current = file.Path
		c.report(*progress)
		rel, _ := localPath(file.Path, c.sources) // validated before verification
		_, err := checkCopy(ctx, dev, dest, file, rel)
		progress.Checked++
		if err == nil {
			progress.Verified++
			fmt.Fprintf(out, "  verified existing %q (%d bytes)\n", file.Path, file.Size)
		} else {
			var category, action string
			switch {
			case errors.Is(err, errLocalMissing):
				progress.Missing++
				category, action = "MISSING", "Local file or parent folder is missing; use transfer mode to copy missing files."
			case errors.Is(err, errContentMismatch):
				progress.Mismatched++
				category, action = "MISMATCH", "Use transfer mode to replace this copy with the phone's version; the phone is authoritative."
			case errors.Is(err, errSourceChanged):
				progress.Changed++
				category, action = "SOURCE CHANGED", "Keep source folders idle, check the source file, and rerun verification."
			default:
				progress.Errors++
				category, action = "ERROR", "Check device connection, local permissions, and path safety, then rerun verification."
			}
			fmt.Fprintf(out, "  %s %q: %q. %s\n", category, file.Path, err.Error(), action)
		}
		c.report(*progress)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
	}
	return ctx.Err()
}

// Only local filesystem failures are classified as missing copies. A missing
// source or a failed device operation must never be reported as a missing copy.
func localFileError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", errLocalMissing, err)
	}
	return err
}

// checkCopy never writes. A missing or mismatched copy can be transferred;
// other failures must stop transfer mode rather than trigger replacement.
func checkCopy(ctx context.Context, dev device, dest *destination, file entry, rel string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := dest.ensureParents(rel, false); err != nil {
		return nil, localFileError(err)
	}
	info, err := dest.root.Lstat(rel)
	if err != nil {
		return nil, localFileError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("existing destination is not a regular file; refusing to replace it")
	}
	if err := dest.exactName(rel); err != nil {
		return nil, localFileError(err)
	}
	if err := compareCopy(ctx, dev, dest, file, rel, false); err != nil {
		if errors.Is(err, errContentMismatch) {
			return info, err
		}
		return nil, err
	}
	return info, nil
}

func compareCopy(ctx context.Context, dev device, dest *destination, file entry, rel string, flush bool) error {
	local, err := dest.hash(ctx, rel, flush)
	if err != nil {
		return localFileError(err)
	}
	after, err := dev.Inspect(ctx, file.Path)
	if err != nil {
		return err
	}
	if after.Size != file.Size || after.ModTime != file.ModTime {
		return errSourceChanged
	}
	if local.Size != after.Size || local.SHA256 != after.SHA256 {
		return errContentMismatch
	}
	return ctx.Err()
}

func copyFile(ctx context.Context, dev device, dest *destination, file entry, rel string, existing os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dest.ensureParents(rel, true); err != nil {
		return err
	}
	temp, err := dest.temporary()
	if err != nil {
		return err
	}
	// Keep the existing copy until its replacement is flushed and verified.
	// Only this invocation's temporary file is removed on failure.
	defer dest.root.Remove(temp)
	if err := dev.Pull(ctx, file.Path, filepath.Join(dest.root.Name(), temp)); err != nil {
		return err
	}
	if err := compareCopy(ctx, dev, dest, file, temp, true); err != nil {
		return err
	}
	if err := dest.ensureParents(rel, false); err != nil {
		return err
	}
	if existing != nil {
		return dest.replace(temp, rel, existing)
	}
	return dest.publish(temp, rel)
}

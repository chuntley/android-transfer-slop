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
	"strings"
	"syscall"
)

// sourceDeleter is intentionally separate from the read-only device interface.
// Implementations must freshly check the complete source fingerprint before
// removing exactly this regular file, and fail closed on any uncertainty.
type sourceDeleter interface {
	RemoveVerified(context.Context, string, fingerprint) error
}

type sourceMetadataDeleter interface {
	RemoveIfMetadataMatches(context.Context, string, int64, int64) error
}

var errDeletionUnconfirmed = errors.New("source deletion was not confirmed; inspect the phone before retrying")

func deleteFiles(ctx context.Context, c config, out io.Writer, dev device, dest *destination, entries []entry, progress *transferProgress, quick bool) (result error) {
	uncertain := 0
	mode := "Safe Source Delete"
	if quick {
		mode = "Quick Source Delete"
	}
	defer func() {
		retained, unprocessed := progress.Checked-progress.Deleted-uncertain, len(entries)-progress.Checked
		status := "complete"
		if result != nil || retained != 0 || uncertain != 0 || unprocessed != 0 || progress.Errors != 0 {
			status = "incomplete"
			if result == nil {
				result = fmt.Errorf("%s retained %d checked files; %d not processed", mode, retained, unprocessed)
			}
		}
		fmt.Fprintf(out, "%s %s: checked: %d; verified: %d; deleted: %d; retained: %d; deletion unconfirmed: %d; missing: %d; mismatched: %d; changed: %d; errors: %d; not processed: %d. Destination files were not copied or replaced.\n",
			mode, status, progress.Checked, progress.Verified, progress.Deleted, retained, uncertain, progress.Missing, progress.Mismatched, progress.Changed, progress.Errors, unprocessed)
	}()
	var safeDeleter sourceDeleter
	var metadataDeleter sourceMetadataDeleter
	if quick {
		metadataDeleter, _ = dev.(sourceMetadataDeleter)
		if metadataDeleter == nil {
			progress.Errors++
			c.report(*progress)
			return errors.New("device does not support metadata-only source deletion; no source files removed")
		}
	} else {
		safeDeleter, _ = dev.(sourceDeleter)
		if safeDeleter == nil {
			progress.Errors++
			c.report(*progress)
			return errors.New("device does not support conditional safe source deletion; no source files removed")
		}
	}
	for _, file := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.Current = file.Path
		c.report(*progress)
		var verified, deleted bool
		var err error
		if quick {
			verified, deleted, err = quickDeleteFile(ctx, c, metadataDeleter, dest, file)
		} else {
			verified, deleted, err = deleteFile(ctx, c, safeDeleter, dest, file)
		}
		progress.Checked++
		if verified {
			progress.Verified++
		}
		if deleted {
			progress.Deleted++
		}
		stop := false
		if err == nil {
			proof := "freshly SHA-256 verified"
			if quick {
				proof = "size and modification time checked only; contents were not hashed"
			}
			fmt.Fprintf(out, "  DELETED %q (%d bytes): %s against the destination copy\n", file.Path, file.Size, proof)
		} else {
			var category string
			switch {
			case errors.Is(err, errLocalMissing):
				progress.Missing++
				category = "MISSING COPY; RETAINED"
			case errors.Is(err, errContentMismatch):
				progress.Mismatched++
				category = "MISMATCH; RETAINED"
			case errors.Is(err, errSourceChanged):
				progress.Changed++
				category = "SOURCE CHANGED; RETAINED"
			default:
				progress.Errors++
				category, stop = "ERROR; RETAINED", true
				if errors.Is(err, errDeletionUnconfirmed) {
					uncertain++
					category = "ERROR; DELETION UNCONFIRMED"
				}
				if deleted {
					category = "DELETED; LOCAL CLOSE ERROR"
				}
			}
			fmt.Fprintf(out, "  %s %q: %q\n", category, file.Path, err.Error())
		}
		c.report(*progress)
		if stop {
			return fmt.Errorf("%s %q: %w", mode, file.Path, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

type deletePathPart struct {
	name string
	info os.FileInfo
}

// An open os.Root survives renames. Never authorize deletion using a displaced
// backup after the user-selected destination has disappeared or been replaced.
func checkDeleteRoot(dest *destination) error {
	held, err := dest.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(dest.root.Name())
	if err != nil {
		return fmt.Errorf("destination root is no longer accessible: %w", err)
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(held, current) {
		return errors.New("destination root changed; source deletion refused")
	}
	return nil
}

// Capture each exact path component and its identity without creating anything.
func inspectDeletePath(dest *destination, rel string) ([]deletePathPart, error) {
	if err := checkDeleteRoot(dest); err != nil {
		return nil, err
	}
	parts := make([]deletePathPart, 0, strings.Count(rel, string(filepath.Separator))+1)
	name := ""
	for remaining := rel; remaining != ""; {
		component, rest, _ := strings.Cut(remaining, string(filepath.Separator))
		name = filepath.Join(name, component)
		info, err := dest.root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (rest != "" && !info.IsDir()) || (rest == "" && !info.Mode().IsRegular()) {
			kind := "regular file"
			if rest != "" {
				kind = "directory"
			}
			return nil, fmt.Errorf("destination path is not a real %s: %q", kind, name)
		}
		if err := dest.exactName(name); err != nil {
			return nil, err
		}
		parts = append(parts, deletePathPart{name: name, info: info})
		remaining = rest
	}
	return parts, nil
}

func unchangedDeleteCopy(before, after os.FileInfo) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func recheckDeletePath(dest *destination, parts []deletePathPart) error {
	if err := checkDeleteRoot(dest); err != nil {
		return err
	}
	for i, part := range parts {
		info, err := dest.root.Lstat(part.name)
		if err != nil {
			return fmt.Errorf("destination path changed before deletion: %q: %w", part.name, err)
		}
		unchanged := info.IsDir() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(part.info, info)
		if i == len(parts)-1 {
			unchanged = unchangedDeleteCopy(part.info, info)
		}
		if !unchanged {
			return fmt.Errorf("destination path changed before deletion: %q", part.name)
		}
		if err := dest.exactName(part.name); err != nil {
			return err
		}
	}
	return nil
}

func deleteFile(ctx context.Context, c config, deleter sourceDeleter, dest *destination, file entry) (verified, deleted bool, result error) {
	rel, err := localPath(file.Path, c.sources)
	if err != nil {
		return false, false, err
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	parts, err := inspectDeletePath(dest, rel)
	if err != nil {
		return false, false, localFileError(err)
	}
	f, err := dest.root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, false, fmt.Errorf("opening destination after path inspection: %w", err)
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	before := parts[len(parts)-1].info
	opened, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	if !unchangedDeleteCopy(before, opened) {
		return false, false, fmt.Errorf("destination changed while opening: %q", rel)
	}
	if err := recheckDeletePath(dest, parts); err != nil {
		return false, false, err
	}
	h := sha256.New()
	if dest.hashBuffer == nil {
		dest.hashBuffer = make([]byte, 256<<10)
	}
	n, err := io.CopyBuffer(h, contextReader{ctx: ctx, r: f}, dest.hashBuffer)
	if err != nil {
		return false, false, err
	}
	if err := f.Sync(); err != nil {
		return false, false, fmt.Errorf("flushing destination before source deletion: %w", err)
	}
	// Persist the directory entries as well as the bytes. No directories or
	// destination files are created, replaced, or removed in this mode.
	for i := len(parts) - 2; i >= 0; i-- {
		if err := dest.syncDir(parts[i].name); err != nil {
			return false, false, fmt.Errorf("flushing destination directory: %w", err)
		}
	}
	if err := dest.syncDir("."); err != nil {
		return false, false, fmt.Errorf("flushing destination root: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	if n != before.Size() || n != file.Size || !unchangedDeleteCopy(before, after) {
		return false, false, errContentMismatch
	}
	if err := recheckDeletePath(dest, parts); err != nil {
		return false, false, err
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	// RemoveVerified performs the single final device-side stat, hash, and
	// conditional removal. The local hash supplies the expected source bytes,
	// so a separate pre-removal Inspect would only duplicate that work.
	source := fingerprint{Size: file.Size, ModTime: file.ModTime, SHA256: hex.EncodeToString(h.Sum(nil))}
	// The descriptor stays open through removal. This is not a filesystem lock:
	// noncooperating writers must remain idle on both sides. RemoveVerified does
	// a final fresh source check, but pathname races cannot be eliminated by ADB.
	if err := deleter.RemoveVerified(ctx, file.Path, source); err != nil {
		if !errors.Is(err, errSourceChanged) {
			err = errors.Join(errDeletionUnconfirmed, err)
		}
		return false, false, err
	}
	return true, true, nil
}

func quickDeleteFile(ctx context.Context, c config, deleter sourceMetadataDeleter, dest *destination, file entry) (verified, deleted bool, result error) {
	rel, err := localPath(file.Path, c.sources)
	if err != nil {
		return false, false, err
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	parts, err := inspectDeletePath(dest, rel)
	if err != nil {
		return false, false, localFileError(err)
	}
	f, err := dest.root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, false, fmt.Errorf("opening destination after path inspection: %w", err)
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	before := parts[len(parts)-1].info
	opened, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	if !unchangedDeleteCopy(before, opened) {
		return false, false, fmt.Errorf("destination changed while opening: %q", rel)
	}
	if err := recheckDeletePath(dest, parts); err != nil {
		return false, false, err
	}
	if before.Size() != file.Size || before.ModTime().Unix() != file.ModTime {
		return false, false, errContentMismatch
	}
	if err := f.Sync(); err != nil {
		return false, false, fmt.Errorf("flushing destination before source deletion: %w", err)
	}
	for i := len(parts) - 2; i >= 0; i-- {
		if err := dest.syncDir(parts[i].name); err != nil {
			return false, false, fmt.Errorf("flushing destination directory: %w", err)
		}
	}
	if err := dest.syncDir("."); err != nil {
		return false, false, fmt.Errorf("flushing destination root: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	if !unchangedDeleteCopy(before, after) || after.Size() != file.Size || after.ModTime().Unix() != file.ModTime {
		return false, false, errContentMismatch
	}
	if err := recheckDeletePath(dest, parts); err != nil {
		return false, false, err
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	if err := deleter.RemoveIfMetadataMatches(ctx, file.Path, file.Size, file.ModTime); err != nil {
		if !errors.Is(err, errSourceChanged) {
			err = errors.Join(errDeletionUnconfirmed, err)
		}
		return false, false, err
	}
	return true, true, nil
}

# Android Transfer SLOP

## Back up Android files without giving up control

Copy photos, videos, documents, and sidecar files from an Android phone to local storage over ADB—without cloud sync or MTP.

- Resumable transfers with SHA-256 verification.
- A local browser UI with clear progress and downloadable reports.
- Safe Source Delete for byte-for-byte verified copies.
- Quick Source Delete when you deliberately accept metadata-only checks.
- Built for large folders, with batched, resumable runs designed for tens of thousands of files and hundreds of gigabytes in one folder.
- Transfer and Verify-only modes never change phone originals.

The phone stays authoritative. Matching copies are skipped, mismatches are repaired only after verification, and source deletion is always a separate confirmed action.

## Install

### 1. Install ADB

ADB is included in Android SDK Platform-Tools. Android Studio is not required.

macOS with [Homebrew](https://brew.sh/):

```sh
brew install --cask android-platform-tools
```

Debian or Ubuntu:

```sh
sudo apt install adb
```

Other systems: install [Android SDK Platform-Tools](https://developer.android.com/tools/releases/platform-tools), then put `adb` on your `PATH`.

Confirm ADB is available:

```sh
adb version
```

### 2. Install Android Transfer SLOP

```sh
curl -fsSL https://raw.githubusercontent.com/chuntley/android-transfer-slop/main/install.sh | sh
```

The installer detects macOS or Linux on Intel or ARM, verifies the release checksum, and installs `android-transfer-slop` into a writable directory on your `PATH`.

### 3. Connect and launch

Enable **Developer options → USB debugging**, connect the phone with a data-capable cable, unlock it, and approve the computer’s debugging request.

```sh
adb devices -l
android-transfer-slop -gui
```

The phone should show `device`, not `unauthorized` or `offline`. The app opens a local `127.0.0.1` page; keep the terminal running.

For a source-only checkout, use `go run . -gui` instead. The native destination picker is macOS-only; Linux users can enter an absolute destination path.

## Transfer your files

1. Select the connected phone.
2. Enter a source folder or use **Choose folder…**. Opening a folder selects it; subfolders and all regular files are included—not just photos.
3. Select a destination on your computer or attached drive, or enter its absolute path.
4. Click **Start / resume transfer**.

For a small trial, open **Advanced settings**, set **Files per batch** to `10` and **Maximum batches** to `1`. Inspect the copies, then set **Maximum batches** to `0` and start again to process the rest.

### What each action does

| Action | Phone files | Destination files |
| --- | --- | --- |
| **Start / resume transfer** | Unchanged | Copies missing files, skips hash matches, replaces mismatches only after verifying a fresh copy |
| **Verify only** | Unchanged | Reads and reports; never repairs or replaces files |
| **Safe Source Delete** | Permanently deletes only files with freshly SHA-256-verified destination copies | Reads and flushes existing copies; never copies, repairs, or replaces them |
| **Quick Source Delete** | Permanently deletes only files whose path, size, and modification time still match inventory | Reads and flushes existing copies; never copies, repairs, or replaces them; does not hash contents |

**Stop safely** interrupts a run. Completed copies remain, and completed deletions cannot be undone. **Closing the browser does not stop the run**; use Stop safely or press **Ctrl-C** in the terminal. Reopening the page restores the current run’s settings while the server is running.

### Folder layout

Only the selected folder’s **contents** go into the destination. Relative subfolders are preserved:

```text
Source: /sdcard/DCIM           → Destination: /Volumes/Backup/Phone
/sdcard/DCIM/Camera/photo.jpg  → /Volumes/Backup/Phone/Camera/photo.jpg

Source: /sdcard/DCIM/Camera    → Destination: /Volumes/Backup/Phone
/sdcard/DCIM/Camera/photo.jpg  → /Volumes/Backup/Phone/photo.jpg
```

Use the same source and destination selections when resuming, verifying, or deleting. Extra destination files are left alone. Existing copies made with a different folder layout are not moved automatically; choose the destination folder whose contents mirror the selected source.

### Resume and batch limits

Every run rescans the phone and hashes existing copies. There is no database or cached success state. Interrupted individual files restart from the beginning.

The defaults are **250 files** and **2 GiB** per batch. New copies and replacements count toward those limits; matching files do not. An oversized file gets its own batch. **Maximum batches = 0** processes all files. Batch limits group work rather than adding concurrency, and do not apply to verification or source deletion.

## Verify and download reports

**Verify only** compares each source file with its mirrored destination using SHA-256. Keep the phone connected. Counts distinguish matches, missing copies, mismatches, changed sources, other errors, and files not checked.

Use **Download verification report** or **Download deletion report** for all recorded paths, outcomes, and final counts—not just the recent activity visible on screen. Downloads during a run are partial. Reports remain available until the next accepted run or server shutdown; download them before restarting. Setup or scan failures produce incomplete reports with unknown totals if the inventory did not finish.

Verification checks source-to-destination coverage, not a two-way mirror. A mismatch does not establish which copy was changed. Transfer mode repairs missing or mismatched copies using the phone’s version; preserve any wanted local edits elsewhere first.

## Safe Source Delete

**Deletion is permanent. Keep a second backup before deleting originals.**

1. Finish transferring and check your copies.
2. Select the same phone, source folder, and destination.
3. Click **Safe Source Delete** beside Start / resume transfer.
4. Review the selected device and paths in the confirmation. Canceling makes no changes.

All regular source files in the selected folder and its subfolders are considered, regardless of batch limits. Directories are not deleted. A prior successful transfer or verification **never authorizes deletion by itself**.

### Deletion safeguards

- The complete source inventory must succeed before any file can be deleted.
- The destination must exist and be locked against another app instance.
- Each matching relative path must resolve to a regular file with exact directory-entry names. Symlinks, changed file identities, and displaced destination roots block deletion.
- Current source and destination bytes must match by SHA-256. The local file and directory entries are flushed before deletion, and the local descriptor stays open through removal.
- A final serial-pinned ADB command rehashes the source and checks stable size, modification time, inode, device, mode, and change time before removing that exact file. Removal is never recursive.
- Missing or mismatched copies and changed sources are retained. Unsafe filesystem or device errors stop the run.

- A failed or interrupted ADB command may have deleted a file before its reply was lost. Such outcomes are reported as **unconfirmed**, never presumed retained. Inspect the source before retrying.

Safe Source Delete remains content-based rather than size/mtime-based; same-size rewrites and restored timestamps are not safe deletion proofs. The current path hashes each local file once, then uses one final device-side verification/removal command per attempted file. The destructive action remains a separate button so transfer cannot silently delete phone originals.

**Keep both source and destination folders idle throughout the run.** ADB cannot atomically combine verification and removal, and an open destination descriptor cannot prevent another application from changing its contents or pathname. Rechecks narrow these races; they cannot eliminate them. Hash equality proves a current byte-for-byte copy, not a healthy original, readable photo, or failure-proof backup drive.

## Quick Source Delete

**Quick Source Delete is intentionally weaker and permanently destructive. It does not hash file contents.** Use it only when the source and destination are trusted, idle, and protected by another backup.

The separate **Quick Source Delete** button checks the selected regular-file path, size, and modification time against the completed phone inventory and the existing destination copy. It never creates, replaces, or removes destination files. Missing, size-mismatched, timestamp-mismatched, changed, or otherwise unverifiable files stay on the phone.

Same-size content rewrites, copied-back files with restored timestamps, and writes that race the final check can evade metadata-only verification. Choose **Safe Source Delete** when byte-for-byte content proof is required.

## Troubleshooting and limits

| Problem | What to check |
| --- | --- |
| Phone missing or `offline` | Unlock it, reconnect with a data-capable cable, try another USB port, then Refresh. |
| Phone `unauthorized` | Accept the USB debugging prompt on the phone, then Refresh. |
| `adb` not found | Install Platform-Tools or launch with `go run . -gui -adb /absolute/path/to/adb`. |
| Scan or large-file timeout | Relaunch with `android-transfer-slop -gui -timeout 6h` (or `go run . -gui -timeout 6h`). The default 4-hour limit is an inactivity timeout: it resets whenever the ADB command produces output. |
| Destination is locked | Stop the other run or app instance. Do not delete the lock file to bypass an active lock. |
| Missing or mismatched destination | Confirm the folder layout. Use transfer mode to copy or repair; do not delete phone originals to force progress. |
| Deletion unconfirmed | Inspect the phone and report before retrying; the command’s acknowledgement may have been lost. |

- Keep the computer awake and phone charged. Do not edit either folder during a run.
- Cloud-only media, private app storage, and Samsung Secure Folder need separate export. Screenshots, messaging media, and SD-card files may be outside DCIM.
- Hidden `.android-transfer-part-*` files may remain after a forced quit. They are never accepted as completed copies. `.android-transfer.lock` is normal; its advisory lock releases when the process exits.
- Turn off USB debugging afterward if you no longer need it.

## Command line

```sh
go run . -dest "$HOME/Pictures/PhoneBackup"
go run . -dest "$HOME/Pictures/PhoneBackup" -verify
go run . -dest /Volumes/Backup/Phone -source /sdcard/Pictures -serial SERIAL
```

Repeat `-source` for multiple roots. Source deletion is GUI-only. Use `go run . -help` for all options.

## Development

The project uses Go’s standard library and embedded HTML, CSS, and JavaScript. No frontend build step is required.

```sh
go test ./...
go vet ./...
go build -o android-transfer-slop .
```

# Android Transfer SLOP

Copy files from an Android phone to your computer over ADB, with SHA-256 verification and resumable runs. A local browser interface handles folder selection, progress, verification reports, and optional **Safe Source Delete**.

**Transfer and Verify only never change phone originals. Source deletion is a separate, explicitly confirmed action.** The phone is authoritative: matching destination files are skipped; mismatches are replaced only after a fresh copy is verified.

## Install the latest release

Run this once; it installs the latest release into a writable directory already on your `PATH`:

```sh
curl -fsSL https://raw.githubusercontent.com/chuntley/android-transfer-slop/main/install.sh | sh
```

The installer detects macOS or Linux on Intel or ARM, downloads the matching latest release, verifies its SHA-256 checksum, and installs `android-transfer-slop` where your shell can find it. If every existing `PATH` directory is protected, it uses `/usr/local/bin` and asks for `sudo`. It does not install `adb`; install Android SDK Platform-Tools separately.

Start the app from any directory:

```sh
android-transfer-slop -gui
```

## Requirements

- **Go 1.25+** and **Android SDK Platform-Tools** (`adb`). No Android Studio or root access required.
- An Android phone with USB debugging enabled, an authorized computer, and a data-capable USB cable.
- macOS or Linux, with destination storage that supports hard links and advisory locking. Use APFS on macOS; exFAT/FAT destinations are unsupported.
- Enough free space for the files and one full temporary copy when replacing a mismatch.

The native destination picker is macOS-only. On Linux, enter an absolute destination path. Files are processed sequentially, not concurrently.

## Quick start

### 1. Install the tools

On macOS with [Homebrew](https://brew.sh/):

```sh
brew install go
brew install --cask android-platform-tools
```

Otherwise, install [Go](https://go.dev/dl/) and [Android Platform-Tools](https://developer.android.com/tools/releases/platform-tools), and put their executables on your `PATH`.

### 2. Connect your phone

Enable **Developer options → USB debugging**, connect the phone, unlock it, and approve this computer’s debugging request. On Samsung phones, enable Developer options by tapping **Settings → About phone → Software information → Build number** seven times.

```sh
adb devices -l
```

Your phone should show `device`, not `unauthorized` or `offline`. Close other phone-transfer apps before starting.

### 3. Launch the app

```sh
git clone https://github.com/chuntley/android-transfer-slop.git
cd android-transfer-slop
go run . -gui
```

The browser opens a local `127.0.0.1` address. Keep the terminal running. The interface uses no cloud service or telemetry.

If the browser does not open:

```sh
go run . -gui -no-open
```

Open the URL printed in the terminal. To build a standalone executable instead:

```sh
go build -o android-transfer-slop .
./android-transfer-slop -gui
```

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
| **Safe Source Delete** | Permanently deletes only files with freshly verified destination copies | Reads and flushes existing copies; never copies, repairs, or replaces them |

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

**Keep both source and destination folders idle throughout the run.** ADB cannot atomically combine verification and removal, and an open destination descriptor cannot prevent another application from changing its contents or pathname. Rechecks narrow these races; they cannot eliminate them. Hash equality proves a current byte-for-byte copy, not a healthy original, readable photo, or failure-proof backup drive.

## Troubleshooting and limits

| Problem | What to check |
| --- | --- |
| Phone missing or `offline` | Unlock it, reconnect with a data-capable cable, try another USB port, then Refresh. |
| Phone `unauthorized` | Accept the USB debugging prompt on the phone, then Refresh. |
| `adb` not found | Install Platform-Tools or launch with `go run . -gui -adb /absolute/path/to/adb`. |
| Scan or large-file timeout | Relaunch with `android-transfer-slop -gui -timeout 6h` (or `go run . -gui -timeout 6h`). The default 4-hour limit applies to each ADB command, including the whole scan. |
| Destination is locked | Stop the other run or app instance. Do not delete the lock file to bypass an active lock. |
| Missing or mismatched destination | Confirm the folder layout. Use transfer mode to copy or repair; do not delete phone originals to force progress. |
| Deletion unconfirmed | Inspect the phone and report before retrying; the command’s acknowledgement may have been lost. |

- Keep the computer awake and phone charged. Do not edit either folder during a run.
- Cloud-only media, private app storage, and Samsung Secure Folder need separate export. Screenshots, messaging media, and SD-card files may be outside DCIM.
- Hidden `.android-transfer-part-*` files may remain after a forced quit. They are never accepted as completed copies. `.android-transfer.lock` is normal; its advisory lock releases when the process exits.
- Turn off USB debugging afterward if you no longer need it.

## Command-line use

```sh
# Copy DCIM; rerun to resume
go run . -dest "$HOME/Pictures/PhoneBackup"

# Verify without copying or replacing anything
go run . -dest "$HOME/Pictures/PhoneBackup" -verify

# Select another source and an explicit phone
go run . -dest /Volumes/Backup/Phone -source /sdcard/Pictures -serial SERIAL

# See every option
go run . -help
```

Repeat `-source` for multiple roots. Each root’s contents share the destination; overlapping roots use the most specific root. Distinct source files mapping to one destination path stop the run before copying. Use separate destinations for separate phones.

Command-line verification returns a nonzero exit status when problems are found. **Source deletion is GUI-only.** With `-gui`, choose transfer settings in the interface; only `-adb`, `-timeout`, and `-no-open` configure its launch.

## Development

The app uses Go’s standard library and embedded HTML, CSS, and vanilla JavaScript. No frontend build step or external Go dependencies are required.

```sh
go test -race -cover ./...
go vet ./...
go build -o android-transfer-slop .

# Optional large-library benchmarks; not a USB throughput measurement
go test -run '^$' -bench . -benchmem
```

Regression tests exercise real local files, in-memory devices, and fake-ADB processes that execute actual shell commands. They cover transfer recovery, fresh verification, deletion eligibility, path changes, symlinks, cancellation, malformed device output, and uncertain deletion outcomes. Browser smoke checks use simulated devices. These checks do not certify compatibility or performance on your physical phone; begin with a small transfer and keep another backup.

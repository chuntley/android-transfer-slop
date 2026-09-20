package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const adbErrorLimit = 4096

type adbDevice struct {
	binary          string
	requestedSerial string
	timeout         time.Duration
	mu              sync.Mutex
	serial          string
}

func newADB(binary, serial string, timeout time.Duration) *adbDevice {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &adbDevice{binary: binary, requestedSerial: serial, timeout: timeout}
}

// shellQuote quotes one POSIX shell word, including embedded quotes and newlines.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type adbLimitedBuffer struct {
	buf bytes.Buffer
}

func (b *adbLimitedBuffer) String() string { return b.buf.String() }

func (b *adbLimitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := adbErrorLimit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

func (d *adbDevice) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.binary, args...)
	// Bound waiting for a descendant that inherited adb's output pipes.
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	var stderr adbLimitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			data := stdout.Bytes()
			if len(data) > adbErrorLimit {
				data = data[:adbErrorLimit]
			}
			detail = strings.TrimSpace(string(data))
		}
		if detail != "" {
			return nil, fmt.Errorf("adb command failed: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("adb command failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func (d *adbDevice) Serial(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.serial != "" {
		return d.serial, nil
	}
	args := []string{"get-serialno"}
	if d.requestedSerial != "" {
		args = append([]string{"-s", d.requestedSerial}, args...)
	}
	out, err := d.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("resolve device serial: %w", err)
	}
	serial := strings.TrimSpace(string(out))
	if serial == "" || strings.IndexFunc(serial, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("adb returned a blank or ambiguous device serial")
	}
	switch strings.ToLower(serial) {
	case "unknown", "(null)", "null", "offline", "unauthorized", "ambiguous", "????????????":
		return "", errors.New("adb did not resolve an available device serial")
	}
	d.serial = serial
	return serial, nil
}

func (d *adbDevice) boundRun(ctx context.Context, args ...string) ([]byte, error) {
	d.mu.Lock()
	serial := d.serial
	d.mu.Unlock()
	if serial == "" {
		return nil, errors.New("device serial must be resolved before accessing files")
	}
	return d.run(ctx, append([]string{"-s", serial}, args...)...)
}

func validADBPath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsRune(p, '\x00')
}

func adbMetadata(line string) (int64, int64, error) {
	fields := strings.Split(line, " ")
	if len(fields) != 2 || !adbDecimal(fields[0], false) || !adbDecimal(fields[1], true) {
		return 0, 0, errors.New("malformed size/mtime from adb stat")
	}
	size, sizeErr := strconv.ParseInt(fields[0], 10, 64)
	mtime, timeErr := strconv.ParseInt(fields[1], 10, 64)
	if sizeErr != nil || timeErr != nil {
		return 0, 0, errors.New("out-of-range size/mtime from adb stat")
	}
	return size, mtime, nil
}

func adbDecimal(s string, signed bool) bool {
	if signed && strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (d *adbDevice) Inventory(ctx context.Context, roots []string, report func(int, string)) ([]entry, error) {
	unique := make(map[string]bool, len(roots))
	for _, root := range roots {
		if !validADBPath(root) {
			return nil, errors.New("inventory root must be a clean absolute path without NUL")
		}
		unique[root] = true
	}
	allRoots := make([]string, 0, len(unique))
	for root := range unique {
		allRoots = append(allRoots, root)
	}
	sort.Strings(allRoots)
	if len(allRoots) == 0 {
		return []entry{}, nil
	}
	var command strings.Builder
	// Check every explicitly requested root, even when another root covers it.
	for _, root := range allRoots {
		q := shellQuote(root)
		command.WriteString("if [ ! -d " + q + " ] || [ -L " + q + " ] || [ ! -r " + q + " ] || [ ! -x " + q + " ]; then printf '%s\\n' 'missing, inaccessible, or symlink inventory root' >&2; exit 1; fi; ")
	}
	walkRoots := make([]string, 0, len(allRoots))
	for _, root := range allRoots {
		covered := false
		for _, parent := range walkRoots {
			if strings.HasPrefix(root, strings.TrimSuffix(parent, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			walkRoots = append(walkRoots, root)
		}
	}
	// find's batched -exec propagates a failing child as a failing find. Do not
	// use a pipeline: it could hide traversal errors and commit a partial tree.
	loop := `for p do if [ -L "$p" ] || [ ! -f "$p" ]; then printf '%s\n' 'source is no longer a regular file' >&2; exit 1; fi; printf '%s\000' "$p" || exit; stat -c '%s %Y' -- "$p" || exit; done`
	for _, root := range walkRoots {
		command.WriteString("find " + shellQuote(root) + " -type f -exec sh -c " + shellQuote(loop) + " sh {} + || exit; ")
	}
	d.mu.Lock()
	serial := d.serial
	d.mu.Unlock()
	if serial == "" {
		return nil, errors.New("device serial must be resolved before accessing files")
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.binary, "-s", serial, "shell", "-T", command.String())
	cmd.WaitDelay = time.Second
	reader, writer := io.Pipe()
	var stderr adbLimitedBuffer
	cmd.Stdout, cmd.Stderr = writer, &stderr
	done := make(chan error, 1)
	go func() {
		err := cmd.Run()
		writer.Close()
		done <- err
	}()
	entries, parseErr := readADBInventory(bufio.NewReader(reader), walkRoots, report)
	contextErr := ctx.Err()
	if parseErr != nil {
		// Stop a malformed producer immediately; closing the reader also releases
		// the exec stdout copier if it is blocked writing another record.
		cancel()
	}
	reader.Close()
	commandErr := <-done
	if contextErr != nil {
		commandErr = contextErr
	} else if parseErr != nil {
		commandErr = parseErr
	}
	if commandErr != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("inventory device files: %w: %s", commandErr, detail)
		}
		return nil, fmt.Errorf("inventory device files: %w", commandErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func readADBInventory(reader *bufio.Reader, roots []string, report func(int, string)) ([]entry, error) {
	byPath := make(map[string]int)
	entries := make([]entry, 0)
	for {
		framedPath, err := reader.ReadString('\x00')
		if err == io.EOF && framedPath == "" {
			return entries, nil
		}
		if err != nil || len(framedPath) <= 1 {
			return nil, errors.New("malformed inventory path framing")
		}
		p := framedPath[:len(framedPath)-1]
		metadata, err := reader.ReadSlice('\n')
		if err != nil {
			return nil, errors.New("malformed inventory metadata framing")
		}
		size, mtime, err := adbMetadata(string(metadata[:len(metadata)-1]))
		if err != nil {
			return nil, err
		}
		inside := false
		for _, root := range roots {
			if strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
				inside = true
				break
			}
		}
		if !validADBPath(p) || !inside {
			return nil, errors.New("inventory returned an invalid or out-of-root path")
		}
		item := entry{Path: p, Size: size, ModTime: mtime}
		if index, ok := byPath[p]; ok {
			if entries[index] != item {
				return nil, errors.New("inventory returned conflicting metadata for a duplicate path")
			}
			continue
		}
		byPath[p] = len(entries)
		entries = append(entries, item)
		if report != nil {
			report(len(entries), p)
		}
	}
}

func (d *adbDevice) Inspect(ctx context.Context, source string) (fingerprint, error) {
	if !validADBPath(source) {
		return fingerprint{}, errors.New("source must be a clean absolute path without NUL")
	}
	// These checks reject existing symlinks. ADB's pathname-based shell/pull
	// interface cannot eliminate a replacement race after a check, nor detect
	// content rewritten without a metadata change. The caller rechecks after
	// pulling and compares the local hash before publishing a verified copy.
	command := "p=" + shellQuote(source) + `; if [ -L "$p" ] || [ ! -f "$p" ]; then printf '%s\n' 'source is not a regular file' >&2; exit 1; fi; stat -c '%s %Y' -- "$p" || exit; sha256sum < "$p" || exit; if [ -L "$p" ] || [ ! -f "$p" ]; then printf '%s\n' 'source is no longer a regular file' >&2; exit 1; fi; stat -c '%s %Y' -- "$p"`
	out, err := d.boundRun(ctx, "shell", "-T", command)
	if err != nil {
		return fingerprint{}, fmt.Errorf("inspect device file: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) != 4 || lines[3] != "" {
		return fingerprint{}, errors.New("malformed fingerprint framing")
	}
	size, mtime, err := adbMetadata(lines[0])
	if err != nil {
		return fingerprint{}, err
	}
	afterSize, afterTime, err := adbMetadata(lines[2])
	if err != nil {
		return fingerprint{}, err
	}
	if size != afterSize || mtime != afterTime {
		return fingerprint{}, fmt.Errorf("source metadata changed while hashing: %w", errSourceChanged)
	}
	hashFields := strings.Fields(lines[1])
	if len(hashFields) != 2 || len(hashFields[0]) != 64 || hashFields[1] != "-" {
		return fingerprint{}, errors.New("malformed SHA256 output")
	}
	if _, err := hex.DecodeString(hashFields[0]); err != nil {
		return fingerprint{}, errors.New("malformed SHA256 digest")
	}
	return fingerprint{Size: size, ModTime: mtime, SHA256: strings.ToLower(hashFields[0])}, nil
}

func (d *adbDevice) Pull(ctx context.Context, source, localTemp string) error {
	if !validADBPath(source) {
		return errors.New("source must be a clean absolute path without NUL")
	}
	_, err := d.boundRun(ctx, "pull", "-a", "-Z", source, localTemp)
	if err != nil {
		return fmt.Errorf("pull device file: %w", err)
	}
	return nil
}

// RemoveVerified rechecks content and source identity within one device shell.
// ADB exposes pathname operations, not atomic compare-and-unlink: replacement or
// in-place writes after the final check remain possible. Keep the phone and its
// source folders idle throughout deletion.
func (d *adbDevice) RemoveVerified(ctx context.Context, source string, expected fingerprint) error {
	if !validADBPath(source) {
		return errors.New("source must be a clean absolute path without NUL")
	}
	if expected.Size < 0 || len(expected.SHA256) != 64 {
		return errors.New("removal requires a complete valid source fingerprint")
	}
	if _, err := hex.DecodeString(expected.SHA256); err != nil {
		return errors.New("removal requires a valid SHA256 digest")
	}
	// Validate every tool result before interpreting it as a mismatch. Sentinels
	// preserve trailing newlines in command substitutions so malformed framing
	// cannot be silently stripped by the shell.
	command := "p=" + shellQuote(source) + "\nwant_meta=" +
		shellQuote(fmt.Sprintf("%d %d", expected.Size, expected.ModTime)) +
		"\nwant_hash=" + shellQuote(strings.ToLower(expected.SHA256)) + `
fail() { printf '%s\n' "$1" >&2; exit 1; }
changed() { printf 'changed\n'; exit 0; }
unsigned() {
	case "$1" in ''|*[!0-9]*) fail 'malformed stat number';; esac
}
signed() {
	case "$1" in -*) unsigned "${1#-}";; *) unsigned "$1";; esac
}
regular() {
	if [ -L "$p" ] || [ ! -f "$p" ]; then
		fail 'source is not a regular file'
	fi
}
read_stat() {
	metadata=$(stat -c '%s %Y %d %i %f %Z' -- "$p" && printf '|') || fail 'source stat failed'
	case "$metadata" in
		*'
|') metadata=${metadata%'
|'};;
		*) fail 'malformed stat framing';;
	esac
	set -f
	set -- $metadata
	[ "$#" -eq 6 ] || fail 'malformed stat fields'
	[ "$metadata" = "$1 $2 $3 $4 $5 $6" ] || fail 'malformed stat spacing'
	unsigned "$1"
	signed "$2"
	unsigned "$3"
	unsigned "$4"
	case "$5" in ''|*[!0-9a-fA-F]*) fail 'malformed stat mode';; esac
	signed "$6"
	source_meta="$1 $2"
}
regular
read_stat
before=$metadata
[ "$source_meta" = "$want_meta" ] || changed
sum=$(sha256sum < "$p" && printf '|') || fail 'source SHA256 failed'
case "$sum" in
	*'  -
|') digest=${sum%'  -
|'};;
	*) fail 'malformed SHA256 framing';;
esac
[ "${#digest}" -eq 64 ] || fail 'malformed SHA256 length'
case "$digest" in *[!0-9a-f]*) fail 'malformed SHA256 digest';; esac
regular
read_stat
[ "$before" = "$metadata" ] || changed
[ "$digest" = "$want_hash" ] || changed
regular
rm -- "$p" || fail 'source removal failed'
printf 'deleted\n'
`
	out, err := d.boundRun(ctx, "shell", "-T", command)
	if err != nil {
		return fmt.Errorf("remove verified device file: %w", err)
	}
	switch string(out) {
	case "changed\n":
		return errSourceChanged
	case "deleted\n":
		return nil
	default:
		return errors.New("uncertain source removal: malformed device response")
	}
}

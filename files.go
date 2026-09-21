package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

type destination struct {
	root       *os.Root
	lock       *os.File
	names      map[string]map[string]bool
	nameInfo   map[string]os.FileInfo
	hashBuffer []byte // reused by the sequential transfer loop
}

func openDestination(dir string, create bool) (*destination, error) {
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Resolve the user-selected root once; descendants must not be symlinks.
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	lock, err := r.OpenFile(".android-transfer.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		r.Close()
		return nil, err
	}
	info, err := lock.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("destination lock is not a regular file")
	}
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		lock.Close()
		r.Close()
		return nil, fmt.Errorf("locking destination (another transfer may be running): %w", err)
	}
	return &destination{root: r, lock: lock, names: make(map[string]map[string]bool), nameInfo: make(map[string]os.FileInfo)}, nil
}

func (d *destination) Close() error {
	return errors.Join(d.lock.Close(), d.root.Close())
}

func localPath(source string, roots []string) (string, error) {
	if !strings.HasPrefix(source, "/") || source == "/" || path.Clean(source) != source || strings.ContainsRune(source, 0) {
		return "", fmt.Errorf("unsafe source path %q", source)
	}
	// The most specific selected root wins when command-line roots overlap.
	root := ""
	for _, candidate := range roots {
		if len(candidate) > len(root) && strings.HasPrefix(source, candidate+"/") {
			root = candidate
		}
	}
	if root == "" {
		return "", fmt.Errorf("source outside selected roots: %q", source)
	}
	rel := source[len(root)+1:]
	first, _, _ := strings.Cut(rel, "/")
	if strings.HasPrefix(first, ".android-transfer") {
		return "", fmt.Errorf("source collides with transfer metadata: %q", source)
	}
	return filepath.FromSlash(rel), nil
}

func inRoots(file string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(file, root+"/") {
			return true
		}
	}
	return false
}

func (d *destination) directoryNames(dir string) (map[string]bool, error) {
	if names, ok := d.names[dir]; ok {
		info, err := d.root.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if previous, known := d.nameInfo[dir]; known &&
			os.SameFile(previous, info) &&
			previous.Size() == info.Size() &&
			previous.ModTime().Equal(info.ModTime()) {
			return names, nil
		}
		delete(d.names, dir)
		delete(d.nameInfo, dir)
	}
	f, err := d.root.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	info, err := d.root.Lstat(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	d.names[dir] = names
	d.nameInfo[dir] = info
	return names, nil
}

// Exact directory-entry names detect case/normalization collisions on macOS
// without rereading a large photo directory for every file.
func (d *destination) exactName(rel string) error {
	names, err := d.directoryNames(filepath.Dir(rel))
	if err != nil {
		return err
	}
	if !names[filepath.Base(rel)] {
		return fmt.Errorf("destination name collision or concurrent directory change: %q", rel)
	}
	return nil
}

func (d *destination) ensureParents(rel string, create bool) error {
	parent := "."
	for _, component := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if component == "." {
			continue
		}
		next := filepath.Join(parent, component)
		info, err := d.root.Lstat(next)
		if errors.Is(err, os.ErrNotExist) && create {
			names, listErr := d.directoryNames(parent)
			if listErr != nil {
				return listErr
			}
			if err = d.root.Mkdir(next, 0700); err != nil {
				return err
			}
			names[component] = true
			if err = d.syncDir(parent); err != nil {
				return err
			}
		} else {
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("destination parent is not a real directory: %q", next)
			}
			if err := d.exactName(next); err != nil {
				return err
			}
		}
		parent = next
	}
	return nil
}

func (d *destination) syncDir(dir string) error {
	f, err := d.root.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (d *destination) temporary() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	name := ".android-transfer-part-" + hex.EncodeToString(token[:])
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		d.root.Remove(name)
		return "", err
	}
	return name, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (d *destination) hash(ctx context.Context, rel string, flush bool) (fingerprint, error) {
	f, err := d.root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !before.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("destination is not a regular file: %q", rel)
	}
	if flush {
		if err := f.Sync(); err != nil {
			return fingerprint{}, fmt.Errorf("flushing local file: %w", err)
		}
	}
	h := sha256.New()
	if d.hashBuffer == nil {
		d.hashBuffer = make([]byte, 256<<10)
	}
	n, err := io.CopyBuffer(h, contextReader{ctx: ctx, r: f}, d.hashBuffer)
	if err != nil {
		return fingerprint{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if n != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fingerprint{}, fmt.Errorf("destination changed while hashing: %q", rel)
	}
	return fingerprint{Size: n, ModTime: after.ModTime().Unix(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (d *destination) publish(temp, rel string) error {
	// A new copy must not overwrite an entry created after the initial check.
	if err := d.root.Link(temp, rel); err == nil {
		if names, ok := d.names[filepath.Dir(rel)]; ok {
			names[filepath.Base(rel)] = true
		}
		return d.syncDir(filepath.Dir(rel))
	} else if !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return fmt.Errorf("publishing without overwrite: %w", err)
	}
	// Some filesystems do not implement hard links. O_EXCL preserves the
	// no-overwrite guarantee there, at the cost of copying the verified bytes
	// into the final name before publication completes.
	return d.publishExclusiveCopy(temp, rel)
}

func (d *destination) publishExclusiveCopy(temp, rel string) error {
	source, err := d.root.Open(temp)
	if err != nil {
		return err
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		source.Close()
		return err
	}
	target, err := d.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, sourceInfo.Mode().Perm())
	if err != nil {
		source.Close()
		return fmt.Errorf("publishing without overwrite: %w", err)
	}
	_, copyErr := io.Copy(target, source)
	metadataErr := target.Chmod(sourceInfo.Mode().Perm())
	if metadataErr == nil {
		metadataErr = syscall.Futimes(int(target.Fd()), []syscall.Timeval{
			syscall.NsecToTimeval(sourceInfo.ModTime().UnixNano()),
			syscall.NsecToTimeval(sourceInfo.ModTime().UnixNano()),
		})
	}
	syncErr := target.Sync()
	closeErr := errors.Join(target.Close(), source.Close())
	if err := errors.Join(copyErr, metadataErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("publishing without overwrite: %w", err)
	}
	if err := d.syncDir(filepath.Dir(rel)); err != nil {
		return err
	}
	if names, ok := d.names[filepath.Dir(rel)]; ok {
		names[filepath.Base(rel)] = true
	}
	return nil
}

func (d *destination) replace(temp, rel string, expected os.FileInfo) error {
	current, err := d.root.Lstat(rel)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(expected, current) ||
		current.Size() != expected.Size() || !current.ModTime().Equal(expected.ModTime()) {
		return fmt.Errorf("destination changed before replacement: %q", rel)
	}
	if err := d.exactName(rel); err != nil {
		return err
	}
	// Rename atomically switches readers from the old copy to the verified one.
	if err := d.root.Rename(temp, rel); err != nil {
		return fmt.Errorf("replacing mismatched copy: %w", err)
	}
	return errors.Join(d.syncDir(filepath.Dir(rel)), d.syncDir("."))
}

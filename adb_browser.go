package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

func listDevices(ctx context.Context, binary string, timeout time.Duration) ([]connectedDevice, error) {
	out, err := newADB(binary, "", timeout).run(ctx, "devices", "-l")
	if err != nil {
		return nil, fmt.Errorf("list devices (install Android Platform Tools and enable USB debugging): %w", err)
	}
	devices := make([]connectedDevice, 0)
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "List of devices attached" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || seen[fields[0]] {
			return nil, errors.New("malformed or duplicate device in adb devices output")
		}
		seen[fields[0]] = true
		item := connectedDevice{Serial: fields[0], State: fields[1]}
		for _, field := range fields[2:] {
			if model, ok := strings.CutPrefix(field, "model:"); ok {
				item.Model = strings.ReplaceAll(model, "_", " ")
			}
		}
		devices = append(devices, item)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	return devices, nil
}

func (d *adbDevice) ListDirectories(ctx context.Context, directory string) ([]string, error) {
	if !validADBPath(directory) {
		return nil, errors.New("folder must be a clean absolute device path")
	}
	// The trailing slash follows the Android /sdcard alias. Child symlinks are
	// not followed. Output uses NUL framing so names are not parsed as lines.
	command := "p=" + shellQuote(directory) + `; if [ ! -d "$p" ] || [ ! -r "$p" ] || [ ! -x "$p" ]; then printf '%s\n' 'folder is missing or inaccessible' >&2; exit 1; fi; find "$p"/ -mindepth 1 -maxdepth 1 -type d -print0`
	out, err := d.boundRun(ctx, "shell", "-T", command)
	if err != nil {
		return nil, fmt.Errorf("browse device folders: %w", err)
	}
	folders := make([]string, 0)
	seen := make(map[string]bool)
	for len(out) > 0 {
		nul := bytes.IndexByte(out, 0)
		if nul <= 0 {
			return nil, errors.New("malformed folder listing")
		}
		folder := path.Clean(string(out[:nul]))
		out = out[nul+1:]
		if !validADBPath(folder) || folder == directory || path.Dir(folder) != directory {
			return nil, errors.New("device returned a folder outside the requested directory")
		}
		if !seen[folder] {
			seen[folder] = true
			folders = append(folders, folder)
		}
	}
	sort.Strings(folders)
	return folders, nil
}

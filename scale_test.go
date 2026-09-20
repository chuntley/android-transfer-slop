package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Real destination files exercise the worst-case flat directory. The phone is
// simulated in memory, so this measures engine overhead, not ADB/USB throughput.
func BenchmarkResume50000Files(b *testing.B) {
	const files = 50000
	dest := b.TempDir()
	folder := dest
	if err := os.MkdirAll(folder, 0700); err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 4096)
	seed := filepath.Join(b.TempDir(), "seed")
	if err := os.WriteFile(seed, payload, 0600); err != nil {
		b.Fatal(err)
	}
	dev := &memoryDevice{files: make(map[string][]byte, files), mtime: 1234}
	for i := range files {
		name := fmt.Sprintf("photo-%05d.jpg", i)
		dev.files["/sdcard/DCIM/"+name] = payload
		if err := os.Link(seed, filepath.Join(folder, name)); err != nil {
			b.Fatal(err)
		}
	}
	c := config{dest: dest, sources: sources{"/sdcard/DCIM"}, batchSize: 250, batchBytes: 2 << 30, timeout: time.Minute}
	var last transferProgress
	c.onProgress = func(p transferProgress) { last = p }
	b.ReportAllocs()
	for b.Loop() {
		if err := runWithDevice(context.Background(), c, io.Discard, dev); err != nil {
			b.Fatal(err)
		}
		if last.Verified != files || last.Copied != 0 {
			b.Fatalf("incorrect large-library result: %+v", last)
		}
	}
	b.ReportMetric(files, "files/op")
}

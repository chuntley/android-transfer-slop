package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkLocalHash(b *testing.B) {
	d, err := openDestination(b.TempDir(), true)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	data := make([]byte, 8<<20)
	if err := d.root.WriteFile("photo", data, 0600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.hash(context.Background(), "photo", false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDirectoryNameLookup(b *testing.B) {
	d, err := openDestination(b.TempDir(), true)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	for i := range 10000 {
		if err := os.WriteFile(filepath.Join(d.root.Name(), fmt.Sprintf("photo-%05d.jpg", i)), nil, 0600); err != nil {
			b.Fatal(err)
		}
	}
	if err := d.exactName("photo-09999.jpg"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := d.exactName("photo-09999.jpg"); err != nil {
			b.Fatal(err)
		}
	}
}

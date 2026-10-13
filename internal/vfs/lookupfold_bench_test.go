package vfs_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

// BenchmarkCaseFold compares the SMB server's caseless lookup of a new name in a folder of
// 2,000 files: listing the folder (what every CREATE of a new name did) against
// LookupFold (I-267).
func BenchmarkCaseFold(b *testing.B) {
	f := testharness.NewFabric(b, 3, 2, 1)
	fs := f.FS()
	if err := fs.MkdirAll("/big", 0o755); err != nil {
		b.Fatal(err)
	}
	for i := range 2000 {
		fh, err := fs.OpenFile(fmt.Sprintf("/big/f%05d.jpg", i), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			b.Fatal(err)
		}
		_ = fh.Close()
	}
	b.Run("list", func(b *testing.B) {
		for b.Loop() {
			fis, err := fs.ReadDir("/big")
			if err != nil {
				b.Fatal(err)
			}
			for _, fi := range fis {
				_ = strings.EqualFold(fi.Name(), "NEW.JPG")
			}
		}
	})
	b.Run("lookupfold", func(b *testing.B) {
		for b.Loop() {
			_, _, _ = fs.LookupFold("/big", "NEW.JPG")
		}
	})
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log file rotates before a write would push it past its size, keeps the configured
// number of generations and drops the oldest; Reopen moves new lines into a fresh file
// after an external rename; a nil LogFile accepts every call.
//
// Invariant: I-124
func TestLogFile_RotatesAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	lf, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lf.maxSize, lf.keep = 100, 3
	line := []byte(strings.Repeat("x", 39) + "\n") // 40 bytes: two fit, the third rotates
	size := func(p string) int64 {
		st, err := os.Stat(p)
		if err != nil {
			return -1
		}
		return st.Size()
	}
	for range 3 {
		if _, err := lf.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if size(path) != 40 || size(path+".1") != 80 {
		t.Fatalf("after the first rotation: %d and %d bytes", size(path), size(path+".1"))
	}
	for range 10 { // rotations shift .1 to .2 to .3; a fourth generation never appears
		if _, err := lf.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []string{".1", ".2", ".3"} {
		if size(path+g) != 80 {
			t.Fatalf("generation %s: %d bytes, want 80", g, size(path+g))
		}
	}
	if size(path+".4") != -1 {
		t.Fatal("a fourth generation exists")
	}
	// an external rotation renames the file away: lines keep landing in it until Reopen
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	before := size(path + ".moved")
	if _, err := lf.Write(line); err != nil {
		t.Fatal(err)
	}
	if size(path+".moved") != before+40 || size(path) != -1 {
		t.Fatal("a write after the rename went somewhere else")
	}
	if err := lf.Reopen(); err != nil {
		t.Fatal(err)
	}
	if _, err := lf.Write(line); err != nil {
		t.Fatal(err)
	}
	if size(path) != 40 {
		t.Fatalf("after Reopen: %d bytes at the path, want 40", size(path))
	}
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}
	var nilFile *LogFile
	if nilFile.Close() != nil || nilFile.Reopen() != nil {
		t.Fatal("a nil LogFile must accept Close and Reopen")
	}
}

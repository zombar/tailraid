package sysfstest_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gurgeh/tailraid/internal/sysfs"
	"github.com/gurgeh/tailraid/internal/sysfs/sysfstest"
)

func writeViaFault(t *testing.T, fsys sysfs.FS, dir string, data []byte) (tmpName string, err error) {
	t.Helper()
	f, err := fsys.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		return name, err
	}
	if err := f.Sync(); err != nil {
		return name, err
	}
	if err := f.Close(); err != nil {
		return name, err
	}
	if err := fsys.Rename(name, filepath.Join(dir, "final")); err != nil {
		return name, err
	}
	return name, fsys.SyncDir(dir)
}

func TestFault_PassThroughByDefault(t *testing.T) {
	dir := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	if _, err := writeViaFault(t, fl, dir, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "final"))
	if string(got) != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestFault_FailAtEachStep(t *testing.T) {
	want := errors.New("boom")
	for _, step := range []sysfstest.Step{
		sysfstest.StepCreateTemp, sysfstest.StepWrite, sysfstest.StepSync,
		sysfstest.StepClose, sysfstest.StepRename, sysfstest.StepSyncDir,
	} {
		t.Run(step.String(), func(t *testing.T) {
			dir := t.TempDir()
			fl := sysfstest.New(sysfs.OS{})
			fl.FailAt(step, want)
			_, err := writeViaFault(t, fl, dir, []byte("abc"))
			if !errors.Is(err, want) {
				t.Fatalf("step %v: err = %v, want boom", step, err)
			}
			if step != sysfstest.StepSyncDir {
				if _, err := os.Stat(filepath.Join(dir, "final")); err == nil {
					t.Fatalf("step %v: final file exists after failure", step)
				}
			}
			// a one-shot fault clears itself
			if _, err := writeViaFault(t, fl, dir, []byte("abc")); err != nil {
				t.Fatalf("second attempt after fault should pass: %v", err)
			}
		})
	}
	// Remove fault
	dir := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	p := filepath.Join(dir, "x")
	_ = os.WriteFile(p, nil, 0o644)
	fl.FailAt(sysfstest.StepRemove, want)
	if err := fl.Remove(p); !errors.Is(err, want) {
		t.Fatalf("Remove err = %v", err)
	}
}

func TestFault_WriteErrAfter(t *testing.T) {
	dir := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	fl.WriteErrAfter(3, syscall.ENOSPC)
	f, err := fl.CreateTemp(dir, ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.Write([]byte("abcdef"))
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v, want ENOSPC", err)
	}
	if n != 3 {
		t.Fatalf("short write n = %d, want 3", n)
	}
	_ = f.Close()
	got, _ := os.ReadFile(f.Name())
	if string(got) != "abc" {
		t.Fatalf("file content %q, want the 3 bytes written before ENOSPC", got)
	}
}

func TestFault_CrashAtPanicsAndThenRefusesUntilRestart(t *testing.T) {
	dir := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	fl.CrashAt(sysfstest.StepRename)
	crashed := func() (crashed bool) {
		defer func() {
			if r := recover(); r != nil {
				if !sysfstest.IsCrash(r) {
					panic(r)
				}
				crashed = true
			}
		}()
		_, _ = writeViaFault(t, fl, dir, []byte("abc"))
		return false
	}()
	if !crashed {
		t.Fatal("expected a simulated crash")
	}
	if _, err := os.Stat(filepath.Join(dir, "final")); err == nil {
		t.Fatal("rename happened despite crash before it")
	}
	if _, err := fl.CreateTemp(dir, ".tmp-*"); !errors.Is(err, sysfstest.ErrCrashed) {
		t.Fatalf("after crash, calls must fail with ErrCrashed, got %v", err)
	}
	fl.Restart()
	if _, err := writeViaFault(t, fl, dir, []byte("abc")); err != nil {
		t.Fatalf("after Restart the FS must work: %v", err)
	}
}

func TestFault_TornWriteAt(t *testing.T) {
	dir := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	fl.TornWriteAt(sysfstest.StepSync, 4) // crash at Sync, only 4 bytes reach disk
	func() {
		defer func() {
			if r := recover(); r != nil && !sysfstest.IsCrash(r) {
				panic(r)
			}
		}()
		_, _ = writeViaFault(t, fl, dir, []byte("abcdefgh"))
	}()
	temps, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if len(temps) != 1 {
		t.Fatalf("expected one temp file after crash, got %v", temps)
	}
	got, err := os.ReadFile(temps[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcd" {
		t.Fatalf("torn file = %q, want first 4 bytes", got)
	}
}

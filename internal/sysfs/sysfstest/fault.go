// Package sysfstest provides Fault, a sysfs.FS wrapper with injectable failures used by
// the shard store tests to simulate ENOSPC, torn writes and process crashes at each step
// of a durable write.
package sysfstest

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
)

// Step identifies a point in the temp → write → sync → close → rename → syncdir sequence.
type Step int

const (
	StepCreateTemp Step = iota + 1
	StepWrite
	StepSync
	StepClose
	StepRename
	StepSyncDir
	StepRemove
	StepOpenFile
)

func (s Step) String() string {
	switch s {
	case StepCreateTemp:
		return "CreateTemp"
	case StepWrite:
		return "Write"
	case StepSync:
		return "Sync"
	case StepClose:
		return "Close"
	case StepRename:
		return "Rename"
	case StepSyncDir:
		return "SyncDir"
	case StepRemove:
		return "Remove"
	case StepOpenFile:
		return "OpenFile"
	}
	return fmt.Sprintf("Step(%d)", int(s))
}

// ErrCrashed is returned by every call after a simulated crash until Restart.
var ErrCrashed = errors.New("sysfstest: filesystem crashed (call Restart)")

type crashSentinel struct{ step Step }

// IsCrash reports whether a recovered panic value is a simulated crash from CrashAt.
func IsCrash(r any) bool { _, ok := r.(crashSentinel); return ok }

// Fault wraps an FS. Faults are one-shot: they fire on the first matching call and clear.
type Fault struct {
	inner sysfs.FS

	mu          sync.Mutex
	failStep    Step
	failErr     error
	failSkip    int // matching calls to let through before failStep fires
	crashStep   Step
	tornKeep    int64 // -1 = no torn write
	writeLimit  int64 // -1 = unlimited
	writeErr    error
	crashed     bool
	writtenByFD map[*faultFile]int64
	calls       map[Step]int // every gated call, per step
	blockStep   Step         // calls of this step block until BlockAt's release
	blockIn     chan struct{}
	blockOut    chan struct{}
}

// BlockAt makes every call of the given step block until release is called; entered
// receives one token per call that is waiting. A group of WAL appends is gathered
// behind one blocked fsync this way.
func (f *Fault) BlockAt(step Step) (entered <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, out := make(chan struct{}, 1024), make(chan struct{})
	f.blockStep, f.blockIn, f.blockOut = step, in, out
	var once sync.Once
	return in, func() {
		once.Do(func() {
			f.mu.Lock()
			if f.blockStep == step {
				f.blockStep = 0
			}
			f.mu.Unlock()
			close(out)
		})
	}
}

// Calls is how many times the given step was called (gated calls only).
func (f *Fault) Calls(step Step) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[step]
}

// New wraps inner with no faults armed.
func New(inner sysfs.FS) *Fault {
	return &Fault{inner: inner, tornKeep: -1, writeLimit: -1, writtenByFD: map[*faultFile]int64{}, calls: map[Step]int{}}
}

// FailAt makes the next call of the given step return err.
func (f *Fault) FailAt(step Step, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStep, f.failErr, f.failSkip = step, err, 0
}

// FailAtNth makes the nth call (1-based) of the given step return err; the calls before
// it succeed. A compaction opens two files, and only the second is the reopen.
func (f *Fault) FailAtNth(step Step, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStep, f.failErr, f.failSkip = step, err, max(n-1, 0)
}

// WriteErrAfter makes writes succeed for the first n bytes in total and then return err
// (short write), simulating a full disk.
func (f *Fault) WriteErrAfter(n int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeLimit, f.writeErr = n, err
}

// CrashAt panics with a crash sentinel at the given step, before the operation happens.
// After the crash every call fails with ErrCrashed until Restart.
func (f *Fault) CrashAt(step Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crashStep, f.tornKeep = step, -1
}

// TornWriteAt is CrashAt plus: the bytes written to the file being built are truncated to
// keepBytes, as if the page cache had flushed only a prefix before power loss.
func (f *Fault) TornWriteAt(step Step, keepBytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crashStep, f.tornKeep = step, keepBytes
}

// Restart clears the crashed state (the process "came back").
func (f *Fault) Restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crashed = false
	f.writtenByFD = map[*faultFile]int64{}
}

// check runs the step gate: crashed → ErrCrashed; crash armed → truncate+panic; fail armed
// → err; block armed → wait for the release, then proceed.
func (f *Fault) check(step Step, file *faultFile) error {
	f.mu.Lock()
	f.calls[step]++
	if f.crashed {
		f.mu.Unlock()
		return ErrCrashed
	}
	if f.crashStep == step {
		f.crashStep = 0
		f.crashed = true
		if f.tornKeep >= 0 && file != nil {
			file.truncateTo(f.tornKeep)
		}
		f.tornKeep = -1
		if file != nil {
			_ = file.File.Close() // a dead process holds no handles (Windows cannot remove open files)
		}
		f.mu.Unlock()
		panic(crashSentinel{step})
	}
	if f.failStep == step {
		if f.failSkip > 0 {
			f.failSkip--
			f.mu.Unlock()
			return nil
		}
		f.failStep = 0
		err := f.failErr
		f.failErr = nil
		f.mu.Unlock()
		return err
	}
	block := f.blockStep == step
	in, out := f.blockIn, f.blockOut
	f.mu.Unlock()
	if block {
		in <- struct{}{}
		<-out
	}
	return nil
}

func (f *Fault) CreateTemp(dir, pattern string) (sysfs.File, error) {
	if err := f.check(StepCreateTemp, nil); err != nil {
		return nil, err
	}
	inner, err := f.inner.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: inner, f: f, path: inner.Name()}, nil
}

func (f *Fault) Open(name string) (sysfs.File, error) {
	f.mu.Lock()
	crashed := f.crashed
	f.mu.Unlock()
	if crashed {
		return nil, ErrCrashed
	}
	return f.inner.Open(name)
}

// OpenFile gates StepOpenFile; the returned file gates Write/WriteAt/Sync/Close like a
// temp file so WAL appends can fail or crash at each step.
func (f *Fault) OpenFile(name string, flag int, perm fs.FileMode) (sysfs.File, error) {
	if err := f.check(StepOpenFile, nil); err != nil {
		return nil, err
	}
	inner, err := f.inner.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: inner, f: f, path: name}, nil
}

func (f *Fault) Rename(oldpath, newpath string) error {
	if err := f.check(StepRename, nil); err != nil {
		return err
	}
	return f.inner.Rename(oldpath, newpath)
}

func (f *Fault) Remove(name string) error {
	if err := f.check(StepRemove, nil); err != nil {
		return err
	}
	return f.inner.Remove(name)
}

func (f *Fault) SyncDir(name string) error {
	if err := f.check(StepSyncDir, nil); err != nil {
		return err
	}
	return f.inner.SyncDir(name)
}

func (f *Fault) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.isCrashed() {
		return nil, ErrCrashed
	}
	return f.inner.ReadDir(name)
}

func (f *Fault) MkdirAll(name string, perm fs.FileMode) error {
	if f.isCrashed() {
		return ErrCrashed
	}
	return f.inner.MkdirAll(name, perm)
}

func (f *Fault) Stat(name string) (fs.FileInfo, error) {
	if f.isCrashed() {
		return nil, ErrCrashed
	}
	return f.inner.Stat(name)
}

func (f *Fault) Free(dir string) (uint64, uint64, error) { return f.inner.Free(dir) }

func (f *Fault) Chmod(name string, mode fs.FileMode) error {
	if f.isCrashed() {
		return ErrCrashed
	}
	return f.inner.Chmod(name, mode)
}

func (f *Fault) Chtimes(name string, atime, mtime time.Time) error {
	if f.isCrashed() {
		return ErrCrashed
	}
	return f.inner.Chtimes(name, atime, mtime)
}

func (f *Fault) isCrashed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.crashed
}

// faultFile gates Write/Sync/Close and keeps the bytes written so a torn write can be
// simulated by truncating the underlying file.
type faultFile struct {
	sysfs.File
	f    *Fault
	path string
	buf  []byte
}

func (ff *faultFile) Write(p []byte) (int, error) {
	if err := ff.f.check(StepWrite, ff); err != nil {
		return 0, err
	}
	ff.f.mu.Lock()
	limit, werr := ff.f.writeLimit, ff.f.writeErr
	written := ff.f.writtenByFD[ff]
	ff.f.mu.Unlock()
	n := len(p)
	var short error
	if limit >= 0 && written+int64(n) > limit {
		n = int(max(limit-written, 0))
		short = werr
	}
	wrote, err := ff.File.Write(p[:n])
	ff.buf = append(ff.buf, p[:wrote]...)
	ff.f.mu.Lock()
	ff.f.writtenByFD[ff] = written + int64(wrote)
	ff.f.mu.Unlock()
	if err != nil {
		return wrote, err
	}
	return wrote, short
}

// WriteAt is gated like Write (same step, same short-write limit).
func (ff *faultFile) WriteAt(p []byte, off int64) (int, error) {
	if err := ff.f.check(StepWrite, ff); err != nil {
		return 0, err
	}
	ff.f.mu.Lock()
	limit, werr := ff.f.writeLimit, ff.f.writeErr
	written := ff.f.writtenByFD[ff]
	ff.f.mu.Unlock()
	n := len(p)
	var short error
	if limit >= 0 && written+int64(n) > limit {
		n = int(max(limit-written, 0))
		short = werr
	}
	wrote, err := ff.File.WriteAt(p[:n], off)
	ff.f.mu.Lock()
	ff.f.writtenByFD[ff] = written + int64(wrote)
	ff.f.mu.Unlock()
	if err != nil {
		return wrote, err
	}
	return wrote, short
}

func (ff *faultFile) Sync() error {
	if err := ff.f.check(StepSync, ff); err != nil {
		return err
	}
	return ff.File.Sync()
}

func (ff *faultFile) Close() error {
	if err := ff.f.check(StepClose, ff); err != nil {
		_ = ff.File.Close() // a failed close still releases the descriptor
		return err
	}
	return ff.File.Close()
}

// truncateTo is called with f.mu held, during a simulated crash.
func (ff *faultFile) truncateTo(keep int64) {
	_ = ff.File.Close()
	if t, ok := ff.f.inner.(interface{ Truncate(string, int64) error }); ok {
		_ = t.Truncate(ff.path, keep)
		return
	}
	_ = truncateFile(ff.path, keep)
}

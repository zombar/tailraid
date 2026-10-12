package smbgw_test

import (
	"os"
	"testing"
	"time"
)

// Explorer and robocopy write a file, then set the source's times (SET_INFO basic) as
// their last step; the flush that commits the data seconds later keeps them, so the copy
// shows the source's "Date modified" and the next robocopy run finds nothing to copy.
// Invariant: I-230
func TestSMB_CopyTimes_SurviveFlush(t *testing.T) {
	g := startGateway(t)
	fh, err := g.sh.OpenFile("song.mp3", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	must(t, err)
	_, err = fh.WriteAt([]byte("frames"), 4096)
	must(t, err)
	must(t, fh.Close())
	mtime := time.Date(2024, 1, 18, 20, 34, 0, 0, time.UTC)
	must(t, g.sh.Chtimes("song.mp3", mtime, mtime))
	must(t, g.f.Flusher().FlushAll(ctx))
	fi, err := g.sh.Stat("song.mp3")
	must(t, err)
	if got := fi.ModTime().UnixNano() / 100; got != mtime.UnixNano()/100 {
		t.Fatalf("ModTime after the flush %v, want the copy's %v", fi.ModTime(), mtime)
	}
	if fi.Size() != 4096+6 {
		t.Fatalf("size %d", fi.Size())
	}
}

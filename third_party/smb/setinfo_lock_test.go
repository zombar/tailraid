package smb

import (
	"encoding/binary"
	"testing"
	"time"
)

// gatedTrunc is a positional handle whose Truncate waits for the test, as a driver's
// truncate waits at a cap.
type gatedTrunc struct {
	gatedFile
	tgate    chan struct{}
	tentered chan struct{}
}

func (g *gatedTrunc) Truncate(int64) error {
	g.tentered <- struct{}{}
	<-g.tgate
	return nil
}

// concurrentWriterFS is tinyFS declaring its positional writes safe beside its other calls.
type concurrentWriterFS struct{ *tinyFS }

func (concurrentWriterFS) ConcurrentWrites() bool { return true }

// A SET_INFO that changes a file's end of file through its handle, its times, or a FLUSH,
// takes no share lock on a driver that declares concurrent writes: a truncate waiting at
// a cap does not hold up every CREATE, CLOSE and rename on the share. A rename or a
// delete-on-close still takes it, and so does any of them on a driver that promises
// nothing.
// Invariant: I-268
func TestSetInfo_EndOfFile_TakesNoShareLockOnAConcurrentDriver(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent bool
	}{{"concurrent driver", true}, {"serial driver", false}} {
		t.Run(tc.name, func(t *testing.T) {
			tiny := &tinyFS{body: []byte("hello")}
			var c *conn
			var of *openFile
			if tc.concurrent {
				c, of = opened(t, concurrentWriterFS{tiny}, "/file.txt", false)
			} else {
				c, of = opened(t, tiny, "/file.txt", false)
			}
			g := &gatedTrunc{tgate: make(chan struct{}), tentered: make(chan struct{}, 1)}
			of.f, of.w = g, g
			body := make([]byte, 32+8)
			body[2], body[3] = infoTypeFile, fileEndOfFileInformation
			binary.LittleEndian.PutUint32(body[4:], 8)
			binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
			copy(body[16:], of.id[:])
			binary.LittleEndian.PutUint64(body[32:], 3)
			done := make(chan []byte, 1)
			go func() { done <- mustDispatch(t, c, cmdSetInfo, body) }()
			<-g.tentered // the truncate is in the driver
			locked := make(chan struct{})
			go func() { of.share.changing()(); close(locked) }()
			select {
			case <-locked:
				if !tc.concurrent {
					t.Fatal("a change on the share ran under a truncate of a driver that promised nothing")
				}
			case <-time.After(200 * time.Millisecond):
				if tc.concurrent {
					t.Fatal("a change on the share waited for a truncate in a concurrent driver")
				}
			}
			close(g.tgate)
			if st := statusOf(t, <-done); st != statusSuccess {
				t.Fatalf("SET_INFO answered %#x", st)
			}
			<-locked
		})
	}
}

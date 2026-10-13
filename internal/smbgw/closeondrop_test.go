package smbgw_test

import (
	"errors"
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// A handle marked delete-on-close whose CLOSE never comes is closed by the server when
// its connection drops, its session logs off or its tree disconnects, and the delete
// happens then (MS-SMB2 3.3.7.1, 3.3.5.6, 3.3.5.8): Explorer marks every file it is
// writing as a guard, and a connection lost mid-copy had left each one behind, full
// size and made of holes.
// Invariant: I-240
func TestSMB_DeleteOnClose_RunsOnDropLogoffTreeDisconnect(t *testing.T) {
	for _, how := range []string{"connection dropped", "logoff", "tree disconnect"} {
		t.Run(how, func(t *testing.T) {
			g := startGateway(t)
			raw, err := net.Dial("tcp", g.addr)
			must(t, err)
			d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: smbgw.User, Password: password}}
			sess, err := d.DialConn(ctx, raw, g.addr)
			must(t, err)
			sh, err := sess.Mount(smbgw.ShareName)
			must(t, err)
			fh, err := sh.OpenFile("copy.mp3", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			must(t, err)
			_, err = fh.WriteAt([]byte("half a copy"), 4096)
			must(t, err)
			must(t, fh.SetDeleteOnClose())
			switch how {
			case "connection dropped":
				must(t, raw.Close())
			case "logoff":
				must(t, sess.Logoff())
			case "tree disconnect":
				must(t, sh.Umount())
			}
			testharness.Eventually(t, func() bool {
				_, err := g.fs.Stat("/copy.mp3")
				return errors.Is(err, os.ErrNotExist)
			}, "the guarded file outlived its "+how)
			runtime.SetFinalizer(fh, nil) // its CLOSE would wait forever on the gone connection
			if how != "connection dropped" {
				_ = raw.Close()
			}
		})
	}
}

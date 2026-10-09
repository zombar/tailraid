package smbgw

import (
	"context"
	"errors"
	"fmt"
	"net"

	smb "github.com/go-filesystems/smb"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// NewServer builds the gateway's SMB server: one share, one user, the share's size from
// meta.Usage (docs/15 §4). password is the config's smb.password; the library refuses
// guest sessions, so an empty one would serve nobody.
func NewServer(fs *vfs.FS, st *meta.Store, password string) (*smb.Server, error) {
	if password == "" {
		return nil, errors.New("smbgw: smb.password is required (init and join generate it)")
	}
	a := New(fs, st)
	srv := smb.New()
	srv.SetName(ShareName)
	srv.AddUser(User, password)
	if err := srv.Share(ShareName, a, smb.WithCapacityFunc(a.Capacity)); err != nil {
		return nil, fmt.Errorf("smbgw: share: %w", err)
	}
	return srv, nil
}

// Listen binds the gateway listener. addr must be a loopback address (docs/08, I-110).
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("smbgw: listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("smbgw: refusing to listen on non-loopback address %q", addr)
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// Serve runs srv on l until ctx is done, then closes the listener and every connection
// it accepted (the library serves each on its own goroutine and would otherwise keep
// answering a mounted client after the gateway's flusher stopped).
func Serve(ctx context.Context, l net.Listener, srv *smb.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		_ = srv.Close()
		<-errc
		return nil
	case err := <-errc:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

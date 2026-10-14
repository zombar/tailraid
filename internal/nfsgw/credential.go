package nfsgw

import (
	"encoding/binary"
	"sync"

	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/users"
	"github.com/zombar/tailraid/internal/vfs"
)

// The actor of an NFS call (docs/21 §5, I-350, I-351): the AUTH_SYS uid the call
// carries, mapped through the device's bindings to a user, served through that user's
// view of the filesystem. An unbound uid is nobody.

// Bindings maps a device's credentials to user names (users.Bindings).
type Bindings interface {
	Lookup(cred string) (string, bool)
}

// views keeps one filesystem view per user, so two handles of one request (RENAME's
// source and target) compare equal and a listing's cache is keyed by its reader.
type views struct {
	mu   sync.Mutex
	by   map[meta.UserIdx]*vfs.FS
	base *vfs.FS
}

func (v *views) for_(actor meta.UserIdx) *vfs.FS {
	v.mu.Lock()
	defer v.mu.Unlock()
	if f, ok := v.by[actor]; ok {
		return f
	}
	if v.by == nil {
		v.by = map[meta.UserIdx]*vfs.FS{}
	}
	f := v.base.As(actor)
	v.by[actor] = f
	return f
}

// WithCredential is the handler of one request (nfs.CredentialHandler): once the cluster
// has users, the view of the user the call's AUTH_SYS uid is bound to, or nobody's.
// Before the first user, and on a gateway with no bindings, every caller is served the
// node's filesystem, as before.
func (h *Handler) WithCredential(cred rpc.Auth) nfs.Handler {
	if h.bind == nil || !h.st.Enforcing() {
		return h
	}
	var actor meta.UserIdx
	if uid, ok := authSysUID(cred); ok {
		if name, bound := h.bind.Lookup(users.UIDCredential(int(uid))); bound {
			if u, found := h.st.UserByName(name); found {
				actor = u.Idx
			}
		}
	}
	cp := *h
	cp.fs = h.views.for_(actor)
	return &cp
}

// authSysUID reads the uid of an AUTH_SYS (AUTH_UNIX) credential: XDR stamp, machine
// name, uid, gid, gids (RFC 5531 §A.3.1). Any other flavor, or a short body, is no uid.
func authSysUID(cred rpc.Auth) (uint32, bool) {
	if cred.Flavor != uint32(nfs.AuthFlavorUnix) {
		return 0, false
	}
	b := cred.Body
	if len(b) < 8 {
		return 0, false
	}
	n := binary.BigEndian.Uint32(b[4:8]) // the machine name's length
	off := 8 + int((n+3)&^3)             // padded to four bytes
	if n > 255 || len(b) < off+8 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[off : off+4]), true
}

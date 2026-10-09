// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Share types and flags a TREE_CONNECT response carries.
const (
	shareTypeDisk uint8 = 0x01
	shareTypePipe uint8 = 0x02

	shareFlagManualCaching uint32 = 0x00000000

	// The access mask a client is granted on the share itself. A read-only
	// share is refused the write bits HERE, so a file manager greys out the
	// actions rather than offering them and failing later.
	accessRead  uint32 = 0x00120089 // read data | read ea | read attributes | read control | synchronize
	accessWrite uint32 = 0x00120116 // write data | append | write ea | write attributes
	accessAll   uint32 = 0x001F01FF
)

// treeConnect answers \\server\share by name.
//
// The path arrives as UTF-16, absolute and with the host in it. Only the last
// element is the share, and the comparison is caseless -- \\HOST\Disk and
// \\host\disk are the same share, and a client will use whichever the person
// typed.
func (c *conn) treeConnect(h header, body []byte, msg []byte) ([]byte, error) {
	if c.session(h) == nil {
		return errorResponse(h, statusUserSessionDeleted), nil
	}
	if len(body) < 8 {
		return nil, fmt.Errorf("smb: TREE_CONNECT body of %d bytes is too short", len(body))
	}
	off := int(binary.LittleEndian.Uint16(body[4:]))
	n := int(binary.LittleEndian.Uint16(body[6:]))
	if off < 0 || n < 0 || off+n > len(msg) {
		return nil, fmt.Errorf("smb: the share path is not inside the message")
	}
	path := fromUTF16le(msg[off : off+n])
	name := path
	if i := strings.LastIndex(path, `\`); i >= 0 {
		name = path[i+1:]
	}
	// IPC$ is not a share anybody exported: it is the pipe share every SMB
	// server has, and the Linux kernel's client TREE_CONNECTs to it before it
	// will list anything, to ask for a DFS referral. Refusing it costs the
	// whole mount -- "BAD_NETWORK_NAME: \\host\IPC$" in the kernel log, then
	// "failed to connect to IPC", then EIO on the first readdir.
	//
	// Connecting is all that is needed. There are no pipes behind it: the
	// referral request that follows is answered by name, and the client
	// carries on with the share it actually wanted.
	if strings.EqualFold(name, "IPC$") {
		c.nextTree++
		tid := c.nextTree
		c.trees[tid] = &treeConn{sh: &share{name: "IPC$", ipc: true, ro: true}, ro: true}
		h.treeID = tid
		// FULL access, not read: a remote procedure call is WRITTEN to the
		// pipe. `smbutil view` reads the mask on the tree and gives up before
		// it opens anything -- it connected to IPC$, disconnected, and printed
		// "unable to list resources: Broken pipe" -- which looks like a broken
		// pipe implementation and was a permission it was told it did not
		// have. The read-only flag on the tree stays: it is about a
		// filesystem, and IPC$ has none.
		return treeConnectResponse(h, shareTypePipe, accessAll), nil
	}
	sh := c.srv.shareByName(name)
	if sh == nil {
		// BAD_NETWORK_NAME is what a client turns into "the share does not
		// exist" rather than "the server is broken".
		return errorResponse(h, statusBadNetworkName), nil
	}
	// Who is asking decides whether they may, and for what. A user who is not
	// on the list is refused here rather than at the first file: ACCESS_DENIED
	// on the connection is what a client shows as "you do not have permission
	// to use this share".
	user := c.session(h).user
	if !sh.mayConnect(user) {
		return errorResponse(h, statusAccessDenied), nil
	}
	ro := sh.readOnlyFor(user)
	c.nextTree++
	tid := c.nextTree
	c.trees[tid] = &treeConn{sh: sh, ro: ro}

	access := accessAll
	if ro {
		access = accessRead
	}
	h.treeID = tid
	return treeConnectResponse(h, shareTypeDisk, access), nil
}

func treeConnectResponse(h header, kind uint8, access uint32) []byte {
	b := append(responseTo(h, statusSuccess), make([]byte, 16)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 16)
	rb[2] = kind
	binary.LittleEndian.PutUint32(rb[4:], shareFlagManualCaching)
	binary.LittleEndian.PutUint32(rb[8:], 0) // capabilities: none of the cluster ones
	binary.LittleEndian.PutUint32(rb[12:], access)
	return b
}

// tree returns the share a message is addressed to, as connected.
func (c *conn) tree(h header) *treeConn {
	return c.trees[h.treeID]
}

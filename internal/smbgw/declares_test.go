package smbgw

import (
	"testing"

	smb "github.com/go-filesystems/smb"
)

// The adapter declares its positional reads and writes safe alongside its other calls, so
// the server runs neither a READ nor a WRITE under the share lock.
// Invariant: I-217, I-254
func TestFS_DeclaresConcurrentReadsAndWrites(t *testing.T) {
	var a any = &FS{}
	r, ok := a.(smb.ConcurrentReader)
	if !ok || !r.ConcurrentReads() {
		t.Fatal("the adapter does not declare concurrent reads")
	}
	w, ok := a.(smb.ConcurrentWriter)
	if !ok || !w.ConcurrentWrites() {
		t.Fatal("the adapter does not declare concurrent writes")
	}
}

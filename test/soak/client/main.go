// Command soak-client drives a tailraid NFS gateway over the pure-Go NFSv3 client, for
// environments without a kernel NFSv3 client (Docker Desktop). Modes:
//
//	write  <gateway addr> <shared dir>   create a tree, random-offset edits, truncate,
//	                                     rename churn; write a sha256 manifest
//	verify <gateway addr> <shared dir>   read everything back and compare to the manifest
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: soak-client write|verify <gateway host:port> <shared dir>")
		os.Exit(2)
	}
	mode, addr, shared := os.Args[1], os.Args[2], os.Args[3]
	tgt, closeFn, err := connect(addr)
	if err != nil {
		fatal("connect %s: %v", addr, err)
	}
	defer closeFn()
	switch mode {
	case "write":
		write(tgt, shared)
	case "verify":
		verify(tgt, shared)
	default:
		fatal("unknown mode %q", mode)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "soak-client: "+format+"\n", args...)
	os.Exit(1)
}

func connect(addr string) (*nfsc.Target, func(), error) {
	var lastErr error
	for attempt := 0; attempt < 60; attempt++ {
		cl, err := rpc.DialTCP("tcp", addr, false)
		if err != nil {
			lastErr = err
			<-time.After(time.Second)
			continue
		}
		type mountArgs struct {
			rpc.Header
			Dirpath string
		}
		res, err := cl.Call(&mountArgs{Header: rpc.Header{Rpcvers: 2, Prog: nfsc.MountProg, Vers: nfsc.MountVers, Proc: nfsc.MountProc3MNT, Cred: rpc.AuthNull, Verf: rpc.AuthNull}, Dirpath: "/"})
		if err != nil {
			cl.Close()
			return nil, nil, err
		}
		if status, _ := xdr.ReadUint32(res); status != nfsc.MNT3Ok {
			cl.Close()
			return nil, nil, fmt.Errorf("mount status %d", status)
		}
		fh, err := xdr.ReadOpaque(res)
		if err != nil {
			cl.Close()
			return nil, nil, err
		}
		tgt, err := nfsc.NewTargetWithClient(cl, rpc.NewAuthUnix("soak", 0, 0).Auth(), fh, "/", 0)
		if err != nil {
			cl.Close()
			return nil, nil, err
		}
		return tgt, cl.Close, nil
	}
	return nil, nil, lastErr
}

// --- NFS helpers ---------------------------------------------------------------------

func writeAt(t *nfsc.Target, p string, off int64, data []byte) {
	f, err := t.OpenFile(p, 0o644)
	if err != nil {
		fatal("open %s: %v", p, err)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		fatal("seek: %v", err)
	}
	for len(data) > 0 { // the client library writes in one RPC; keep pieces at 64 KiB
		n := min(len(data), 64<<10)
		if _, err := f.Write(data[:n]); err != nil {
			fatal("write %s@%d: %v", p, off, err)
		}
		data = data[n:]
		off += int64(n)
	}
	_ = f.Close()
}

func putFile(t *nfsc.Target, p string, data []byte) {
	if _, err := t.Create(p, 0o644); err != nil {
		fatal("create %s: %v", p, err)
	}
	writeAt(t, p, 0, data)
}

func readAll(t *nfsc.Target, p string) []byte {
	fi, _, err := t.Lookup(p)
	if err != nil {
		fatal("lookup %s: %v", p, err)
	}
	f, err := t.Open(p)
	if err != nil {
		fatal("open %s: %v", p, err)
	}
	defer f.Close()
	out := make([]byte, fi.Size())
	var got int64
	for got < int64(len(out)) {
		n, err := f.ReadAt(out[got:min(got+(64<<10), int64(len(out)))], got)
		if n == 0 && err != nil {
			fatal("read %s@%d: %v", p, got, err)
		}
		got += int64(n)
	}
	return out
}

func truncate(t *nfsc.Target, p string, size uint64) {
	if err := t.Setattr(p, nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: size}}); err != nil {
		fatal("truncate %s: %v", p, err)
	}
}

func rename(t *nfsc.Target, from, to string) {
	fromDir, fromName := path.Split(from)
	toDir, toName := path.Split(to)
	_, ffh, err := t.Lookup(fromDir)
	if err != nil {
		fatal("lookup %s: %v", fromDir, err)
	}
	_, tfh, err := t.Lookup(toDir)
	if err != nil {
		fatal("lookup %s: %v", toDir, err)
	}
	type renameArgs struct {
		rpc.Header
		From nfsc.Diropargs3
		To   nfsc.Diropargs3
	}
	res, err := t.Call(&renameArgs{
		Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Rename, Cred: rpc.NewAuthUnix("soak", 0, 0).Auth(), Verf: rpc.AuthNull},
		From:   nfsc.Diropargs3{FH: ffh, Filename: fromName},
		To:     nfsc.Diropargs3{FH: tfh, Filename: toName},
	})
	if err != nil {
		fatal("rename: %v", err)
	}
	status, _ := xdr.ReadUint32(res)
	if err := nfsc.NFS3Error(status); err != nil {
		fatal("rename %s → %s: %v", from, to, err)
	}
}

func mkdirAll(t *nfsc.Target, p string) {
	cur := "/"
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if _, _, err := t.Lookup(cur); err != nil {
			if _, err := t.Mkdir(cur, 0o755); err != nil {
				fatal("mkdir %s: %v", cur, err)
			}
		}
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// --- workload --------------------------------------------------------------------------

func write(t *nfsc.Target, shared string) {
	files := map[string][]byte{}
	mkdirAll(t, "/tree/a/b")
	mkdirAll(t, "/tree/c")
	files["/tree/a/big.bin"] = randomBytes(3_000_000)
	files["/tree/a/b/small.bin"] = randomBytes(777)
	for i := 1; i <= 150; i++ {
		files[fmt.Sprintf("/tree/c/f%d.txt", i)] = []byte(fmt.Sprintf("file %d\n", i))
	}
	for p, data := range files {
		putFile(t, p, data)
	}
	// random-access edits mirrored on a local reference
	ref := randomBytes(4_194_304)
	putFile(t, "/rw.bin", ref)
	patch := randomBytes(70_000)
	writeAt(t, "/rw.bin", 1_500_000, patch)
	copy(ref[1_500_000:], patch)
	truncate(t, "/rw.bin", 3_000_000)
	ref = ref[:3_000_000]
	if got := readAll(t, "/rw.bin"); !bytes.Equal(got, ref) {
		fatal("rw.bin differs on the writing gateway at %d", firstDiff(got, ref))
	}
	files["/rw.bin"] = ref
	// rename churn
	for i := 1; i <= 30; i++ {
		putFile(t, "/tmp.new", []byte(fmt.Sprintf("v%d\n", i)))
		rename(t, "/tmp.new", "/versioned.txt")
	}
	files["/versioned.txt"] = []byte("v30\n")
	// manifest
	lines := make([]string, 0, len(files))
	for p, data := range files {
		lines = append(lines, fmt.Sprintf("%x  %s", sha256.Sum256(data), p))
	}
	sort.Strings(lines)
	if err := os.WriteFile(path.Join(shared, "manifest.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		fatal("manifest: %v", err)
	}
	fmt.Printf("write: ok, %d files written through %s; waiting for the idle flush\n", len(files), "n1")
	<-time.After(5 * time.Second) // real idle timer of the running nodes
}

func verify(t *nfsc.Target, shared string) {
	manifest, err := os.ReadFile(path.Join(shared, "manifest.txt"))
	if err != nil {
		fatal("manifest: %v", err)
	}
	failed := 0
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			continue
		}
		n++
		got := fmt.Sprintf("%x", sha256.Sum256(readAll(t, parts[1])))
		if got != parts[0] {
			fmt.Printf("MISMATCH %s\n", parts[1])
			failed++
		}
	}
	if failed > 0 {
		fatal("%d of %d files differ through the second gateway", failed, n)
	}
	fmt.Printf("verify: ok — all %d files read identically through the second gateway\n", n)
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

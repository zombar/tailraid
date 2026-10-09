//go:build livemount

package smb_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	fssmb "github.com/go-filesystems/smb"
)

// The judge that is not a library: the Linux kernel's own SMB client, the one
// behind `mount -t cifs`. A foreign Go client runs in every other lane; this
// one is an operating system, and it asks for things no library bothers with
// -- chained requests, credits, the information classes a stat needs.
//
// It is behind a build tag because it needs root to mount and a kernel module
// that a macOS or Windows runner does not have. CI runs it on Linux.
//
// vers=2.1 is not optional: mount.cifs defaults to 3.1.1, and this server
// speaks 2.1. Saying so here is better than a lane that fails with "the
// server does not support the requested dialect" and reads as a bug.
func TestLiveMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mount -t cifs is Linux's")
	}
	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("sudo"); err != nil {
			t.Skip("mounting needs root, and there is no sudo here")
		}
	}

	mem := newMemFS(true)
	body := bytes.Repeat([]byte("go-filesystems/smb "), 3000) // 57 kB: several reads
	if err := mem.WriteFile("/greeting.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mem.MkDir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/sub/inner.txt", []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := fssmb.New()
	srv.AddUser("alice", "hunter2")
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	mnt := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	// No dialect is named: the kernel offers what it has and this server picks
	// the best it speaks, which is what a person typing `mount -t cifs` gets.
	// If that fails, 3.0 is asked for explicitly -- and the test says which
	// one worked, because "it mounts when told exactly what to do" is a
	// weaker result than "it mounts".
	base := fmt.Sprintf("port=%d,username=alice,password=hunter2,uid=%d,gid=%d",
		port, os.Getuid(), os.Getgid())
	out, err := run("mount", "-t", "cifs", "//127.0.0.1/disk", mnt, "-o", base)
	if err != nil {
		t.Logf("mounting without naming a dialect failed, trying 3.0: %v\n%s", err, out)
		if out, err = run("mount", "-t", "cifs", "//127.0.0.1/disk", mnt, "-o", base+",vers=3.0"); err != nil {
			fatal(t, "mounting with %q: %v\n%s", base, err, out)
		}
	}
	if out, err := run("sh", "-c", "mount | grep cifs"); err == nil {
		t.Logf("mounted: %s", strings.TrimSpace(out))
	}
	defer run("umount", mnt)

	// Everything a person does with a mount.
	entries, err := os.ReadDir(mnt)
	if err != nil {
		fatal(t, "listing the mount: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "greeting.txt,sub" {
		t.Errorf("the mount lists %q", got)
	}

	got, err := os.ReadFile(filepath.Join(mnt, "greeting.txt"))
	if err != nil {
		fatal(t, "reading through the mount: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read %d bytes of %d, and they differ", len(got), len(body))
	}
	st, err := os.Stat(filepath.Join(mnt, "greeting.txt"))
	if err != nil || st.Size() != int64(len(body)) {
		t.Errorf("stat says %v bytes (%v), want %d", st, err, len(body))
	}
	if inner, err := os.ReadFile(filepath.Join(mnt, "sub", "inner.txt")); err != nil || string(inner) != "inside" {
		t.Errorf("the file in the subdirectory read %q, %v", inner, err)
	}

	// …and the other direction, checked in the filesystem underneath rather
	// than through the mount that wrote it.
	written := []byte("written by the kernel client")
	if err := os.WriteFile(filepath.Join(mnt, "fromclient.txt"), written, 0o644); err != nil {
		fatal(t, "writing through the mount: %v", err)
	}
	if inDriver, err := mem.ReadFile("/fromclient.txt"); err != nil || !bytes.Equal(inDriver, written) {
		t.Errorf("the filesystem holds %q (%v), not what the client wrote", inDriver, err)
	}
}

// fatal fails with the kernel's own account of what went wrong. The client
// here is a kernel module: it reports EIO or "Invalid argument" to userspace
// and writes the actual reason to the log, so a lane without this says only
// that something is wrong.
func fatal(t *testing.T, format string, args ...any) {
	t.Helper()
	kernel, _ := run("dmesg", "--ctime")
	lines := strings.Split(strings.TrimSpace(kernel), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	t.Fatalf(format+"\n--- the kernel's own account ---\n%s", append(args, strings.Join(lines, "\n"))...)
}

// run executes a command as root, through sudo when it has to.
func run(name string, args ...string) (string, error) {
	if os.Geteuid() != 0 {
		args = append([]string{name}, args...)
		name = "sudo"
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// The other half of what a person does with a server: find out what is ON it.
//
// The judge here is Samba's own client -- the reference implementation of the
// client side of srvsvc. `smbclient -L` binds to \srvsvc over SMB2 and calls
// NetrShareEnum, which is three formats deep (IOCTL, DCE/RPC, NDR) and is
// where a server that gets one layer wrong hands back something that decodes
// into nonsense rather than nothing.
//
// It also proves the filtering end to end: Bob is not allowed on one of the
// shares and must not be shown it.
//
// macOS cannot be the judge for this one. `smbutil view` binds with an
// ncacn_np string -- `ncacn_np:HOST[\pipe\srvsvc]` -- which has NO port
// field, so it dials 445 whatever the URL said. Against a server on a high
// port it logs "RPC to srvsrvc gave error 0x16c9a034", falls back to the SMB1
// RAP call, and prints "unable to list resources: Broken pipe". The proxy
// witness shows the tree connect to IPC$ and then nothing: the pipe is never
// opened. A privileged port would be needed to test it, which is a person's
// decision and not a lane's.
func TestLiveShareEnumeration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("smbclient is the judge here and the lane that has it is Linux")
	}
	if _, err := exec.LookPath("smbclient"); err != nil {
		t.Skip("smbclient is not installed")
	}

	srv := fssmb.New()
	srv.AddUser("alice", "hunter2")
	srv.AddUser("bob", "swordfish")
	for _, share := range []struct {
		name string
		opts []fssmb.ShareOption
	}{
		{"attic", nil},
		{"photos", nil},
		{"alices", []fssmb.ShareOption{fssmb.AllowUsers("alice")}},
	} {
		if err := srv.Share(share.name, newMemFS(true), share.opts...); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	// The credentials go in a FILE, not on the command line: an argument is
	// visible in the process list to every user on the machine.
	list := func(user, password string) string {
		t.Helper()
		creds := filepath.Join(dir, user+".creds")
		body := fmt.Sprintf("username = %s\npassword = %s\n", user, password)
		if err := os.WriteFile(creds, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("smbclient", "-L", "//127.0.0.1",
			"-p", fmt.Sprint(port), "-A", creds,
			"--option=client min protocol=SMB2").CombinedOutput()
		t.Logf("smbclient -L as %s (err %v):\n%s", user, err, out)
		if err != nil {
			t.Fatalf("smbclient could not list the shares as %s: %v", user, err)
		}
		return string(out)
	}

	// Samba prints one share per line, name first after the indent.
	named := func(out, name string) bool {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == name {
				return true
			}
		}
		return false
	}

	alice := list("alice", "hunter2")
	for _, name := range []string{"attic", "photos", "alices", "IPC$"} {
		if !named(alice, name) {
			t.Errorf("alice was not shown %q", name)
		}
	}

	bob := list("bob", "swordfish")
	for _, name := range []string{"attic", "photos", "IPC$"} {
		if !named(bob, name) {
			t.Errorf("bob was not shown %q", name)
		}
	}
	if named(bob, "alices") {
		t.Error("bob was shown a share he may not connect to")
	}
}

package mount

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCommand_PerOS(t *testing.T) {
	cases := []struct {
		goos, want string
	}{
		{"darwin", "mount -t nfs -o port=20490,mountport=20490,vers=3,tcp,nolocks,soft 127.0.0.1:/ /Users/me/TailRAID"},
		{"linux", "mount -t nfs -o port=20490,mountport=20490,nfsvers=3,tcp,nolock,noacl,soft 127.0.0.1:/ /Users/me/TailRAID"},
	}
	for _, tc := range cases {
		prog, args, err := Command(tc.goos, "127.0.0.1:20490", "/Users/me/TailRAID")
		if err != nil {
			t.Fatal(err)
		}
		if got := prog + " " + strings.Join(args, " "); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.goos, got, tc.want)
		}
	}
	// Windows has no NFS mount any more (docs/15)
	if _, _, err := Command("windows", "127.0.0.1:2049", "T:"); err == nil || !strings.Contains(err.Error(), "SMB") {
		t.Fatalf("windows NFS mount: %v", err)
	}
	if _, _, err := Command("plan9", "127.0.0.1:1", "/x"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	if _, _, err := Command("linux", "nonsense", "/x"); err == nil {
		t.Fatal("bad address accepted")
	}
}

// The Windows mount names the port (24H2's -TcpPort), the user, and takes the password
// from the environment variable: it is never an argument (docs/15 §6), and it does not
// go through `net use … *`, which reads a password from the console only.
// Invariant: I-110
func TestSMBCommand_PerOS(t *testing.T) {
	cases := []struct {
		goos, want string
	}{
		{"windows", `powershell -NoProfile -NonInteractive -Command New-SmbMapping -LocalPath T: -RemotePath \\127.0.0.1\tailraid -TcpPort 20445 -UserName tailraid -Password $env:TAILRAID_SMB_PASSWORD -Persistent $false`},
		{"darwin", "mount_smbfs //tailraid@127.0.0.1:20445/tailraid /Users/me/TailRAID"},
		{"linux", "mount -t cifs //127.0.0.1/tailraid /Users/me/TailRAID -o port=20445,username=tailraid,vers=3.0"},
	}
	for _, tc := range cases {
		target := "/Users/me/TailRAID"
		if tc.goos == "windows" {
			target = "T:"
		}
		prog, args, err := SMBCommand(tc.goos, "127.0.0.1:20445", target)
		if err != nil {
			t.Fatal(err)
		}
		got := prog + " " + strings.Join(args, " ")
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.goos, got, tc.want)
		}
		if strings.Contains(strings.ReplaceAll(got, "-Password $env:"+SMBPasswordEnv, ""), "assword") {
			t.Errorf("%s: a password on the command line: %s", tc.goos, got)
		}
	}
	if _, _, err := SMBCommand("plan9", "127.0.0.1:1", "/x"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	if _, _, err := SMBCommand("windows", "nonsense", "T:"); err == nil {
		t.Fatal("bad address accepted")
	}
}

func TestUnmountCommand_PerOS(t *testing.T) {
	cases := map[string]string{
		"darwin":  "umount /x",
		"linux":   "umount /x",
		"windows": "net use /x /delete /y",
	}
	for goos, want := range cases {
		prog, args, err := UnmountCommand(goos, "/x")
		if err != nil {
			t.Fatal(err)
		}
		if got := prog + " " + strings.Join(args, " "); got != want {
			t.Errorf("%s: got %s, want %s", goos, got, want)
		}
	}
	if _, _, err := UnmountCommand("plan9", "/x"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
}

// Windows 11 24H2 (build 26100) is the floor: older builds cannot name the SMB port.
func TestCheckWindowsBuild_Table(t *testing.T) {
	for build, ok := range map[uint32]bool{0: true, 22631: false, 26099: false, 26100: true, 26200: true} {
		err := CheckWindowsBuild(build)
		if (err == nil) != ok {
			t.Errorf("build %d: %v, want ok=%v", build, err, ok)
		}
	}
}

// mount --wait maps only once the gateway accepts a connection (the service may start
// after the logon task), and gives up after the bound with the address in the error.
// Invariant: I-142
func TestMount_WaitGateway(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := l.Addr().String()
	if err := WaitGateway(t.Context(), open, time.Second); err != nil {
		t.Errorf("a listening gateway: %v", err)
	}
	if err := WaitGateway(t.Context(), open, 0); err != nil {
		t.Errorf("a listening gateway with no wait: %v", err)
	}
	_ = l.Close()
	began := time.Now()
	err = WaitGateway(t.Context(), open, 600*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), open) {
		t.Fatalf("a closed port: %v", err)
	}
	if d := time.Since(began); d < 600*time.Millisecond || d > 5*time.Second {
		t.Errorf("gave up after %v, want the bound", d)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := WaitGateway(ctx, open, time.Minute); err == nil {
		t.Error("a cancelled wait succeeded")
	}
}

// A mapping Windows refuses with error 1219 (another connection to the same server name
// under another user, whatever the port) says which server and how to clear it; any
// other failure, and any other OS, keeps its own error.
// Invariant: I-142
func TestMount_Explain1219(t *testing.T) {
	_, args, err := SMBCommand("windows", "127.0.0.1:20445", "T:")
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("exit status 1")
	out := "New-SmbMapping : Multiple connections to a server or shared resource by the same user, using more than one user name, are not allowed.\n    + FullyQualifiedErrorId : Windows System Error 1219,New-SmbMapping"
	got := explainMount("windows", args, out, cause)
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), `\\127.0.0.1 as another user`) || !strings.Contains(got.Error(), `net use \\127.0.0.1\<share> /delete`) {
		t.Errorf("1219: %v", got)
	}
	for _, tc := range []struct{ goos, out string }{{"windows", "System error 86"}, {"linux", out}, {"windows", ""}} {
		if got := explainMount(tc.goos, args, tc.out, cause); got == nil || got.Error() != cause.Error() {
			t.Errorf("%s %q: %v, want the error unchanged", tc.goos, tc.out, got)
		}
	}
}

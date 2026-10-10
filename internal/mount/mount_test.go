package mount

import (
	"strings"
	"testing"
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

// From an elevated window the mapping is made but the warning names the letter and the
// way out; from a normal one nothing is printed (docs/15 §6).
func TestElevatedWarning(t *testing.T) {
	if w := ElevatedWarning(false, "T:"); w != "" {
		t.Fatalf("not elevated: %q", w)
	}
	w := ElevatedWarning(true, "T:")
	for _, want := range []string{"administrator", "Explorer", "net use T: /delete", "tailraid mount"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
}

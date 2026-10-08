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
		{"windows", `mount -o anon,nolock,mtype=soft \\127.0.0.1\tailraid /Users/me/TailRAID`},
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
	if _, _, err := Command("plan9", "127.0.0.1:1", "/x"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	if _, _, err := Command("linux", "nonsense", "/x"); err == nil {
		t.Fatal("bad address accepted")
	}
	if prog, args, _ := UnmountCommand("darwin", "/x"); prog != "umount" || len(args) != 1 {
		t.Fatal("umount")
	}
}

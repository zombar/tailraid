package admin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/config"
)

// diskutil's answer: FileVault or an encrypted external volume is on, a described volume
// without either is off, and output that describes no volume is unknown.
// Invariant: I-341
func TestEncryption_ParseDiskutil_Table(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want Encryption
	}{
		{"filevault", "   File System Personality:   APFS\n   Encrypted:                 No\n   FileVault:                 Yes\n", EncryptedOn},
		{"external encrypted", "   File System Personality:   APFS\n   Encrypted:                 Yes\n", EncryptedOn},
		{"plain", "   File System Personality:   APFS\n   Encrypted:                 No\n   FileVault:                 No\n", EncryptedOff},
		{"hfs plain", "   File System Personality:   Journaled HFS+\n   Encrypted:                 No\n", EncryptedOff},
		{"not a volume", "Could not find disk for /x\n", EncryptedUnknown},
		{"empty", "", EncryptedUnknown},
	} {
		if got := parseDiskutil(tc.out); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// manage-bde's answer: Protection On, Protection Off, and a localized or absent line is
// unknown rather than off.
// Invariant: I-341
func TestEncryption_ParseManageBDE_Table(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want Encryption
	}{
		{"on", "Volume C: [Windows]\r\n    Conversion Status:    Fully Encrypted\r\n    Protection Status:    Protection On\r\n", EncryptedOn},
		{"off", "Volume C: [Windows]\r\n    Conversion Status:    Fully Decrypted\r\n    Protection Status:    Protection Off\r\n", EncryptedOff},
		{"localized", "Volume C:\r\n    Protection Status:    Schutz aktiviert\r\n", EncryptedUnknown},
		{"no line", "ERROR: An error occurred (code 0x80070005)\r\n", EncryptedUnknown},
	} {
		if got := parseManageBDE(tc.out); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Linux: the mount that holds the path, followed through the device-mapper chain. A
// LUKS device is on, LVM over LUKS is on, a plain partition is off, a filesystem that is
// not on a block device is unknown, and the longest mount point wins.
// Invariant: I-341
func TestEncryption_Linux_Table(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a Linux function over a Linux mountinfo fixture: slash paths and /dev symlinks")
	}
	root := t.TempDir()
	sys := filepath.Join(root, "block")
	dev := filepath.Join(root, "dev")
	mk := func(name, uuid string, slaves ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sys, name, "slaves"), 0o700); err != nil {
			t.Fatal(err)
		}
		if uuid != "" {
			if err := os.MkdirAll(filepath.Join(sys, name, "dm"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sys, name, "dm", "uuid"), []byte(uuid+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range slaves {
			if err := os.MkdirAll(filepath.Join(sys, name, "slaves", s), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("sda", "")
	mk("sda1", "")
	mk("sda2", "")
	mk("dm-0", "CRYPT-LUKS2-abcd-cryptroot", "sda2")
	mk("dm-1", "LVM-xyz", "dm-0")
	mk("dm-2", "LVM-plain", "sda1")
	mk("md0", "", "sda1")
	for _, d := range []string{"sda1", "dm-0", "dm-1", "dm-2", "md0"} {
		if err := os.WriteFile(filepath.Join(dev, d), nil, 0o600); err != nil {
			_ = os.MkdirAll(dev, 0o700)
			if err := os.WriteFile(filepath.Join(dev, d), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(dev, "mapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"cryptroot": "dm-0", "vg-lv": "dm-1", "vg-plain": "dm-2"} {
		if err := os.Symlink(filepath.Join(dev, target), filepath.Join(dev, "mapper", name)); err != nil {
			t.Fatal(err)
		}
	}
	mounts := strings.Join([]string{
		"1 0 8:1 / / rw,relatime - ext4 " + filepath.Join(dev, "sda1") + " rw",
		"2 1 253:0 / /crypt rw - xfs " + filepath.Join(dev, "mapper", "cryptroot") + " rw",
		"3 1 253:1 / /lvm rw shared:1 - ext4 " + filepath.Join(dev, "mapper", "vg-lv") + " rw",
		"4 1 253:2 / /lvm/plain rw - btrfs " + filepath.Join(dev, "mapper", "vg-plain") + " rw",
		"5 1 9:0 / /raid rw - ext4 " + filepath.Join(dev, "md0") + " rw",
		"6 1 0:40 / /mnt/with\\040space rw - ext4 " + filepath.Join(dev, "mapper", "cryptroot") + " rw",
		"7 1 0:41 / /zpool rw - zfs tank/data rw",
		"8 1 0:42 / /tmp rw - tmpfs tmpfs rw",
		"garbage line",
		"",
	}, "\n")
	mi := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(mi, []byte(mounts), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want Encryption
	}{
		{"/home/u/data", EncryptedOff},     // the root mount, a plain partition
		{"/crypt/disk1", EncryptedOn},      // dm-crypt
		{"/crypt", EncryptedOn},            // the mount point itself
		{"/lvm/x", EncryptedOn},            // LVM over LUKS
		{"/lvm/plain/x", EncryptedOff},     // the deeper mount wins: LVM over a plain partition
		{"/lvm/plainer", EncryptedOn},      // a sibling name is not under /lvm/plain
		{"/raid/x", EncryptedOff},          // md over plain partitions
		{"/mnt/with space/x", EncryptedOn}, // an escaped mount point
		{"/zpool/x", EncryptedUnknown},     // not a block filesystem
		{"/tmp/x", EncryptedUnknown},
	} {
		if got := linuxEncryption(tc.path, mi, sys); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.path, got, tc.want)
		}
	}
	if got := linuxEncryption("/x", filepath.Join(root, "missing"), sys); got != EncryptedUnknown {
		t.Errorf("no mountinfo: got %s", got)
	}
}

// A path is probed once, then served from the cache until an hour has passed; the
// report lists every path in config order.
// Invariant: I-341
func TestPaths_ProbeOncePerHour(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	calls := map[string]int{}
	p := NewPaths([]config.PathEntry{{Kind: "data directory", Path: "/d"}, {Kind: "disk", Path: "/a"}, {Kind: "mirror", Path: "/m"}}, clk)
	p.probe = func(path string) Encryption {
		calls[path]++
		if path == "/a" {
			return EncryptedOff
		}
		return EncryptedOn
	}
	for range 3 {
		rep := p.Report()
		if len(rep) != 3 || rep[0].Kind != "data directory" || rep[1].Path != "/a" || rep[1].Encrypted != EncryptedOff || rep[2].Kind != "mirror" {
			t.Fatalf("report %+v", rep)
		}
	}
	if calls["/a"] != 1 || calls["/d"] != 1 || calls["/m"] != 1 {
		t.Fatalf("probed %v within the hour", calls)
	}
	clk.Advance(ProbeEvery - time.Second)
	p.Report()
	if calls["/a"] != 1 {
		t.Fatalf("probed again before the hour: %v", calls)
	}
	clk.Advance(2 * time.Second)
	p.Report()
	if calls["/a"] != 2 {
		t.Fatalf("not probed again after the hour: %v", calls)
	}
	rep := p.Report()
	rep[0].Encrypted = EncryptedUnknown
	if p.Report()[0].Encrypted != EncryptedOn {
		t.Fatal("the report is the cache itself, not a copy")
	}
}

// Only off warns: on and unknown are shown in the report and say nothing.
// Invariant: I-341
func TestPathWarnings_Table(t *testing.T) {
	reps := []PathReport{
		{Kind: "data directory", Path: "/d", Encrypted: EncryptedOn},
		{Kind: "disk", Path: "/a", Encrypted: EncryptedOff},
		{Kind: "mirror", Path: "/m", Encrypted: EncryptedUnknown},
		{Kind: "disk", Path: "/b", Encrypted: EncryptedOff},
	}
	w := PathWarnings(reps)
	if len(w) != 2 || !strings.Contains(w[0], "disk /a") || !strings.Contains(w[1], "disk /b") || !strings.Contains(w[0], "full-disk encryption") {
		t.Fatalf("warnings %v", w)
	}
	if w := PathWarnings(nil); len(w) != 0 {
		t.Fatalf("no paths warned: %v", w)
	}
}

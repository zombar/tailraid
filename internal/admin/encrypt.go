package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/config"
)

// Encryption is what a device knows about the full-disk encryption under one of its
// paths (docs/08 §Files on disk, I-341). A probe that cannot answer says unknown, never
// off: only off is a warning.
type Encryption string

const (
	EncryptedOn      Encryption = "on"
	EncryptedOff     Encryption = "off"
	EncryptedUnknown Encryption = "unknown"
)

// PathReport is one path of the device in status's `machine.paths` (docs/12 §8).
type PathReport struct {
	Kind      string     `json:"kind"` // "data directory", "disk" or "mirror"
	Path      string     `json:"path"`
	Encrypted Encryption `json:"encrypted"`
}

// ProbeEvery is how long a path's answer is kept before the filesystem is asked again:
// the probe shells out on macOS and Windows, and the status refresher runs every 2 s.
const ProbeEvery = time.Hour

// Paths answers the device's paths from a cache refreshed at most once per ProbeEvery.
type Paths struct {
	mu      sync.Mutex
	entries []config.PathEntry
	probe   func(path string) Encryption
	clk     clock.Clock
	at      time.Time
	last    []PathReport
}

// NewPaths probes entries with the OS's own tool (docs/08 §Files on disk).
func NewPaths(entries []config.PathEntry, clk clock.Clock) *Paths {
	return &Paths{entries: entries, probe: platformEncryption, clk: clk}
}

// Report is every path with its answer, probed now when the cache is older than
// ProbeEvery or empty; a nil Paths (a test's admin ops) has none.
func (p *Paths) Report() []PathReport {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	if p.last != nil && now.Sub(p.at) < ProbeEvery {
		return slices.Clone(p.last)
	}
	out := make([]PathReport, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, PathReport{Kind: e.Kind, Path: e.Path, Encrypted: p.probe(e.Path)})
	}
	p.last, p.at = out, now
	return slices.Clone(out)
}

// PathWarnings is one line per path whose filesystem reports encryption off; unknown
// is shown in the report and never warned about (I-341).
func PathWarnings(reps []PathReport) []string {
	var w []string
	for _, r := range reps {
		if r.Encrypted == EncryptedOff {
			w = append(w, fmt.Sprintf("%s %s is on a filesystem without full-disk encryption: turn it on (FileVault, LUKS or BitLocker, docs/08)", r.Kind, r.Path))
		}
	}
	return w
}

// parseDiskutil reads `diskutil info <path>`: a volume under FileVault says
// "FileVault: Yes", an encrypted external APFS or Core Storage volume "Encrypted: Yes".
// Neither key present means diskutil did not describe a volume: unknown.
func parseDiskutil(out string) Encryption {
	seen := false
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "FileVault", "Encrypted":
			seen = true
			if strings.TrimSpace(v) == "Yes" {
				return EncryptedOn
			}
		}
	}
	if seen {
		return EncryptedOff
	}
	return EncryptedUnknown
}

// parseManageBDE reads `manage-bde -status <drive>`: "Protection Status: Protection On".
// A localized Windows prints other words, and a caller without the right to ask gets an
// error: both are unknown.
func parseManageBDE(out string) Encryption {
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "Protection Status" {
			continue
		}
		switch strings.TrimSpace(v) {
		case "Protection On":
			return EncryptedOn
		case "Protection Off":
			return EncryptedOff
		}
		return EncryptedUnknown
	}
	return EncryptedUnknown
}

// blockFilesystems are the filesystems that sit directly on a block device, so the
// device chain under them says whether anything encrypts the bytes. Anything else (ZFS
// with its own encryption, FUSE, a network share, tmpfs, overlays) is unknown.
var blockFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
}

// linuxEncryption follows path's mount (the longest mount point in mountinfo that
// contains it) to its source device, then the chain under that device in sysBlock
// (`<dev>/slaves/*`: LVM over LUKS, md over LUKS) looking for a device-mapper uuid that
// begins with CRYPT-. A plain disk or partition at the bottom is off.
func linuxEncryption(path, mountinfo, sysBlock string) Encryption {
	src, fstype := mountSource(path, mountinfo)
	if src == "" || !blockFilesystems[fstype] {
		return EncryptedUnknown
	}
	dev, err := filepath.EvalSymlinks(src)
	if err != nil {
		return EncryptedUnknown
	}
	return chainEncryption(sysBlock, filepath.Base(dev), 0)
}

func chainEncryption(sysBlock, name string, depth int) Encryption {
	if depth > 8 {
		return EncryptedUnknown
	}
	uuid, err := os.ReadFile(filepath.Join(sysBlock, name, "dm", "uuid")) //nolint:gosec // a sysfs attribute under the device the mount names
	if err == nil && strings.HasPrefix(string(uuid), "CRYPT-") {
		return EncryptedOn
	}
	if slaves, err := os.ReadDir(filepath.Join(sysBlock, name, "slaves")); err == nil && len(slaves) > 0 {
		res := EncryptedOff
		for _, s := range slaves {
			switch chainEncryption(sysBlock, s.Name(), depth+1) {
			case EncryptedOn:
				return EncryptedOn
			case EncryptedUnknown:
				res = EncryptedUnknown
			case EncryptedOff:
			}
		}
		return res
	}
	if _, err := os.Stat(filepath.Join(sysBlock, name)); err == nil {
		return EncryptedOff
	}
	return EncryptedUnknown
}

// mountSource reads /proc/self/mountinfo (its path given) and answers the source and
// filesystem type of the mount that holds path: the longest mount point that is a prefix
// of it. Mount points carry octal escapes (\040 for a space).
func mountSource(path, mountinfo string) (src, fstype string) {
	b, err := os.ReadFile(mountinfo) //nolint:gosec // /proc/self/mountinfo, or a test's copy
	if err != nil {
		return "", ""
	}
	path = filepath.Clean(path)
	best := -1
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if len(fields) < 7 || sep < 5 || sep+2 >= len(fields) {
			continue
		}
		mp := unescapeMount(fields[4])
		if mp != path && !strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if len(mp) > best {
			best, src, fstype = len(mp), unescapeMount(fields[sep+2]), fields[sep+1]
		}
	}
	return src, fstype
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

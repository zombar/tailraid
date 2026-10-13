package smb

import (
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// folderFS is tinyFS with a CaseFolder that counts its calls and a ListDir that counts
// its listings.
type folderFS struct {
	*tinyFS
	folds, lists int
	ok           bool
}

func (f *folderFS) FoldName(_, want string) (string, bool, bool) {
	f.folds++
	if want == "FILE.TXT" {
		return "file.txt", true, f.ok
	}
	return "", false, f.ok
}

func (f *folderFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	f.lists++
	return f.tinyFS.ListDir(p)
}

// A backend that can find a name regardless of case is asked instead of listed: a CREATE of
// a name not there yet costs no listing of its folder. One that says it cannot (ok false)
// is listed, as any backend without the capability.
// Invariant: I-267
func TestFoldedMatch_AsksTheCaseFolder(t *testing.T) {
	f := &folderFS{tinyFS: &tinyFS{body: []byte("x")}, ok: true}
	if got := resolveCase(f, "/FILE.TXT"); got != "/file.txt" || f.lists != 0 || f.folds == 0 {
		t.Fatalf("resolved %q with %d listings and %d folds, want /file.txt from the folder", got, f.lists, f.folds)
	}
	if got := resolveCase(f, "/new.txt"); got != "/new.txt" || f.lists != 0 {
		t.Fatalf("a new name resolved to %q after %d listings, want none", got, f.lists)
	}
	f.ok = false
	if got := resolveCase(f, "/FILE.TXT"); got != "/file.txt" || f.lists == 0 {
		t.Fatalf("a folder that cannot fold: %q after %d listings, want the listing's answer", got, f.lists)
	}
}

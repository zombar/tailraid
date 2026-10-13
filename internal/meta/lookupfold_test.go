package meta

import "testing"

// LookupFold finds the entry of a folder whose name differs from the one asked only by
// case, from the folder's names alone (no entry copied): the name itself when it exists,
// the first by sorted name when several differ by case alone, and nothing when none
// does or the parent is not a folder. The SMB gateway resolves a caseless name with it on
// every CREATE of a new name, where it listed the whole folder (I-267).
// Invariant: I-267
func TestFSM_LookupFold(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "Photos")
	tf.create(d, "IMG_0001.jpg")
	tf.create(d, "readme")
	tf.create(d, "README")
	f := tf.create(d, "plain")
	for _, tc := range []struct {
		parent Ino
		name   string
		want   string
		found  bool
	}{
		{RootIno, "photos", "Photos", true},
		{RootIno, "Photos", "Photos", true},
		{d, "img_0001.JPG", "IMG_0001.jpg", true},
		{d, "ReadMe", "README", true}, // two differ by case alone: the first by sorted name
		{d, "missing", "", false},
		{d, "", "", false},
		{f, "x", "", false},            // not a folder
		{Ino(1 << 40), "x", "", false}, // no such inode
	} {
		got, found := tf.f.LookupFold(tc.parent, tc.name)
		if got != tc.want || found != tc.found {
			t.Errorf("LookupFold(%d, %q) = %q %v, want %q %v", tc.parent, tc.name, got, found, tc.want, tc.found)
		}
	}
}

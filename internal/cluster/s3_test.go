package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// add_s3 registers a bucket once (docs/10 §4.1).
// Invariant: I-137
func TestMember_AddS3_Refusals(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	add := func(name, id, endpoint, bucket, prefix string, capacity uint64) error {
		return n.MemberOp(ctx, rpc.MemberOp{
			Op: "add_s3", Name: name, Capacity: capacity,
			S3: &rpc.S3Spec{ID: id, Endpoint: endpoint, Bucket: bucket, Prefix: prefix, Region: "eu-central-003"},
		})
	}
	if err := add("offsite", "aaaaaaaaaaaaaaaa", "https://s3.example.com", "b1", "c1", 1<<40); err != nil {
		t.Fatal(err)
	}
	var row meta.Node
	for _, nd := range n.FSM.Nodes() {
		if nd.Kind == meta.KindS3 {
			row = nd
		}
	}
	if row.Name != "offsite" || row.ID != "aaaaaaaaaaaaaaaa" || row.S3Endpoint != "https://s3.example.com" || row.S3Bucket != "b1" ||
		row.S3Prefix != "c1" || row.S3Region != "eu-central-003" || row.Capacity != 1<<40 || row.State != meta.Active || row.JoinedAt == 0 || row.Host != 0 || row.Voter {
		t.Fatalf("row %+v", row)
	}
	// a retry with the same ID updates the row it created
	if err := add("offsite", "aaaaaaaaaaaaaaaa", "https://s3.example.com", "b1", "c1", 2<<40); err != nil {
		t.Fatalf("retry: %v", err)
	}
	rows := 0
	for _, nd := range n.FSM.Nodes() {
		if nd.Kind == meta.KindS3 {
			rows++
			if nd.Capacity != 2<<40 || nd.Idx != row.Idx {
				t.Fatalf("retry row %+v", nd)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("%d S3 rows after a retry", rows)
	}
	self, _ := n.SelfIdx()
	selfRow, _ := n.FSM.Node(self)
	for _, c := range []struct {
		why                                string
		name, id, endpoint, bucket, prefix string
		capacity                           uint64
	}{
		{"name of the S3 row", "offsite", "bbbbbbbbbbbbbbbb", "https://s3.other.com", "b2", "", 1},
		{"name of a device", selfRow.Name, "bbbbbbbbbbbbbbbb", "https://s3.other.com", "b2", "", 1},
		{"same bucket, other prefix", "second", "bbbbbbbbbbbbbbbb", "https://s3.example.com", "b1", "c2", 1},
		{"same bucket, trailing slash", "second", "bbbbbbbbbbbbbbbb", "https://s3.example.com/", "b1", "", 1},
		{"not a URL", "second", "bbbbbbbbbbbbbbbb", "s3.example.com", "b2", "", 1},
		{"ftp", "second", "bbbbbbbbbbbbbbbb", "ftp://s3.example.com", "b2", "", 1},
		{"no bucket", "second", "bbbbbbbbbbbbbbbb", "https://s3.example.com", "", "", 1},
		{"no capacity", "second", "bbbbbbbbbbbbbbbb", "https://s3.example.com", "b2", "", 0},
		{"no name", "", "bbbbbbbbbbbbbbbb", "https://s3.example.com", "b2", "", 1},
		{"short id", "second", "bbbb", "https://s3.example.com", "b2", "", 1},
		{"id of the device", "second", selfRow.ID, "https://s3.example.com", "b2", "", 1},
	} {
		if err := add(c.name, c.id, c.endpoint, c.bucket, c.prefix, c.capacity); !errors.Is(err, ErrBadMemberOp) {
			t.Errorf("%s: %v, want ErrBadMemberOp", c.why, err)
		}
	}
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "add_s3", Name: "x", Capacity: 1}); !errors.Is(err, ErrBadMemberOp) {
		t.Errorf("no spec: %v", err)
	}
	// once the row is removed, its name and bucket are free again
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: row.Idx}); err != nil {
		t.Fatal(err)
	}
	if err := add("offsite", "cccccccccccccccc", "https://s3.example.com", "b1", "c1", 1); err != nil {
		t.Fatalf("re-adding a removed bucket: %v", err)
	}
}

// The credentials file is private to the device and keeps every other node's keys when
// one is written (docs/10 §4.1).
// Invariant: I-139
func TestS3Keys_FileModeAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if keys, err := LoadS3Keys(dir); err != nil || len(keys) != 0 {
		t.Fatalf("no file: %v %v", keys, err)
	}
	a := s3.Credentials{AccessKey: "AK1", SecretKey: "s3cr3t/with: yaml"}
	b := s3.Credentials{AccessKey: "AK2", SecretKey: "other"}
	if err := SaveS3Key(dir, "offsite", a); err != nil {
		t.Fatal(err)
	}
	if err := SaveS3Key(dir, "second", b); err != nil {
		t.Fatal(err)
	}
	if err := SaveS3Key(dir, "offsite", s3.Credentials{AccessKey: "AK1", SecretKey: "rotated"}); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadS3Keys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["second"] != b || keys["offsite"].SecretKey != "rotated" {
		t.Fatalf("keys %+v", keys)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, S3KeysFile))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", st.Mode().Perm())
		}
	}
	if err := SaveS3Key(dir, "", a); err == nil {
		t.Fatal("a key without a name was saved")
	}
	if err := os.WriteFile(filepath.Join(dir, S3KeysFile), []byte("offsite: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadS3Keys(dir); err == nil {
		t.Fatal("a broken file loaded")
	}
}

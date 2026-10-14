package cluster

import (
	"errors"
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

// An S3 node's credentials are the secrets s3/<name>/access_key and s3/<name>/secret_key;
// a name off the secret pattern or with a slash has none; the opened secrets pair into
// credentials by node, and a node with one of the two, an empty one or an unknown field
// has none.
// Invariant: I-308
func TestS3SecretNames(t *testing.T) {
	for name, ok := range map[string]bool{"offsite": true, "b2.eu-1": true, "Offsite": false, "with space": false, "": false, "a/b": false} {
		access, secret, valid := S3SecretNames(name)
		if valid != ok || ok && (access != "s3/"+name+"/access_key" || secret != "s3/"+name+"/secret_key") {
			t.Errorf("S3SecretNames(%q) = %q %q %v", name, access, secret, valid)
		}
	}
	got := S3KeysFromSecrets(map[string]string{
		"s3/offsite/access_key": "AK", "s3/offsite/secret_key": "SK",
		"s3/half/access_key":  "AK2",
		"s3/empty/access_key": "AK3", "s3/empty/secret_key": "",
		"s3/odd/access_key": "AK4", "s3/odd/secret_key": "SK4", "s3/odd/seal_key": "ignored",
		"s3/deep/er/access_key": "AK5", "s3/deep/er/secret_key": "SK5",
		"tmdb": "k", "s3": "x", "s3/": "y",
	})
	want := map[string]s3.Credentials{"offsite": {AccessKey: "AK", SecretKey: "SK"}, "odd": {AccessKey: "AK4", SecretKey: "SK4"}, "deep/er": {AccessKey: "AK5", SecretKey: "SK5"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for node, c := range want {
		if got[node] != c {
			t.Errorf("%s: %+v, want %+v", node, got[node], c)
		}
	}
}

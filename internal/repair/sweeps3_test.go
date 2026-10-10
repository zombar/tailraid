package repair_test

import (
	"crypto/sha256"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/testharness"
)

// The leader's S3 sweep deletes exactly the objects older than grace that the chunk
// table does not place on that bucket; it keeps every younger object and everything
// placed there (docs/06 §3).
// Invariant: I-138, I-14
// Lifecycle: L-170
func TestSweepS3_DeletesOldOrphansOnly(t *testing.T) {
	f := testharness.NewFabric(t, 2, 1, 1)
	srv := s3test.New()
	defer srv.Close()
	r := f.MustApply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
		ID: "s3aaaaaaaaaaaaaa", Name: "offsite", Kind: meta.KindS3, Capacity: 1 << 40, S3Endpoint: srv.URL, S3Bucket: s3test.Name, S3PathStyle: true,
	}}})
	bucket := meta.NodeIdx(r.Ino)
	c := flushedChunk(t, f)
	place := slices.Clone(c.Placement)
	place[1] = bucket // the parity slot on the bucket
	f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, Placement: place, K: c.K, M: c.M, Expect: c.Placement}})

	b, err := s3.New(srv.Target(""), s3test.Creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := rpc.NewClient([16]byte{}, nil)
	client.SetS3(func(idx meta.NodeIdx) (*s3.Bucket, bool) { return b, idx == bucket })

	const grace = 10 * time.Minute
	other := chunk.Key(sha256.Sum256([]byte("gone")), 1, 1, 1)
	put := func(key chunk.ShardKey, age time.Duration) string {
		name := rpc.ShardObject(key)
		srv.PutObject(name, []byte("x"))
		srv.Age(name, age)
		return name
	}
	keepPlaced := put(chunk.Key(c.Hash, c.K, c.M, 1), 2*grace)
	dropOtherSlot := put(chunk.Key(c.Hash, c.K, c.M, 0), 2*grace) // slot 0 is on a disk
	dropOldPolicy := put(chunk.Key(c.Hash, 2, 1, 1), 2*grace)
	dropUnknown := put(other, grace+time.Second)
	keepYoung := put(chunk.Key(sha256.Sum256([]byte("young")), 1, 1, 0), grace-time.Second)
	srv.PutObject("shards/notes.txt", []byte("an operator's file"))
	srv.Age("shards/notes.txt", 2*grace)

	n, err := repair.SweepS3(ctx, client, f.FSM, bucket, grace, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("deleted %d objects, want 3", n)
	}
	for name, want := range map[string]bool{keepPlaced: true, keepYoung: true, "shards/notes.txt": true, dropOtherSlot: false, dropOldPolicy: false, dropUnknown: false} {
		if _, ok := srv.Object(name); ok != want {
			t.Errorf("%s present=%v, want %v", name, ok, want)
		}
	}
	if d := srv.Count(http.MethodDelete); d != 3 {
		t.Errorf("%d DELETEs", d)
	}
}

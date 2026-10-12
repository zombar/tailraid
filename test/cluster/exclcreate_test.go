package cluster_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

// Two clients on two gateways create the same names exclusively at the same moment,
// each writing its own bytes at a non-zero offset once its create succeeded: every name
// has exactly one winner, the loser sees EEXIST, and every gateway reads the winner's
// bytes, never a truncated or mixed file.
// Invariant: I-222
func TestCluster_ExclCreate_TwoGateways_OneWins(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	const names = 40
	type outcome struct {
		won  bool
		data []byte
	}
	results := make([][2]outcome, names)
	for i := range names {
		path := fmt.Sprintf("/race-%02d.bin", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := range 2 {
			wg.Go(func() {
				<-start
				data := testharness.Pattern(300+g, byte(i*2+g+1))
				fh, err := c.Nodes[g].FS.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
				if err != nil {
					if !errors.Is(err, os.ErrExist) {
						t.Errorf("%s via n%d: err=%v, want EEXIST or success", path, g, err)
					}
					return
				}
				defer func() { _ = fh.Close() }()
				if _, err := fh.Seek(7, io.SeekStart); err != nil {
					t.Error(err)
					return
				}
				if _, err := fh.Write(data); err != nil {
					t.Error(err)
					return
				}
				results[i][g] = outcome{won: true, data: data}
			})
		}
		close(start)
		wg.Wait()
	}
	for g := range 2 {
		flushAll(t, c, g)
	}
	for i := range names {
		path := fmt.Sprintf("/race-%02d.bin", i)
		r := results[i]
		if r[0].won == r[1].won {
			t.Errorf("%s: n0 won %v, n1 won %v: want exactly one", path, r[0].won, r[1].won)
			continue
		}
		w := r[0]
		if r[1].won {
			w = r[1]
		}
		want := append(make([]byte, 7), w.data...)
		for n := range c.Nodes {
			got, err := c.ReadFileCold(n, path)
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s via n%d: err=%v, %d bytes, want the winner's %d", path, n, err, len(got), len(want))
			}
		}
	}
}

package chunk

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestKey_RoundTrip(t *testing.T) {
	h := sha256.Sum256([]byte("abc"))
	for _, tc := range []struct {
		k, m uint8
		idx  int
	}{{1, 0, 0}, {2, 1, 0}, {2, 1, 2}, {4, 2, 5}, {1, 2, 2}, {200, 56, 255}} {
		k := Key(h, tc.k, tc.m, tc.idx)
		gh, gk, gm, gi, err := ParseKey(k)
		if err != nil || gh != h || gk != tc.k || gm != tc.m || gi != tc.idx {
			t.Fatalf("ParseKey(%q) = %x %d %d %d %v", k, gh, gk, gm, gi, err)
		}
	}
	want := ShardKey("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad.2-1.0")
	if Key(h, 2, 1, 0) != want {
		t.Fatalf("key = %s", Key(h, 2, 1, 0))
	}
}

func TestParseKey_Rejects(t *testing.T) {
	good := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, bad := range []string{
		"", good, good + ".", good + ".2-1", good + ".2-1.", good + ".2-1.x", good + ".2-1.-1",
		good + ".2-1.3", // idx >= k+m
		good + ".2-1.01", good + ".02-1.0", good + ".2-01.0", good + ".0-1.0", good + ".2.0",
		good + ".2-1-0", good + ".200-57.0", // k+m > 256
		good + ".256-0.0", // k is a uint8 in Key
		good + ".2-1.256", good[:63] + ".2-1.0", good + "0.2-1.0", strings.ToUpper(good) + ".2-1.0",
		"zz" + good[2:] + ".2-1.0", "../" + good + ".2-1.0", good + ".2-1.0/../x", ".tmp-abc",
	} {
		if _, _, _, _, err := ParseKey(ShardKey(bad)); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

package chunk

import "testing"

// Key and ParseKey are inverses over every valid (k, m, idx): a key that parses encodes
// back to itself, and ParseKey never accepts what Key cannot produce.
//
// Invariant: I-11
func FuzzParseKey(f *testing.F) {
	f.Add(uint8(2), uint8(1), 0, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad.2-1.0")
	f.Add(uint8(255), uint8(1), 255, "")
	f.Fuzz(func(t *testing.T, k, m uint8, idx int, raw string) {
		if int(k)+int(m) <= 256 && k >= 1 && idx >= 0 && idx < int(k)+int(m) {
			var h [32]byte
			copy(h[:], raw)
			key := Key(h, k, m, idx)
			gh, gk, gm, gi, err := ParseKey(key)
			if err != nil || gh != h || gk != k || gm != m || gi != idx {
				t.Fatalf("Key(%x,%d,%d,%d) = %s does not round-trip: %v", h[:4], k, m, idx, key, err)
			}
		}
		if h, k2, m2, i2, err := ParseKey(ShardKey(raw)); err == nil {
			if string(Key(h, k2, m2, i2)) != raw {
				t.Fatalf("ParseKey accepted %q, which Key spells %s", raw, Key(h, k2, m2, i2))
			}
		}
	})
}

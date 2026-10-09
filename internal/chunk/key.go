// Package chunk is the shard store of a node: content-addressed shard files with an
// integrity header, written durably (temp → fsync → rename → dir sync), plus a bounded
// cache of decoded chunks and the sweep that removes orphaned shards. It never talks to
// the network and knows nothing about Raft; the chunk table is consulted through a
// callback so this package has no dependency on internal/meta.
package chunk

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ShardKey names one shard: hex(chunk hash) + "." + k + "-" + m + "." + shard index,
// e.g. "ab12…ef.2-1.0". The encoding is part of the name because a policy migration
// re-encodes the same hash with a different (k,m) and old and new shards must coexist on a
// node until the new placement is committed. It is the file name under shards/<hh>/ and
// the object key on an S3 node.
type ShardKey string

// MaxShardIdx is the largest shard index (k+m ≤ 256).
const MaxShardIdx = 255

// Key builds the ShardKey for chunk hash h encoded as (k,m), shard index idx.
func Key(h [32]byte, k, m uint8, idx int) ShardKey {
	return ShardKey(hex.EncodeToString(h[:]) + "." + strconv.Itoa(int(k)) + "-" +
		strconv.Itoa(int(m)) + "." + strconv.Itoa(idx))
}

// ErrBadKey is returned by ParseKey for anything that is not exactly a Key() output.
var ErrBadKey = errors.New("chunk: malformed shard key")

// ParseKey is the strict inverse of Key: 64 lowercase hex digits, ".", k "-" m, ".", and
// a canonical decimal index in [0, MaxShardIdx] with idx < k+m. Anything else (including
// path separators) is rejected so a key received over the network is a safe file name.
func ParseKey(key ShardKey) (h [32]byte, k, m uint8, idx int, err error) {
	s := string(key)
	bad := func(why string) ([32]byte, uint8, uint8, int, error) {
		return h, 0, 0, 0, fmt.Errorf("%w: %q: %s", ErrBadKey, s, why)
	}
	if len(s) < 64+1+3+1+1 || s[64] != '.' {
		return bad("shape")
	}
	hx := s[:64]
	if strings.ToLower(hx) != hx {
		return bad("uppercase hex")
	}
	b, err := hex.DecodeString(hx)
	if err != nil {
		return bad(err.Error())
	}
	rest := strings.Split(s[65:], ".")
	if len(rest) != 2 {
		return bad("segments")
	}
	km := strings.Split(rest[0], "-")
	if len(km) != 2 {
		return bad("k-m")
	}
	kk, ok1 := canonicalInt(km[0], 1, 255) // uint8, like Key
	mm, ok2 := canonicalInt(km[1], 0, 255)
	ii, ok3 := canonicalInt(rest[1], 0, MaxShardIdx)
	if !ok1 || !ok2 || !ok3 || kk+mm > 256 || ii >= kk+mm {
		return bad("k, m or index out of range")
	}
	copy(h[:], b)
	return h, uint8(kk), uint8(mm), ii, nil
}

func canonicalInt(s string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

package chunk

import (
	"crypto/sha256"
	"errors"

	"github.com/zombar/tailraid/internal/meta"
)

// BlockSize is the unit a data shard's block table hashes: a client's range fetch moves
// whole blocks and checks each against the table (docs/04 §5, I-422). It is part of the
// recorded roots, so it never changes.
const BlockSize = 64 << 10

// ErrNoRange is GetRange's answer from a node that serves no slice (an S3 node, whose
// object comes with no block table): the reader fetches the chunk instead (I-423).
var ErrNoRange = errors.New("chunk: no range from this node")

// Slice is what a range GET returns: the shard's block table and the payload bytes from
// Off, whole blocks covering what was asked, short only at the payload's end (docs/04 §5,
// I-423).
type Slice struct {
	Table []meta.Hash
	Off   int64
	Data  []byte
}

// BlockSums is payload's block table: the SHA-256 of each BlockSize block in order, the
// last one shorter; an empty payload has none.
func BlockSums(payload []byte) []meta.Hash {
	table := make([]meta.Hash, (len(payload)+BlockSize-1)/BlockSize)
	for i := range table {
		table[i] = sha256.Sum256(payload[i*BlockSize : min((i+1)*BlockSize, len(payload))])
	}
	return table
}

// BlockRoot is the root of a block table: the SHA-256 of its hashes concatenated.
func BlockRoot(table []meta.Hash) meta.Hash {
	h := sha256.New()
	for i := range table {
		h.Write(table[i][:])
	}
	var root meta.Hash
	copy(root[:], h.Sum(nil))
	return root
}

// ShardRoot is the recorded sum of a data shard under SumsBlocks (docs/04 §5, I-422).
func ShardRoot(payload []byte) meta.Hash { return BlockRoot(BlockSums(payload)) }

// BlockRange widens [lo, hi) of a payload n bytes long to the whole blocks covering it,
// clamped to the payload; an empty or inverted range and one starting past the end are
// empty.
func BlockRange(lo, hi, n int64) (a, b int64) {
	if lo < 0 || hi <= lo || lo >= n {
		return 0, 0
	}
	a = lo / BlockSize * BlockSize
	b = min((hi+BlockSize-1)/BlockSize*BlockSize, n)
	return a, b
}

// VerifySlice reports whether s is a slice of the shard whose recorded root is root: the
// table hashes to the root, Off is block-aligned, and every block of Data hashes to its
// entry, a short block only as the table's last (docs/04 §5, I-424).
func VerifySlice(s Slice, root meta.Hash) bool {
	if BlockRoot(s.Table) != root || s.Off < 0 || s.Off%BlockSize != 0 {
		return false
	}
	first := int(s.Off / BlockSize)
	for i := 0; i*BlockSize < len(s.Data); i++ {
		j := first + i
		end := min((i+1)*BlockSize, len(s.Data))
		if j >= len(s.Table) || sha256.Sum256(s.Data[i*BlockSize:end]) != s.Table[j] {
			return false
		}
		if end-i*BlockSize < BlockSize && j != len(s.Table)-1 {
			return false
		}
	}
	return true
}

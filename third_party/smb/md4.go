// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"math/bits"
)

// MD4 (RFC 1320) is here because NTLM's password hash is MD4 of the UTF-16LE
// password and nothing else will do. It is broken as a hash and that is not a
// reason to avoid it: it is not being used as one. It is a fixed function
// inside a protocol, and reimplementing it is what keeps this module free of
// dependencies -- golang.org/x/crypto/md4 is deprecated and would be a
// dependency on something nobody maintains.
//
// This is the whole algorithm: three rounds of sixteen operations over a
// 64-byte block, with the standard padding.
func md4sum(data []byte) []byte {
	var s [4]uint32 = [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}

	// Padding: a 0x80 byte, zeros, and the bit length in the last eight.
	msg := make([]byte, len(data), (len(data)+72)/64*64)
	copy(msg, data)
	msg = append(msg, 0x80)
	for len(msg)%64 != 56 {
		msg = append(msg, 0)
	}
	msg = binary.LittleEndian.AppendUint64(msg, uint64(len(data))*8)

	var x [16]uint32
	for off := 0; off < len(msg); off += 64 {
		block := msg[off : off+64]
		for i := range x {
			x[i] = binary.LittleEndian.Uint32(block[i*4:])
		}
		a, b, c, d := s[0], s[1], s[2], s[3]

		// Round 1: F(x,y,z) = (x AND y) OR (NOT x AND z)
		for _, r := range [...]struct{ i, sft int }{
			{0, 3}, {1, 7}, {2, 11}, {3, 19}, {4, 3}, {5, 7}, {6, 11}, {7, 19},
			{8, 3}, {9, 7}, {10, 11}, {11, 19}, {12, 3}, {13, 7}, {14, 11}, {15, 19},
		} {
			a = bits.RotateLeft32(a+(b&c|^b&d)+x[r.i], r.sft)
			a, b, c, d = d, a, b, c
		}
		// Round 2: G(x,y,z) = (x AND y) OR (x AND z) OR (y AND z), + 0x5a827999
		for _, r := range [...]struct{ i, sft int }{
			{0, 3}, {4, 5}, {8, 9}, {12, 13}, {1, 3}, {5, 5}, {9, 9}, {13, 13},
			{2, 3}, {6, 5}, {10, 9}, {14, 13}, {3, 3}, {7, 5}, {11, 9}, {15, 13},
		} {
			a = bits.RotateLeft32(a+(b&c|b&d|c&d)+x[r.i]+0x5a827999, r.sft)
			a, b, c, d = d, a, b, c
		}
		// Round 3: H(x,y,z) = x XOR y XOR z, + 0x6ed9eba1
		for _, r := range [...]struct{ i, sft int }{
			{0, 3}, {8, 9}, {4, 11}, {12, 15}, {2, 3}, {10, 9}, {6, 11}, {14, 15},
			{1, 3}, {9, 9}, {5, 11}, {13, 15}, {3, 3}, {11, 9}, {7, 11}, {15, 15},
		} {
			a = bits.RotateLeft32(a+(b^c^d)+x[r.i]+0x6ed9eba1, r.sft)
			a, b, c, d = d, a, b, c
		}
		s[0] += a
		s[1] += b
		s[2] += c
		s[3] += d
	}
	out := make([]byte, 16)
	for i, v := range s {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

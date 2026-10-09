// SPDX-License-Identifier: BSD-3-Clause

package smb

import "crypto/aes"

// AES-CMAC (RFC 4493) is how SMB 3.x signs, and the standard library has no
// CMAC. It is sixty lines and a fixed function inside a protocol, which is the
// same reason md4.go is here: a dependency for this would be a dependency on
// something nobody maintains.
//
// The construction is CBC-MAC with a tweak that makes it safe for messages of
// any length: two subkeys derived from the block cipher's encryption of zero,
// one of which is mixed into the last block depending on whether it needed
// padding.

// cmacSum returns the 16-byte CMAC of msg under a 128-bit key.
func cmacSum(key, msg []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		// A key of the wrong length is a programming error here: every key
		// this package derives is sixteen bytes.
		panic("smb: cmac: " + err.Error())
	}
	const blockSize = aes.BlockSize

	// The subkeys: encrypt a zero block, then shift left, xoring in the
	// polynomial when the top bit was set.
	l := make([]byte, blockSize)
	block.Encrypt(l, l)
	k1 := shiftLeftXor(l)
	k2 := shiftLeftXor(k1)

	last := make([]byte, blockSize)
	n := (len(msg) + blockSize - 1) / blockSize
	complete := n > 0 && len(msg)%blockSize == 0
	if complete {
		copy(last, msg[(n-1)*blockSize:])
		xorInto(last, k1)
	} else {
		// The final block is padded with a one bit and zeros, and mixed with
		// the OTHER subkey -- which is what stops a padded message colliding
		// with an unpadded one that happens to end in the same bytes.
		rest := 0
		if n > 0 {
			rest = (n - 1) * blockSize
		} else {
			n = 1
		}
		copy(last, msg[rest:])
		last[len(msg)-rest] = 0x80
		xorInto(last, k2)
	}

	x := make([]byte, blockSize)
	for i := 0; i < n-1; i++ {
		xorInto(x, msg[i*blockSize:(i+1)*blockSize])
		block.Encrypt(x, x)
	}
	xorInto(x, last)
	block.Encrypt(x, x)
	return x
}

// shiftLeftXor shifts a block left by one bit, xoring in the CMAC polynomial
// when a bit fell off the top.
func shiftLeftXor(b []byte) []byte {
	out := make([]byte, len(b))
	overflow := byte(0)
	for i := len(b) - 1; i >= 0; i-- {
		out[i] = b[i]<<1 | overflow
		overflow = b[i] >> 7
	}
	if overflow != 0 {
		out[len(out)-1] ^= 0x87
	}
	return out
}

func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

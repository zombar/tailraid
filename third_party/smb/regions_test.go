// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"testing"
)

// vdlFile is a handle's file that knows its valid data length.
type vdlFile struct {
	vdl, eof int64
}

func (f *vdlFile) ReadAt(p []byte, _ int64) (int, error) { return 0, nil }
func (f *vdlFile) Close() error                          { return nil }
func (f *vdlFile) Size() int64                           { return f.eof }
func (f *vdlFile) ValidData() (valid, size int64, err error) {
	return f.vdl, f.eof, nil
}

const ioctlFixed = 56 // the IOCTL request body without its buffer

// ioctlReq is an IOCTL request body for the handle id with input in (placed right after
// the fixed part, 8-aligned) and room for maxOut bytes of output.
func ioctlReq(code uint32, id [16]byte, in []byte, maxOut uint32, fsctl bool) []byte {
	body := make([]byte, ioctlFixed, ioctlFixed+len(in))
	binary.LittleEndian.PutUint16(body[0:], 57)
	binary.LittleEndian.PutUint32(body[4:], code)
	copy(body[8:], id[:])
	if len(in) > 0 {
		binary.LittleEndian.PutUint32(body[24:], headerLen+ioctlFixed)
		binary.LittleEndian.PutUint32(body[28:], uint32(len(in)))
	}
	binary.LittleEndian.PutUint32(body[44:], maxOut)
	if fsctl {
		binary.LittleEndian.PutUint32(body[48:], 1) // SMB2_0_IOCTL_IS_FSCTL
	}
	return append(body, in...)
}

// ioctlOutput is the output buffer of an IOCTL response.
func ioctlOutput(t *testing.T, out []byte) []byte {
	t.Helper()
	if len(out) < headerLen+48 {
		return nil
	}
	rb := out[headerLen:]
	if binary.LittleEndian.Uint16(rb[0:]) != 49 {
		return nil // an error response
	}
	off := int(binary.LittleEndian.Uint32(rb[32:]))
	n := int(binary.LittleEndian.Uint32(rb[36:]))
	if n == 0 {
		return []byte{}
	}
	if off+n > len(out) {
		t.Fatalf("output [%d, %d) is outside the %d-byte response", off, off+n, len(out))
	}
	return out[off : off+n]
}

func regionInput(off, length int64, usage uint32) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint64(b[0:], uint64(off))
	binary.LittleEndian.PutUint64(b[8:], uint64(length))
	binary.LittleEndian.PutUint32(b[16:], usage)
	return b
}

type region struct {
	off, n int64
	usage  uint32
}

const (
	usageCached    uint32 = 1 // FILE_REGION_USAGE_VALID_CACHED_DATA
	usageNoncached uint32 = 2 // FILE_REGION_USAGE_VALID_NONCACHED_DATA
	maxLongLong           = int64(^uint64(0) >> 1)
)

// FSCTL_QUERY_FILE_REGIONS answers as MS-FSA computes it from the file's valid data
// length and end of file, for every branch of its pseudocode.
// Invariant: I-227
func TestQueryFileRegions_FSA(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name     string
		vdl, eof int64
		in       []byte // nil: no FILE_REGION_INPUT
		maxOut   uint32
		status   uint32
		total    uint32
		regions  []region
		noOutput bool // success with zero output bytes
	}{
		// The probe Explorer sends: a file it has just created and sized, nothing written.
		{"created and sized, unwritten", 0, 5 * mib, regionInput(0, 5*mib, usageCached), 64,
			statusSuccess, 1, []region{{0, 5 * mib, 0}}, false},
		{"written whole", 5 * mib, 5 * mib, regionInput(0, 5*mib, usageCached), 64,
			statusSuccess, 1, []region{{0, 5 * mib, usageCached}}, false},
		{"written in part: valid then invalid", 3 * mib, 5 * mib, regionInput(0, 5*mib, usageCached), 64,
			statusSuccess, 2, []region{{0, 3 * mib, usageCached}, {3 * mib, 2 * mib, 0}}, false},
		{"the second region does not fit", 3 * mib, 5 * mib, regionInput(0, 5*mib, usageCached), 40,
			statusBufferOverflow, 2, []region{{0, 3 * mib, usageCached}}, false},
		{"asked range inside the valid data", 5 * mib, 5 * mib, regionInput(mib, mib, usageCached), 64,
			statusSuccess, 1, []region{{mib, mib, usageCached}}, false},
		{"asked range starts past the valid data", 2 * mib, 5 * mib, regionInput(3*mib, 4*mib, usageCached), 64,
			statusSuccess, 1, []region{{3 * mib, 2 * mib, 0}}, false},
		{"offset past the end of file", 5 * mib, 5 * mib, regionInput(6*mib, 1, usageCached), 64,
			statusSuccess, 0, nil, true},
		{"offset at a non-zero end of file", 5 * mib, 5 * mib, regionInput(5*mib, 1, usageCached), 64,
			statusSuccess, 0, nil, true},
		{"empty file", 0, 0, regionInput(0, 1, usageCached), 64,
			statusSuccess, 1, []region{{0, 0, 0}}, false},
		{"no input: the whole file, cached usage", 3 * mib, 5 * mib, nil, 64,
			statusSuccess, 2, []region{{0, 3 * mib, usageCached}, {3 * mib, 2 * mib, 0}}, false},
		{"both usage flags", 5 * mib, 5 * mib, regionInput(0, 5*mib, usageCached|usageNoncached), 64,
			statusSuccess, 1, []region{{0, 5 * mib, usageCached | usageNoncached}}, false},
		{"input shorter than FILE_REGION_INPUT", 0, 5 * mib, make([]byte, 23), 64,
			statusBufferTooSmall, 0, nil, false},
		{"output shorter than FILE_REGION_OUTPUT", 0, 5 * mib, regionInput(0, 1, usageCached), 39,
			statusBufferTooSmall, 0, nil, false},
		{"zero length", 0, 5 * mib, regionInput(0, 0, usageCached), 64,
			statusInvalidParameter, 0, nil, false},
		{"negative length", 0, 5 * mib, regionInput(0, -1, usageCached), 64,
			statusInvalidParameter, 0, nil, false},
		{"offset plus length past 63 bits", 0, 5 * mib, regionInput(1, maxLongLong, usageCached), 64,
			statusInvalidParameter, 0, nil, false},
		{"usage without the cached-data flag", 0, 5 * mib, regionInput(0, 1, usageNoncached), 64,
			statusInvalidParameter, 0, nil, false},
		{"no usage", 0, 5 * mib, regionInput(0, 1, 0), 64,
			statusInvalidParameter, 0, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, of := opened(t, &tinyFS{body: []byte("x")}, "/file.txt", false)
			of.f = &vdlFile{vdl: tc.vdl, eof: tc.eof}
			out := mustDispatch(t, c, cmdIoctl, ioctlReq(fsctlQueryFileRegions, of.id, tc.in, tc.maxOut, true))
			if st := statusOf(t, out); st != tc.status {
				t.Fatalf("status = %#x, want %#x", st, tc.status)
			}
			if tc.status != statusSuccess && tc.status != statusBufferOverflow {
				return
			}
			b := ioctlOutput(t, out)
			if b == nil {
				t.Fatal("no IOCTL response body")
			}
			if tc.noOutput {
				if len(b) != 0 {
					t.Fatalf("%d output bytes, want none", len(b))
				}
				return
			}
			if uint32(len(b)) > tc.maxOut {
				t.Fatalf("%d output bytes exceed MaxOutputResponse %d", len(b), tc.maxOut)
			}
			if len(b) < 16 {
				t.Fatalf("%d output bytes, want a FILE_REGION_OUTPUT", len(b))
			}
			if got := binary.LittleEndian.Uint32(b[4:]); got != tc.total {
				t.Errorf("TotalRegionEntryCount = %d, want %d", got, tc.total)
			}
			n := binary.LittleEndian.Uint32(b[8:])
			if int(n) != len(tc.regions) || len(b) != 16+24*len(tc.regions) {
				t.Fatalf("RegionEntryCount = %d with %d bytes, want %d regions", n, len(b), len(tc.regions))
			}
			for i, want := range tc.regions {
				r := b[16+24*i:]
				got := region{int64(binary.LittleEndian.Uint64(r[0:])), int64(binary.LittleEndian.Uint64(r[8:])), binary.LittleEndian.Uint32(r[16:])}
				if got != want {
					t.Errorf("region %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// IOCTL statuses follow MS-SMB2 §3.3.5.15: not an FSCTL, an FSCTL the server does not
// implement (or a file that cannot answer it), and an input buffer in the wrong place.
// Invariant: I-228
func TestIoctl_Statuses(t *testing.T) {
	const fsctlGetRetrievalPointers uint32 = 0x00090073
	c, of := opened(t, &tinyFS{body: []byte("x")}, "/file.txt", false)
	_, dirOf := opened(t, &tinyFS{body: []byte("x")}, "/", false)
	c.files[[16]byte{2}] = &openFile{path: "/", share: of.share, dir: true, id: [16]byte{2}}
	_ = dirOf

	misplaced := func(off uint32) []byte {
		b := ioctlReq(fsctlQueryFileRegions, of.id, regionInput(0, 1, usageCached), 64, true)
		binary.LittleEndian.PutUint32(b[24:], off)
		return b
	}
	cases := []struct {
		name string
		body []byte
		want uint32
	}{
		{"an IOCTL that is not an FSCTL", ioctlReq(fsctlQueryFileRegions, of.id, nil, 64, false), statusNotSupported},
		{"an FSCTL the server does not implement", ioctlReq(fsctlGetRetrievalPointers, of.id, nil, 64, true), statusInvalidDeviceRequest},
		{"QUERY_FILE_REGIONS on a file that cannot answer it", ioctlReq(fsctlQueryFileRegions, of.id, nil, 64, true), statusInvalidDeviceRequest},
		{"QUERY_FILE_REGIONS on a directory", ioctlReq(fsctlQueryFileRegions, [16]byte{2}, nil, 64, true), statusInvalidParameter},
		{"QUERY_FILE_REGIONS on a closed handle", ioctlReq(fsctlQueryFileRegions, [16]byte{9}, nil, 64, true), statusFileClosed},
		{"input offset inside the fixed header", misplaced(headerLen + 8), statusInvalidParameter},
		{"input offset not a multiple of 8", misplaced(headerLen + ioctlFixed + 1), statusInvalidParameter},
		{"input past the message", misplaced(headerLen + ioctlFixed + 8), statusInvalidParameter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if st := statusOf(t, mustDispatch(t, c, cmdIoctl, tc.body)); st != tc.want {
				t.Errorf("status = %#x, want %#x", st, tc.want)
			}
		})
	}
}

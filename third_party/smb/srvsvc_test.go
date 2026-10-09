// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"testing"
)

// The pipe, from the outside: the commands a client sends, in order.
//
// go-smb2 walks the same road in enumerate_test.go and is the judge that the
// bytes are right. These tests are about what it never does -- calling before
// binding, asking for an operation that is not there, sending something that
// is not DCE/RPC at all -- because a refusal nobody has seen is a guess.
func pipeOpen(t *testing.T, srv *Server, user string) (*conn, uint32, [16]byte) {
	t.Helper()
	c := newConn(srv, nil)
	c.sessions[0] = &session{user: user}

	n := utf16le(`\\127.0.0.1\IPC$`)
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[4:], uint16(headerLen+8))
	binary.LittleEndian.PutUint16(body[6:], uint16(len(n)))
	out, err := c.dispatch(request(cmdTreeConnect, 0, append(body, n...)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := parseHeader(out)
	tid := h.treeID

	name := utf16le("srvsvc")
	create := make([]byte, 56)
	binary.LittleEndian.PutUint16(create[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(create[46:], uint16(len(name)))
	out, err = c.dispatch(request(cmdCreate, tid, append(create, name...)))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("opening the pipe answered %#x", st)
	}
	var id [16]byte
	copy(id[:], out[headerLen+64:])
	return c, tid, id
}

// bindRequest and enumRequest are what a client sends, built here rather than
// borrowed, so a mistake in the server's own encoder cannot hide in both.
func bindRequest(callID uint32, maxRecv uint16) []byte {
	b := make([]byte, 72)
	b[0], b[1], b[2], b[3] = rpcVersion, rpcVersionMinor, rpcTypeBind, rpcFlagFirst|rpcFlagLast
	binary.LittleEndian.PutUint32(b[4:], rpcLittleEndian)
	binary.LittleEndian.PutUint16(b[8:], 72)
	binary.LittleEndian.PutUint32(b[12:], callID)
	binary.LittleEndian.PutUint16(b[16:], 4280)
	binary.LittleEndian.PutUint16(b[18:], maxRecv)
	binary.LittleEndian.PutUint32(b[24:], 1) // one context item
	binary.LittleEndian.PutUint16(b[30:], 1) // with one transfer syntax
	copy(b[52:], ndrSyntax[:])
	binary.LittleEndian.PutUint32(b[68:], 2)
	return b
}

func enumRequest(callID uint32, opnum uint16, level uint32, server string) []byte {
	stub := make([]byte, 4)
	if server == "" {
		binary.LittleEndian.PutUint32(stub[0:], 0) // a null pointer: no name
	} else {
		chars := utf16le(server)
		n := uint32(len(chars)/2 + 1)
		binary.LittleEndian.PutUint32(stub[0:], 0x00020000)
		stub = binary.LittleEndian.AppendUint32(stub, n)
		stub = binary.LittleEndian.AppendUint32(stub, 0)
		stub = binary.LittleEndian.AppendUint32(stub, n)
		stub = append(stub, chars...)
		stub = append(stub, 0, 0)
		for len(stub)%4 != 0 {
			stub = append(stub, 0)
		}
	}
	stub = binary.LittleEndian.AppendUint32(stub, level)
	b := make([]byte, 24)
	b[0], b[1], b[2], b[3] = rpcVersion, rpcVersionMinor, rpcTypeRequest, rpcFlagFirst|rpcFlagLast
	binary.LittleEndian.PutUint32(b[4:], rpcLittleEndian)
	binary.LittleEndian.PutUint16(b[8:], uint16(24+len(stub)))
	binary.LittleEndian.PutUint32(b[12:], callID)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(stub)))
	binary.LittleEndian.PutUint16(b[22:], opnum)
	return append(b, stub...)
}

// transceive is the one-round-trip form: the call goes in, the reply comes out.
func transceive(t *testing.T, c *conn, tid uint32, id [16]byte, in []byte, maxOut uint32) (uint32, []byte) {
	t.Helper()
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:], 57)
	binary.LittleEndian.PutUint32(body[4:], fsctlPipeTransceive)
	copy(body[8:], id[:])
	binary.LittleEndian.PutUint32(body[24:], uint32(headerLen+56))
	binary.LittleEndian.PutUint32(body[28:], uint32(len(in)))
	binary.LittleEndian.PutUint32(body[44:], maxOut)
	binary.LittleEndian.PutUint32(body[48:], 1) // IS_FSCTL
	out, err := c.dispatch(request(cmdIoctl, tid, append(body, in...)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := parseHeader(out)
	if h.status != statusSuccess && h.status != statusBufferOverflow {
		return h.status, nil
	}
	off := int(binary.LittleEndian.Uint32(out[headerLen+32:]))
	n := int(binary.LittleEndian.Uint32(out[headerLen+36:]))
	return h.status, out[off : off+n]
}

// writeRead is the other form, and the one Samba's client uses: WRITE the
// call, READ the reply.
func writeRead(t *testing.T, c *conn, tid uint32, id [16]byte, in []byte, length uint32) (uint32, []byte) {
	t.Helper()
	w := make([]byte, 48)
	binary.LittleEndian.PutUint16(w[2:], uint16(headerLen+48))
	binary.LittleEndian.PutUint32(w[4:], uint32(len(in)))
	copy(w[16:], id[:])
	out, err := c.dispatch(request(cmdWrite, tid, append(w, in...)))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("writing to the pipe answered %#x", st)
	}
	if n := binary.LittleEndian.Uint32(out[headerLen+4:]); int(n) != len(in) {
		t.Errorf("the pipe took %d of %d bytes", n, len(in))
	}
	r := make([]byte, 48)
	binary.LittleEndian.PutUint32(r[4:], length)
	copy(r[16:], id[:])
	out, err = c.dispatch(request(cmdRead, tid, r))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := parseHeader(out)
	if h.status != statusSuccess {
		return h.status, nil
	}
	off := int(out[headerLen+2])
	n := int(binary.LittleEndian.Uint32(out[headerLen+4:]))
	return h.status, out[off : off+n]
}

// shareNames decodes the answer, from the client's side of the specification.
// Level 0 is names only; level 1 adds the type and the comment.
func shareNames(t *testing.T, b []byte) []string {
	t.Helper()
	if len(b) < 48 {
		t.Fatalf("a reply of %d bytes cannot hold a share list", len(b))
	}
	level := binary.LittleEndian.Uint32(b[24:])
	count := int(binary.LittleEndian.Uint32(b[36:]))
	row := 4
	if level == 1 {
		row = 12
	}
	off := 48 + count*row
	names := make([]string, 0, count)
	for range count {
		strLen := func() string {
			n := int(binary.LittleEndian.Uint32(b[off+8:])) * 2
			s := fromUTF16le(b[off+12 : off+12+n-2]) // without the terminator
			off = (off + 12 + n + 3) &^ 3
			return s
		}
		names = append(names, strLen())
		if level == 1 {
			strLen() // the comment, which this decoder does not check
		}
	}
	return names
}

func TestThePipeAnswersInBothForms(t *testing.T) {
	srv := New()
	srv.AddUser("alice", "hunter2")
	for _, n := range []string{"disk", "attic"} {
		if err := srv.Share(n, nothingFS{}); err != nil {
			t.Fatal(err)
		}
	}

	for _, form := range []struct {
		name string
		call func(*testing.T, *conn, uint32, [16]byte, []byte, uint32) (uint32, []byte)
	}{
		{"transceive", transceive},
		{"write then read", writeRead},
	} {
		t.Run(form.name, func(t *testing.T) {
			c, tid, id := pipeOpen(t, srv, "alice")
			st, out := form.call(t, c, tid, id, bindRequest(7, 4280), 4280)
			if st != statusSuccess {
				t.Fatalf("the bind answered %#x", st)
			}
			if out[2] != rpcTypeBindAck || binary.LittleEndian.Uint32(out[12:]) != 7 {
				t.Fatalf("the bind acknowledgement is type %d call %d", out[2], binary.LittleEndian.Uint32(out[12:]))
			}
			// The client checks the fragment sizes it offered come back, and
			// they live at 16 -- inside the first twenty-four bytes, not after
			// them.
			if got := binary.LittleEndian.Uint16(out[16:]); got != 4280 {
				t.Errorf("the acknowledgement says the maximum transmit is %d", got)
			}

			for _, level := range []uint32{0, 1} {
				st, out = form.call(t, c, tid, id, enumRequest(8, opNetShareEnum, level, "127.0.0.1"), 4280)
				if st != statusSuccess {
					t.Fatalf("level %d answered %#x", level, st)
				}
				if out[2] != rpcTypeResponse {
					t.Fatalf("level %d came back as type %d", level, out[2])
				}
				got := shareNames(t, out)
				if len(got) != 3 || got[0] != "attic" || got[1] != "disk" || got[2] != "IPC$" {
					t.Errorf("level %d listed %q", level, got)
				}
			}
		})
	}
}

// Every refusal, by name. A client that gets no answer waits; one that gets a
// fault carries on.
func TestThePipeRefusesWhatItCannotDo(t *testing.T) {
	srv := New()
	srv.AddUser("alice", "hunter2")
	if err := srv.Share("disk", nothingFS{}); err != nil {
		t.Fatal(err)
	}

	t.Run("a call before a bind", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		_, out := transceive(t, c, tid, id, enumRequest(1, opNetShareEnum, 1, ""), 4280)
		if out[2] != rpcTypeFault || binary.LittleEndian.Uint32(out[24:]) != faultProtoErr {
			t.Errorf("type %d, status %#x", out[2], binary.LittleEndian.Uint32(out[24:]))
		}
	})

	t.Run("an operation that is not there", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		transceive(t, c, tid, id, bindRequest(1, 4280), 4280)
		_, out := transceive(t, c, tid, id, enumRequest(2, 16, 1, ""), 4280)
		if out[2] != rpcTypeFault || binary.LittleEndian.Uint32(out[24:]) != faultOpRange {
			t.Errorf("type %d, status %#x", out[2], binary.LittleEndian.Uint32(out[24:]))
		}
	})

	t.Run("a level nobody implements", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		transceive(t, c, tid, id, bindRequest(1, 4280), 4280)
		_, out := transceive(t, c, tid, id, enumRequest(3, opNetShareEnum, 502, ""), 4280)
		if out[2] != rpcTypeResponse {
			t.Fatalf("an unsupported level came back as type %d", out[2])
		}
		// WERR_INVALID_LEVEL is the LAST word of the stub, and the container
		// before it is null: a client reads the code, not the emptiness.
		if code := binary.LittleEndian.Uint32(out[len(out)-4:]); code != 124 {
			t.Errorf("the return code is %d, want 124 (invalid level)", code)
		}
		if count := binary.LittleEndian.Uint32(out[36:]); count != 0 {
			t.Errorf("an unsupported level came back with %d entries", count)
		}
	})

	t.Run("a request whose argument runs off the end", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		transceive(t, c, tid, id, bindRequest(1, 4280), 4280)
		bad := enumRequest(4, opNetShareEnum, 1, "127.0.0.1")
		binary.LittleEndian.PutUint32(bad[24+12:], 0xFFFF) // a count nothing backs
		_, out := transceive(t, c, tid, id, bad, 4280)
		if out[2] != rpcTypeFault {
			t.Errorf("a hostile length came back as type %d", out[2])
		}
	})

	t.Run("something that is not DCE/RPC", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		st, _ := transceive(t, c, tid, id, []byte("hello, is this a pipe?"), 4280)
		if st != statusInvalidParameter {
			t.Errorf("nonsense answered %#x, want INVALID_PARAMETER", st)
		}
		// A bind that can receive nothing is nonsense of the same kind.
		st, _ = transceive(t, c, tid, id, bindRequest(1, 0), 4280)
		if st != statusInvalidParameter {
			t.Errorf("a bind with no receive size answered %#x", st)
		}
	})

	t.Run("a read with nothing in flight", func(t *testing.T) {
		c, tid, id := pipeOpen(t, srv, "alice")
		r := make([]byte, 48)
		binary.LittleEndian.PutUint32(r[4:], 4280)
		copy(r[16:], id[:])
		out, err := c.dispatch(request(cmdRead, tid, r))
		if err != nil {
			t.Fatal(err)
		}
		if st := statusOf(t, out); st != statusEndOfFile {
			t.Errorf("reading an idle pipe answered %#x, want END_OF_FILE", st)
		}
	})

	t.Run("a control code on a handle that is not a pipe", func(t *testing.T) {
		c, of := opened(t, &tinyFS{body: []byte("hello")}, "/", false)
		st, _ := transceive(t, c, 1, of.id, bindRequest(1, 4280), 4280)
		if st != statusFileClosed {
			t.Errorf("transceive on a file answered %#x", st)
		}
	})
}

// The reply that does not fit, from the inside: the boundary itself.
//
// A read must never cross a fragment, because the client expects the next one
// to begin with a header. The arithmetic is checked here rather than only
// through a client, so a failure says WHICH byte.
func TestAReplyIsCutIntoFragments(t *testing.T) {
	stub := make([]byte, 300)
	for i := range stub {
		stub[i] = byte(i)
	}
	frags := rpcResponse(9, stub, 124) // 124 - 24 = 100 bytes of stub each
	if len(frags) != 3 {
		t.Fatalf("%d fragments, want 3", len(frags))
	}
	var got []byte
	for i, f := range frags {
		if len(f) != 124 {
			t.Errorf("fragment %d is %d bytes", i, len(f))
		}
		if n := binary.LittleEndian.Uint16(f[8:]); int(n) != len(f) {
			t.Errorf("fragment %d says it is %d bytes and is %d", i, n, len(f))
		}
		first, last := f[3]&rpcFlagFirst != 0, f[3]&rpcFlagLast != 0
		if first != (i == 0) || last != (i == len(frags)-1) {
			t.Errorf("fragment %d: first=%v last=%v", i, first, last)
		}
		got = append(got, f[24:]...)
	}
	if string(got) != string(stub) {
		t.Error("the fragments do not reassemble into the stub")
	}

	// A client that offers less than a header gets the default rather than an
	// endless stream of empty fragments.
	if n := len(rpcResponse(9, stub, 8)); n != 1 {
		t.Errorf("an impossible maximum produced %d fragments", n)
	}
	// An empty stub is still one fragment: a reply of nothing is a reply.
	if n := len(rpcResponse(9, nil, 4280)); n != 1 {
		t.Errorf("an empty stub produced %d fragments", n)
	}
}

// take hands over one fragment at a time and says whether more is waiting.
func TestTakeNeverCrossesAFragment(t *testing.T) {
	p := &pipe{out: [][]byte{[]byte("first"), []byte("second")}}
	if out, more := p.take(3); string(out) != "fir" || !more {
		t.Errorf("a partial take gave %q, more=%v", out, more)
	}
	if out, more := p.take(100); string(out) != "st" || !more {
		t.Errorf("the rest of a fragment gave %q, more=%v", out, more)
	}
	if out, more := p.take(-1); len(out) != 0 || !more {
		t.Errorf("a negative take gave %q, more=%v", out, more)
	}
	if out, more := p.take(100); string(out) != "second" || more {
		t.Errorf("the last fragment gave %q, more=%v", out, more)
	}
	if out, more := p.take(100); out != nil || more {
		t.Errorf("an empty pipe gave %q, more=%v", out, more)
	}
}

// The one argument that is read, and every shape it can arrive in.
//
// The count in a conformant string comes from the CLIENT, and the walk past it
// is arithmetic on lengths -- which is where a server reads somebody else's
// memory if it trusts them.
func TestFindingTheLevelInARequest(t *testing.T) {
	le := binary.LittleEndian
	full := enumRequest(1, opNetShareEnum, 3, "server")[24:]
	null := enumRequest(1, opNetShareEnum, 4, "")[24:]

	hostile := append([]byte(nil), full...)
	le.PutUint32(hostile[12:], 0x7FFFFFFF)

	shortName := append([]byte(nil), full[:12]...)

	for _, tc := range []struct {
		name  string
		stub  []byte
		want  uint32
		valid bool
	}{
		{"a name and a level", full, 3, true},
		{"a null name pointer", null, 4, true},
		{"nothing at all", nil, 0, false},
		{"a pointer and no string", shortName, 0, false},
		{"a count nothing backs", hostile, 0, false},
		{"a name but no level after it", full[:len(full)-4], 0, false},
	} {
		got, ok := netShareEnumLevel(tc.stub)
		if ok != tc.valid || got != tc.want {
			t.Errorf("%s: level %d, ok %v; want %d, %v", tc.name, got, ok, tc.want, tc.valid)
		}
	}
}

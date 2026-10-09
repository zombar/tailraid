// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb_test

import (
	"encoding/binary"
	"net"
	"runtime"
	"testing"
	"time"
)

// A frame header announcing a large message costs the server what has
// arrived, not what was announced. Sixteen connections that each announce
// 8 MiB and send nothing more used to make it allocate 128 MiB, before any
// authentication, for as long as they kept quiet.
func TestAnAnnouncedFrameCostsOnlyWhatArrives(t *testing.T) {
	const conns = 16
	addr, _ := serve(t)
	heap := func() int64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	before := heap()
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for range conns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 8<<20) // 8 MiB, then silence
		if _, err := c.Write(append(hdr[:], 0xFE, 'S', 'M', 'B')); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let every connection read its header
	if grown := heap() - before; grown > 16<<20 {
		t.Fatalf("%d connections that announced 8 MiB and sent 4 bytes cost %d MiB", conns, grown>>20)
	} else {
		t.Logf("%d announcing connections cost %d KiB", conns, grown>>10)
	}
}

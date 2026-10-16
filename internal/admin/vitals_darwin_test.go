//go:build darwin

package admin

import (
	"encoding/binary"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The offsets the macOS parser reads at are those of x/sys/unix's structs, and a
// synthesized interface list sums every interface but loopback and picks the tailnet one.
// Invariant: I-190
func TestVitals_IfList2Parser(t *testing.T) {
	if unsafe.Offsetof(unix.IfMsghdr2{}.Data) != ifm2Data || unsafe.Offsetof(unix.IfMsghdr2{}.Index) != ifm2Index || unsafe.Offsetof(unix.IfMsghdr2{}.Flags) != ifm2Flags ||
		unsafe.Offsetof(unix.IfData64{}.Ibytes) != ifd64In || unsafe.Offsetof(unix.IfData64{}.Obytes) != ifd64Out {
		t.Fatal("the parser's offsets do not match x/sys/unix")
	}
	msg := func(index uint16, flags int32, in, out uint64) []byte {
		b := make([]byte, ifm2Data+ifd64Out+8+4) // a trailing word, as a message carries sockaddrs
		binary.LittleEndian.PutUint16(b, uint16(len(b)))
		b[ifm2Type] = unix.RTM_IFINFO2
		binary.LittleEndian.PutUint32(b[ifm2Flags:], uint32(flags))
		binary.LittleEndian.PutUint16(b[ifm2Index:], index)
		binary.LittleEndian.PutUint64(b[ifm2Data+ifd64In:], in)
		binary.LittleEndian.PutUint64(b[ifm2Data+ifd64Out:], out)
		return b
	}
	other := make([]byte, 8) // an unrelated routing message
	binary.LittleEndian.PutUint16(other, 8)
	other[ifm2Type] = unix.RTM_IFINFO
	var list []byte
	list = append(list, msg(1, unix.IFF_LOOPBACK|unix.IFF_UP, 99, 99)...)
	list = append(list, other...)
	list = append(list, msg(4, unix.IFF_UP, 1000, 200)...)
	list = append(list, msg(12, unix.IFF_UP, 50, 7)...)
	rx, tx, trx, ttx, ok := sumIfs(ifList2(list, 12))
	if !ok || rx != 1050 || tx != 207 || trx != 50 || ttx != 7 {
		t.Fatalf("got %d %d %d %d %v", rx, tx, trx, ttx, ok)
	}
	if _, _, trx, ttx, _ := sumIfs(ifList2(list, -1)); trx != 0 || ttx != 0 {
		t.Fatal("no tailnet index matched something")
	}
	if _, _, _, _, ok := sumIfs(ifList2(nil, -1)); ok {
		t.Fatal("an empty list reported counters")
	}
	if _, _, _, _, ok := sumIfs(ifList2(list[:ifm2Data], -1)); ok {
		t.Fatal("a truncated message reported counters")
	}
	// the live list parses and reports something for this machine
	raw, err := syscall.RouteRIB(syscall.NET_RT_IFLIST2, 0) //nolint:staticcheck // as the probe
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := sumIfs(ifList2(raw, -1)); !ok {
		t.Fatal("the live interface list has no counters")
	}
}

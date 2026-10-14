package portmap

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DefaultGateway is the host's default IPv4 gateway and the local address that reaches
// it: the routing table on Linux (/proc/net/route), `route -n get default` on macOS and
// `route print 0.0.0.0` on Windows; the local address is what a socket toward the gateway
// binds (no packet is sent).
func DefaultGateway(ctx context.Context) (gw, local netip.Addr, err error) {
	switch runtime.GOOS {
	case "linux":
		gw, err = linuxGateway()
	case "darwin":
		gw, err = commandGateway(ctx, "route", []string{"-n", "get", "default"}, "gateway:")
	case "windows":
		gw, err = windowsGateway(ctx)
	default:
		err = fmt.Errorf("%w: unsupported OS %s", ErrNoGateway, runtime.GOOS)
	}
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	local, err = localToward(gw)
	return gw, local, err
}

// localToward is the local address a UDP socket toward addr would use.
func localToward(addr netip.Addr) (netip.Addr, error) {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	ap, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	return ap.Addr().Unmap(), nil
}

func linuxGateway() (netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %w", ErrNoGateway, err)
	}
	defer f.Close()
	return parseProcRoute(f)
}

// parseProcRoute reads /proc/net/route: the gateway of the first entry whose
// destination is 0.0.0.0 and whose flags carry RTF_GATEWAY; hex fields are
// little-endian.
func parseProcRoute(r interface{ Read([]byte) (int, error) }) (netip.Addr, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var flags int
		if _, err := fmt.Sscanf(f[3], "%x", &flags); err != nil || flags&2 == 0 { // RTF_GATEWAY
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], binary.LittleEndian.Uint32(raw))
		return netip.AddrFrom4(b), nil
	}
	return netip.Addr{}, ErrNoGateway
}

// commandGateway runs a route command and takes the address after the field named.
func commandGateway(ctx context.Context, prog string, args []string, field string) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, prog, args...).Output() //nolint:gosec // a fixed command
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %s: %w", ErrNoGateway, prog, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == field {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() {
				return a, nil
			}
		}
	}
	return netip.Addr{}, ErrNoGateway
}

// windowsGateway parses `route print 0.0.0.0`: the line whose first two fields are
// 0.0.0.0 names the gateway third.
func windowsGateway(ctx context.Context) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "route", "print", "0.0.0.0").Output()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: route print: %w", ErrNoGateway, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "0.0.0.0" && f[1] == "0.0.0.0" {
			if a, err := netip.ParseAddr(f[2]); err == nil && a.Is4() {
				return a, nil
			}
		}
	}
	return netip.Addr{}, errors.New("portmap: no default route in route print")
}

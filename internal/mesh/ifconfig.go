package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// Command is one step of giving the interface its address, MTU and route (docs/20 §1).
// An Optional step may fail (a route that already exists).
type Command struct {
	Args     []string
	Optional bool
}

// String prints the step as a shell line.
func (c Command) String() string { return strings.Join(c.Args, " ") }

// ErrUnsupportedOS is a GOOS without an interface command table.
var ErrUnsupportedOS = errors.New("mesh: unsupported OS")

// Commands is the per-OS table that configures ifname with addr (a host address with the
// subnet's bits) and mtu; lifted from tunnelmesh's internal/tun. Pure: the tests check
// the table, the commands are not run.
func Commands(goos, ifname string, addr netip.Prefix, mtu int) ([]Command, error) {
	if !addr.IsValid() || !addr.Addr().Is4() {
		return nil, fmt.Errorf("mesh: interface address %v is not an IPv4 prefix", addr)
	}
	ip := addr.Addr().String()
	mask := net4Mask(addr.Bits())
	subnet := addr.Masked().String()
	m := strconv.Itoa(mtu)
	switch goos {
	case "darwin":
		return []Command{
			{Args: []string{"ifconfig", ifname, "inet", ip, ip, "netmask", mask, "up"}},
			{Args: []string{"ifconfig", ifname, "mtu", m}},
			{Args: []string{"route", "-q", "-n", "delete", "-net", subnet}, Optional: true},
			{Args: []string{"route", "-q", "-n", "add", "-net", subnet, "-interface", ifname}},
		}, nil
	case "linux":
		return []Command{
			{Args: []string{"ip", "addr", "replace", addr.String(), "dev", ifname}},
			{Args: []string{"ip", "link", "set", ifname, "mtu", m, "up"}},
		}, nil
	case "windows":
		return []Command{
			{Args: []string{"netsh", "interface", "ip", "set", "address", "name=" + ifname, "static", ip, mask}},
			{Args: []string{"netsh", "interface", "ipv4", "set", "subinterface", ifname, "mtu=" + m, "store=active"}},
		}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedOS, goos)
}

func net4Mask(bits int) string {
	var m uint32
	if bits > 0 {
		m = ^uint32(0) << (32 - bits) //nolint:gosec // bits is 0..32
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
}

// Runner runs one command; the default is exec.
type Runner func(ctx context.Context, name string, args ...string) error

// ExecRunner runs commands with os/exec, keeping their output for the error.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // the per-OS table above is the only caller
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Apply runs the table in order; an Optional step's failure is skipped.
func Apply(ctx context.Context, cmds []Command, run Runner) error {
	for _, c := range cmds {
		if err := run(ctx, c.Args[0], c.Args[1:]...); err != nil && !c.Optional {
			return fmt.Errorf("mesh: configure interface: %w", err)
		}
	}
	return nil
}

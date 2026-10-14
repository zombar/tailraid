package mesh

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// Invariant: I-324
func TestCommands_PerOS(t *testing.T) {
	addr := netip.MustParsePrefix("100.70.3.2/24")
	cases := []struct {
		goos string
		want []string
	}{
		{"darwin", []string{
			"ifconfig utun4 inet 100.70.3.2 100.70.3.2 netmask 255.255.255.0 up",
			"ifconfig utun4 mtu 1280",
			"route -q -n delete -net 100.70.3.0/24",
			"route -q -n add -net 100.70.3.0/24 -interface utun4",
		}},
		{"linux", []string{
			"ip addr replace 100.70.3.2/24 dev utun4",
			"ip link set utun4 mtu 1280 up",
		}},
		{"windows", []string{
			"netsh interface ip set address name=utun4 static 100.70.3.2 255.255.255.0",
			"netsh interface ipv4 set subinterface utun4 mtu=1280 store=active",
		}},
	}
	for _, tc := range cases {
		cmds, err := Commands(tc.goos, "utun4", addr, MTU)
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		var got []string
		for _, c := range cmds {
			got = append(got, c.String())
		}
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got  %s\n want %s", tc.goos, strings.Join(got, "\n      "), strings.Join(tc.want, "\n      "))
		}
	}
	if _, err := Commands("plan9", "x", addr, MTU); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("plan9: %v", err)
	}
	if _, err := Commands("linux", "x", netip.MustParsePrefix("fd00::1/64"), MTU); err == nil {
		t.Error("an IPv6 address must be refused")
	}
}

// Invariant: I-324
func TestApply_OptionalStepsMayFail(t *testing.T) {
	cmds, _ := Commands("darwin", "utun9", netip.MustParsePrefix("100.70.3.2/24"), MTU)
	var ran []string
	run := func(_ context.Context, name string, args ...string) error {
		ran = append(ran, name+" "+args[0])
		if name == "route" && args[3] == "delete" {
			return errors.New("not in table")
		}
		return nil
	}
	if err := Apply(context.Background(), cmds, run); err != nil {
		t.Fatalf("an optional failure must be skipped: %v", err)
	}
	if len(ran) != 4 {
		t.Fatalf("ran %v", ran)
	}
	failing := func(_ context.Context, name string, _ ...string) error {
		if name == "ifconfig" {
			return errors.New("permission denied")
		}
		return nil
	}
	if err := Apply(context.Background(), cmds, failing); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a required failure must surface: %v", err)
	}
}

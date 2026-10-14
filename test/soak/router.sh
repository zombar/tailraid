#!/bin/sh
# NAT router of the rig (docs/20 §10 phase 27). Env: PUBLIC_NET (the prefix of the public
# network's addresses, "172.30.0."), NAT_MODE cone|symmetric, DROP_NETS (the other rig's
# private networks, space-separated: dropped on the way out, as the internet drops a
# private address, so two NATted nodes never meet by their own addresses). The public
# interface is the one holding an address of that network; Linux MASQUERADE keeps a
# source port across destinations when it can (an endpoint-independent mapping: a cone
# NAT), and --random-fully gives every flow a fresh port (a symmetric NAT).
set -eu
pub=$(ip -o -4 addr show | awk -v p="$PUBLIC_NET" 'index($4, p) == 1 { print $2; exit }')
[ -n "$pub" ] || { echo "no interface on $PUBLIC_NET" >&2; exit 1; }
# forwarding is the compose file's sysctl (the container's /proc/sys is read-only)
[ "$(cat /proc/sys/net/ipv4/ip_forward)" = 1 ] || { echo "ip_forward is off: set it in the compose file's sysctls" >&2; exit 1; }
for net in ${DROP_NETS:-}; do iptables -A FORWARD -d "$net" -j DROP; done
# a flow a mapping lets in (DNAT) leaves for the inner network from the router's own
# inner address: the Docker bridge drops a new flow whose source is off its subnet, as it
# dropped the forwarded probes of the mapping round before this; so the cone side sees a
# mapped-in peer at its router, as behind a home router that hairpins
inner=$(ip -o -4 addr show | awk -v p="$PUBLIC_NET" 'index($4, p) != 1 && $2 != "lo" { print $2; exit }')
iptables -t nat -A POSTROUTING -o "$inner" -m conntrack --ctstate DNAT -j MASQUERADE
case "${NAT_MODE:-cone}" in
  cone) iptables -t nat -A POSTROUTING -o "$pub" -j MASQUERADE ;;
  symmetric) iptables -t nat -A POSTROUTING -o "$pub" -j MASQUERADE --random-fully ;;
  *) echo "NAT_MODE must be cone or symmetric" >&2; exit 2 ;;
esac
echo "router up: $NAT_MODE NAT on $pub"
exec sleep infinity

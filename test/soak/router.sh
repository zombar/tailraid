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
case "${NAT_MODE:-cone}" in
  cone) iptables -t nat -A POSTROUTING -o "$pub" -j MASQUERADE ;;
  symmetric) iptables -t nat -A POSTROUTING -o "$pub" -j MASQUERADE --random-fully ;;
  *) echo "NAT_MODE must be cone or symmetric" >&2; exit 2 ;;
esac
echo "router up: $NAT_MODE NAT on $pub"
# the port-mapping responder (docs/20 §7): silent until /run/portmap.on exists, which
# phase 27's mapping round creates; /run/portmap.reset makes it forget every mapping
ext=$(ip -o -4 addr show dev "$pub" | awk '{ print $4 }' | cut -d/ -f1 | head -1)
exec soak-client natpmp 0.0.0.0:5351 "$ext" /run/portmap.on /run/portmap.reset

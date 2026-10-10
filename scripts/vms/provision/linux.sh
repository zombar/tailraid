#!/usr/bin/env bash
# Runs inside the Linux guest: register with the rig's headscale.
# usage: linux.sh <headscale url> <pre-auth key> <hostname>
set -euo pipefail
url=$1; key=$2; host=$3
sudo hostnamectl set-hostname "$host"
# the rig's CA: headscale and its DERP relay serve TLS from it (tailscaled reads the
# system roots at start, so it is restarted)
sudo install -m 644 /tmp/rig-ca.crt /usr/local/share/ca-certificates/rig-ca.crt
sudo update-ca-certificates >/dev/null
sudo systemctl enable tailscaled >/dev/null
sudo systemctl restart tailscaled
# --operator lets the admin user (which runs tailraid) use the CLI without sudo
sudo tailscale up --login-server "$url" --authkey "$key" --hostname "$host" --operator=admin --accept-dns=true --reset
for _ in $(seq 1 60); do
  if tailscale status --json 2>/dev/null | grep -q '"BackendState": *"Running"'; then
    echo "tailnet: $(tailscale ip -4) $(tailscale status --json | sed -n 's/.*"DNSName": *"\([^"]*\)".*/\1/p' | head -1)"
    exit 0
  fi
  sleep 1
done
echo "tailscale did not reach Running" >&2
tailscale status || true
exit 1

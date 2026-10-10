#!/usr/bin/env bash
# Runs inside the macOS guest: register with the rig's headscale.
# usage: mac.sh <headscale url> <pre-auth key> <hostname>
set -euo pipefail
url=$1; key=$2; host=$3
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
sudo scutil --set HostName "$host"
sudo scutil --set LocalHostName "$host"
sudo scutil --set ComputerName "$host"
# the rig's CA: headscale and its DERP relay serve TLS from it (tailscaled reads the
# system trust store at start, so it is restarted)
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain /tmp/rig-ca.crt
# the open-source daemon from Homebrew's formula, installed as a system daemon by the image
pgrep -x tailscaled >/dev/null || sudo tailscaled install-system-daemon
sudo launchctl kickstart -k system/com.tailscale.tailscaled
sleep 2
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

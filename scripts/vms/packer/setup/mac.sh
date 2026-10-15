#!/usr/bin/env bash
# Packer provisioner inside the macOS base image (docs/vms.md).
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
echo "$SSH_PUBLIC_KEY" >> "$HOME/.ssh/authorized_keys" && chmod 600 "$HOME/.ssh/authorized_keys"
sudo -n true || { echo "admin needs passwordless sudo"; exit 1; }

# the open-source tailscaled (Homebrew formula), as a system daemon under launchd
brew install --formula tailscale
sudo tailscaled install-system-daemon
# the VM never sleeps and keeps its GUI session (the LaunchAgent and the mount are per user)
sudo pmset -a sleep 0 displaysleep 0 disksleep 0
sudo scutil --set HostName tailraid-mac
sudo scutil --set LocalHostName tailraid-mac
sudo scutil --set ComputerName tailraid-mac
mkdir -p "$HOME/tailraid-d1" "$HOME/tailraid-d2"
tailscale version
echo "mac image ready"

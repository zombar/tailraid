#!/usr/bin/env bash
# headscale.sh up|down|destroy|key|url|nodes — the throwaway control server of the rig.
#
# headscale runs in Docker on this Mac and is reached at the host's address on the Tart
# guests' vmnet (common.sh headscale_url: derived from a running Tart guest, then pinned
# in test/vms/run/headscale/server_url), so nothing touches the real tailnet. Its state (the
# registered guests and their keys) lives in a named volume that `down` keeps and
# `destroy` removes, so a stopped rig comes back with every guest still registered.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

export HS_DIR="$RUN/headscale" # the generated config and the pinned URL; the compose file mounts from here
HS_COMPOSE="$RIG/headscale/compose.yml"
HS_USER=tr
hs() { docker compose -f "$HS_COMPOSE" exec -T headscale headscale "$@"; }
# user_id: headscale 0.26+ addresses users by numeric ID on the command line
user_id() { hs users list -o json | tr -d '\n\t ' | sed -n "s/.*\"id\":\"\{0,1\}\([0-9]*\)\"\{0,1\},\"name\":\"$HS_USER\".*/\1/p"; }

write_config() {
  mkdir -p "$HS_DIR"
  local url
  url=$(headscale_url)
  [ -n "$url" ] || die "no headscale URL yet: start a Tart guest first (vm.sh linux start), or set HEADSCALE_URL"
  if [ -f "$HS_DIR/server_url" ] && [ "$(cat "$HS_DIR/server_url")" != "$url" ]; then
    die "headscale was set up at $(cat "$HS_DIR/server_url") and the guests now sit behind $url: make vms-destroy, then vms-up"
  fi
  echo "$url" > "$HS_DIR/server_url"
  sed -e "s|@SERVER_URL@|$url|" "$RIG/headscale/config.yaml.in" > "$HS_DIR/config.yaml"
  write_tls "$url"
}

# write_tls <url>: the rig's CA (once: the guests trust it) and a server certificate for
# the URL's address (renewed when missing or within a month of expiry). Tailscale talks
# to a DERP relay over TLS only, so headscale serves TLS; the guests get ca.crt at
# provisioning (vm.sh) and trust it system-wide. RSA keys: Go rejects LibreSSL's EC
# encoding; 397 days: macOS refuses a longer-lived server certificate.
write_tls() {
  local host tls="$HS_DIR/tls"
  host=${1#https://}; host=${host%%:*}
  mkdir -p "$tls"
  if [ ! -f "$tls/ca.crt" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
      -keyout "$tls/ca.key" -out "$tls/ca.crt" -subj "/CN=tailraid rig CA" \
      -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
    log "rig CA at $tls/ca.crt"
  fi
  if [ -f "$tls/server.crt" ] && openssl x509 -in "$tls/server.crt" -noout -checkend 2592000 >/dev/null 2>&1; then
    return
  fi
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$tls/server.key" -out "$tls/server.csr" -subj "/CN=headscale" 2>/dev/null
  printf 'subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$host" > "$tls/server.ext"
  openssl x509 -req -in "$tls/server.csr" -CA "$tls/ca.crt" -CAkey "$tls/ca.key" -CAcreateserial \
    -out "$tls/server.crt" -days 397 -sha256 -extfile "$tls/server.ext" 2>/dev/null
  chmod 644 "$tls"/server.key "$tls"/server.crt # the container's headscale user reads them
  log "server certificate for $host"
}

up() {
  need docker
  write_config
  docker compose -f "$HS_COMPOSE" up -d --quiet-pull --force-recreate # a renewed certificate is read at start
  wait_for 60 "headscale /health" curl -fsSk -o /dev/null "https://127.0.0.1:$HS_PORT/health"
  hs users list -o json 2>/dev/null | grep -q "\"name\": *\"$HS_USER\"" || hs users create "$HS_USER" >/dev/null
  log "headscale up at $(headscale_url) (user $HS_USER)"
}

case "${1:-}" in
  up) up ;;
  down) docker compose -f "$HS_COMPOSE" down ;;
  destroy) docker compose -f "$HS_COMPOSE" down -v; rm -rf "$HS_DIR" ;;
  key) # a reusable pre-auth key, good for an hour: provisioning takes it, nothing stores it
    id=$(user_id)
    [ -n "$id" ] || die "headscale has no user $HS_USER (run headscale.sh up)"
    hs preauthkeys create --user "$id" --reusable --expiration 1h -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p'
    ;;
  url) headscale_url ;;
  ca) echo "$HS_DIR/tls/ca.crt" ;;
  nodes) hs nodes list ;;
  *) echo "usage: headscale.sh up|down|destroy|key|url|ca|nodes" >&2; exit 2 ;;
esac

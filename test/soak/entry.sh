#!/bin/sh
# Node entrypoint for the compose cluster. Env:
#   ROLE=init|join   SELF_IP, NODE_NAME, LEADER (ip for join), TOKEN, K, M, CAPACITY, VOTER
set -eu
mkdir -p "$TAILRAID_HOME"
# one log format for the whole container stream: shell messages and CLI output become
# JSON lines like the daemon's (already-JSON lines pass through)
jsonlog() {
  while IFS= read -r line; do
    case "$line" in
      "{"*) printf '%s\n' "$line" ;;
      *) printf '{"time":"%s","level":"INFO","msg":"%s","src":"entry"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(printf '%s' "$line" | sed 's/"/\\"/g')" ;;
    esac
  done
}
say() { printf '%s\n' "$*" | jsonlog; }
# per-node fake tailscale: this container is "self", the others are peers (docs/09 §soak)
cat > "$TAILRAID_HOME/ts.json" <<EOF
{
  "status": {
    "BackendState": "Running",
    "TailscaleIPs": ["$SELF_IP"],
    "MagicDNSSuffix": "compose.invalid",
    "Self": {"ID": "ts-$NODE_NAME", "HostName": "$NODE_NAME", "TailscaleIPs": ["$SELF_IP"], "Online": true},
    "Peer": {}
  },
  "whois": {
$(echo "$IDENTITIES" | tr ',' '\n' | while IFS='=' read -r ip id; do
    printf '    "%s": {"Node": {"StableID": "%s", "Name": "%s.compose.invalid.", "Addresses": ["%s/32"], "Hostinfo": {"Hostname": "%s", "OS": "linux"}}},\n' "$ip" "ts-$id" "$id" "$ip" "$id"
  done | sed '$ s/,$//')
  }
}
EOF
cat > "$TAILRAID_HOME/config.yaml" <<EOF
node:
  name: $NODE_NAME
  data_dir: $TAILRAID_HOME/data
  ${CAPACITY:+capacity: $CAPACITY}
  voter: ${VOTER:-true}
listen:
  raft: 20480
  rpc: 20481
  nfs: 127.0.0.1:20490
mount:
  path: /mnt/tailraid
repair:
  grace: ${REPAIR_GRACE:-10m}
  fill_after: ${FILL_AFTER:-10m}
dev:
  enabled: true
  tailscale_fake: $TAILRAID_HOME/ts.json
EOF
if [ ! -f "$TAILRAID_HOME/data/cluster_id" ]; then
  # CLI output goes through jsonlog without losing the exit status (no pipefail in ash)
  cli() { "$@" > "$TAILRAID_HOME/cli.log" 2>&1; st=$?; jsonlog < "$TAILRAID_HOME/cli.log"; return $st; }
  if [ "$ROLE" = init ]; then
    cli tailraid init --k "${K:-2}" --m "${M:-1}" --chunk "${CHUNK:-1M}" --token "$TOKEN"
  else
    until curl -fsS "http://$LEADER:20481/health" >/dev/null 2>&1; do say "waiting for $LEADER"; sleep 1; done
    until cli tailraid join "$LEADER" "$TOKEN" $( [ "${VOTER:-true}" = true ] && echo --voter ); do say "join retry"; sleep 2; done
  fi
fi
exec tailraid serve

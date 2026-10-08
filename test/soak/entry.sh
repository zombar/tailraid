#!/bin/sh
# Node entrypoint for the compose cluster. Env:
#   ROLE=init|join   SELF_IP, NODE_NAME, LEADER (ip for join), TOKEN, K, M, CAPACITY, VOTER
set -eu
mkdir -p "$TAILRAID_HOME"
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
  capacity: ${CAPACITY:-2G}
  voter: ${VOTER:-true}
listen:
  raft: 20480
  rpc: 20481
  nfs: 127.0.0.1:20490
mount:
  path: /mnt/tailraid
dev:
  enabled: true
  tailscale_fake: $TAILRAID_HOME/ts.json
EOF
if [ ! -f "$TAILRAID_HOME/data/cluster_id" ]; then
  if [ "$ROLE" = init ]; then
    tailraid init --k "${K:-2}" --m "${M:-1}" --chunk "${CHUNK:-1M}" --token "$TOKEN"
  else
    until curl -fsS "http://$LEADER:20481/health" >/dev/null 2>&1; do echo "waiting for $LEADER"; sleep 1; done
    until tailraid join "$LEADER" "$TOKEN" $( [ "${VOTER:-true}" = true ] && echo --voter ); do echo "join retry"; sleep 2; done
  fi
fi
exec tailraid serve

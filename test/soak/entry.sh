#!/bin/sh
# Node entrypoint for the compose cluster. Env:
#   ROLE=init|join|tryjoin   NODE_NAME, TOKEN (16 base32 characters), K, M, VOTER
#   CODE_FILE (join and tryjoin: the file holding the join code; /codes/join by default,
#   written by the init node from its own output)
#   DISKS (space-separated disks, docs/14, each `path` or `path=capacity`; empty for a
#   diskless host, docs/14 §9)
#   MIRRORS (space-separated mirror paths, docs/16: listed under node.mirrors on every
#   start, registered by `tailraid mirror add` from the driver), MIRRORS_EVERY
# A file $TAILRAID_HOME/rejoin holding a code makes the next start reset the node's Raft
# state and identity and join again by that code (phase 25).
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
cat > "$TAILRAID_HOME/config.yaml" <<EOF2
node:
  name: $NODE_NAME
  data_dir: $TAILRAID_HOME/data
  voter: ${VOTER:-true}
listen:
  raft: 20480
  rpc: 20481
  nfs: 127.0.0.1:20490
  smb: 127.0.0.1:20445
smb:
  password: ${SMB_PASSWORD:-soak-copies}
mount:
  path: /mnt/tailraid
repair:
  grace: ${REPAIR_GRACE:-10m}
  fill_after: ${FILL_AFTER:-10m}
stats:
  every: ${STATS_EVERY:-5s}
dev:
  enabled: true
EOF2
# init and join both register the disks the config lists (docs/14 §2, §9): a derived
# budget for a bare path, a pinned one for `path=capacity`
diskcfg() {
  echo "  disks:"
  for d in $DISKS; do
    case "$d" in
      *=*) printf '    - path: %s\n      capacity: "%s"\n' "${d%%=*}" "${d#*=}" ;;
      *) printf '    - path: %s\n' "$d" ;;
    esac
  done
}
if [ -n "${DISKS:-}" ] && [ ! -f "$TAILRAID_HOME/data/cluster_id" ]; then
  # insert under node: (after the voter line)
  diskcfg > "$TAILRAID_HOME/disks.yaml"
  sed -i "/^  voter:/r $TAILRAID_HOME/disks.yaml" "$TAILRAID_HOME/config.yaml"
fi
if [ -f "$TAILRAID_HOME/disks.saved.yaml" ]; then
  # a restart rewrites config.yaml from scratch: keep the disks added since
  sed -i "/^  voter:/r $TAILRAID_HOME/disks.saved.yaml" "$TAILRAID_HOME/config.yaml"
fi
if [ -n "${MIRRORS:-}" ]; then
  # a restart rewrites config.yaml from scratch: the mirrors are listed every time
  {
    echo "  mirrors:"
    for m in $MIRRORS; do
      mkdir -p "$m"
      printf '    - path: %s\n' "$m"
    done
  } > "$TAILRAID_HOME/mirrors.yaml"
  sed -i "/^  voter:/r $TAILRAID_HOME/mirrors.yaml" "$TAILRAID_HOME/config.yaml"
  printf 'mirrors:\n  every: %s\n' "${MIRRORS_EVERY:-1m}" >> "$TAILRAID_HOME/config.yaml"
fi
# CLI output goes through jsonlog without losing the exit status (no pipefail in ash)
cli() { "$@" > "$TAILRAID_HOME/cli.log" 2>&1; st=$?; jsonlog < "$TAILRAID_HOME/cli.log"; return $st; }
code_file=${CODE_FILE:-/codes/join}
wait_code() { # the code another container writes
  until [ -s "$code_file" ]; do say "waiting for the join code in $code_file"; sleep 1; done
}
voter=; [ "${VOTER:-true}" = true ] && voter=--voter
if [ "$ROLE" = tryjoin ]; then
  # one join attempt by a throwaway device: its output and exit status are the result
  wait_code
  cli tailraid join "$(cat "$code_file")"
  exit $?
fi
if [ -f "$TAILRAID_HOME/rejoin" ]; then
  # a reset device joining again under its key (docs/20 §4, phase 25)
  code=$(cat "$TAILRAID_HOME/rejoin"); rm -f "$TAILRAID_HOME/rejoin"
  cli tailraid reset --yes --discard-dirty
  if [ -n "${DISKS:-}" ]; then
    diskcfg > "$TAILRAID_HOME/disks.yaml"
    sed -i "/^  voter:/r $TAILRAID_HOME/disks.yaml" "$TAILRAID_HOME/config.yaml"
  fi
  until cli tailraid join "$code" ${voter:+"$voter"}; do say "join retry"; sleep 2; done
fi
if [ ! -f "$TAILRAID_HOME/data/cluster_id" ]; then
  if [ "$ROLE" = init ]; then
    cli tailraid init --k "${K:-2}" --m "${M:-1}" --chunk "${CHUNK:-1M}" --token "$TOKEN"
    # init prints the join code: the joiners read it from the shared volume
    mkdir -p "$(dirname "$code_file")"
    sed -n 's/^join from another device: tailraid join //p' "$TAILRAID_HOME/cli.log" > "$code_file.tmp"
    [ -s "$code_file.tmp" ] || { say "init printed no join code"; exit 1; }
    mv "$code_file.tmp" "$code_file"
  else
    wait_code
    until cli tailraid join "$(cat "$code_file")" ${voter:+"$voter"}; do say "join retry"; sleep 2; done
  fi
  if [ -n "${DISKS:-}" ]; then
    diskcfg > "$TAILRAID_HOME/disks.saved.yaml"
  fi
fi
exec tailraid serve

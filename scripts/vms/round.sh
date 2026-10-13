#!/usr/bin/env bash
# round.sh <mac|linux|win> — one round of the three-VM rig (docs/vms.md): the named OS
# runs `tailraid init`, the other two join by name, and the ten steps below exercise the
# cluster through the real service managers and OS mounts. FULL=1 runs the long verify.
#
# Every step appends a line to test/vms/results/<run>/round-<first>.jsonl and the
# summary goes to round-<first>.md; a failed step ends the round (teardown still runs).
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
S="$VMS_ROOT/scripts/vms"
C="$S/checks"

FIRST=${1:-}
case "$FIRST" in mac|linux|win|--live|--voters) ;; *) echo "usage: round.sh <mac|linux|win>" >&2; exit 2 ;; esac
RUN_ID=${RUN_ID:-$(date +%Y%m%d-%H%M%S)}
OUT="$STATE/results/$RUN_ID"
if [ "$FIRST" != --live ] && [ "$FIRST" != --voters ]; then
  mkdir -p "$OUT"
  RESULTS="$OUT/round-$FIRST.jsonl"; : > "$RESULTS"
  export FIRST RESULTS
fi
others=$(for os in $OSES; do [ "$os" = "$FIRST" ] || printf '%s ' "$os"; done)
NONVOTER=$(echo "$others" | awk '{ print $NF }') # the last joiner joins without --voter
TOKEN=vmtoken
failed=""
# what `status` must show once every guest is in: n devices and n disks live, and the
# automatic policy for n disks on n hosts (docs/14 §6: 2+1 for three, 1+1 for two)
N=$(echo "$OSES" | wc -w | tr -d ' ')
case "$N" in 3) EXPECT="3/3 3/3 policy 2+1 auto=true" ;; 2) EXPECT="2/2 2/2 policy 1+1 auto=true" ;; *) die "OSES needs two or three guests" ;; esac

step() { # <name> <function>: runs it, records the result, remembers a failure
  local name=$1 fn=$2 out rc
  log "== $name"
  out=$($fn 2>&1); rc=$?
  printf '%s\n' "$out" | sed 's/^/   /' >&2
  if [ $rc -eq 0 ]; then record "$name" ok "$(printf '%s' "$out" | tail -c 2000)"
  else record "$name" fail "$(printf '%s' "$out" | tail -c 4000)"; failed="$failed$name; "; fi
  return $rc
}

# ---- guest helpers (OS differences live here) ------------------------------------------
guest_home() { case "$1" in mac) echo "/Users/admin" ;; linux) echo "/home/admin" ;; win) printf '%s\n' 'C:\tailraid' ;; esac; }
mount_path() { case "$1" in win) echo "T:" ;; *) echo "$(guest_home "$1")/TailRAID" ;; esac; }
# tr <os> <args...>: a tailraid command in the guest
tr_() { vm_ssh "$1" "tailraid ${*:2}" | tr -d '\r'; }
# svc_restart <os>: through the service manager
svc_restart() { tr_ "$1" stop && tr_ "$1" start; }
# status_json <os>: the node's status, or nothing while it is not ready (exit 1)
status_json() { vm_ssh "$1" "tailraid status --json" 2>/dev/null | tr -d '\r'; }
ready() { status_json "$1" >/dev/null; }
# live_nodes <os>: "<devices live>/<devices> <disks live>/<disks> policy <k>+<m> auto=<bool>"
live_nodes() {
  status_json "$1" | python3 -c '
import json, sys
r = json.load(sys.stdin)
dev = [n for n in r["nodes"] if n["kind"] == "device" and n["state"] != "removed"]
disk = [n for n in r["nodes"] if n["kind"] == "disk" and n["state"] != "removed"]
v = r["volume"]
print("%d/%d %d/%d policy %d+%d auto=%s" % (sum(n["live"] for n in dev), len(dev), sum(n["live"] for n in disk), len(disk), v["k"], v["m"], str(v["auto"]).lower()))'
}
node_table() { # <os>: the member table as one canonical line per row (idx, id, kind, state, addr)
  status_json "$1" | python3 -c '
import json, sys
for n in sorted(json.load(sys.stdin)["nodes"], key=lambda n: n["idx"]):
    print(n["idx"], n["id"], n["kind"], n["state"], n["addr"], n["name"])'
}
node_id() { status_json "$1" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r["node"])'; }
# data <os> <op> <args...>: checks/data.* through the guest's mount
data() {
  local os=$1; shift
  case "$os" in
    win) vm_run_script win "$C/data.ps1" "T:" "$@" | tr -d '\r' ;;
    *) vm_run_script "$os" "$C/data.sh" "$(mount_path "$os")" "$@" ;;
  esac
}
wait_ready() { # <os> <seconds>
  wait_for "$2" "$1 ready" ready "$1"
}
ready_or_log() { # <os> <seconds>: like wait_ready, but a node that stays unready prints its log tail and returns 1
  local i=0
  until ready "$1"; do
    i=$((i + 1))
    if [ "$i" -ge "$2" ]; then
      echo "$1 not ready after ${2}s; its log tail:"
      tr_ "$1" logs -n 40 2>&1 | cut -c1-300 || true
      return 1
    fi
    sleep 1
  done
}
mount_() { # <os>: mount, and on Windows only check that the mapping works (it is per session)
  case "$1" in
    # the SMB redirector remembers a path it failed to reach (the slate step's umount
    # ran against no server) for a few seconds: error 53 right after the service came up
    # is retried
    win)
      # shellcheck disable=SC2016 # PowerShell, expanded in the guest
      vm_ssh win 'for ($i = 0; $i -lt 6; $i++) { tailraid mount; if (Test-Path T:\) { break }; Start-Sleep -Seconds 5 }; if (-not (Test-Path T:\)) { throw "T: not mapped" }; Get-ChildItem T:\ | Out-Null; tailraid umount' | tr -d '\r' ;;
    *) tr_ "$1" mount && vm_ssh "$1" "ls $(mount_path "$1") >/dev/null" ;;
  esac
}

# ---- the steps ---------------------------------------------------------------------------
s1_binaries() { "$S/binaries.sh"; }

s2_slate() {
  for os in $OSES; do
    case "$os" in
      win) vm_run_script win "$C/slate.ps1" | tr -d '\r' ;;
      *) vm_run_script "$os" "$C/slate.sh" "$os" ;;
    esac
  done
}

s3_init() {
  tr_ "$FIRST" init --token "$TOKEN"
  tr_ "$FIRST" install
  wait_ready "$FIRST" 90
  live_nodes "$FIRST"
  mount_ "$FIRST"
}

s4_join() {
  for os in $others; do
    # by name, through the tailnet (I-76). The last joiner is a non-voter until step 7b:
    # a restarted voter the leader refuses just wins an election and fills its own
    # table, so only a non-voter shows the fresh-follower defect step 6 is for
    local voter=--voter
    [ "$os" = "$NONVOTER" ] && voter=""
    tr_ "$os" join "tailraid-$FIRST" "$TOKEN" $voter
    tr_ "$os" install
    wait_ready "$os" 120
    mount_ "$os"
  done
}

# all_live <seconds>: every guest's own status shows EXPECT (devices and disks live, the
# policy) within the time; a guest that was just restarted takes a few probe rounds to
# see its peers live again, so one sample is no verdict
all_live() {
  local i=0 ok
  while :; do
    ok=1
    for os in $OSES; do [ "$(live_nodes "$os" 2>/dev/null)" = "$EXPECT" ] || ok=""; done
    [ -n "$ok" ] && return 0
    i=$((i + 5)); [ "$i" -lt "$1" ] || return 1
    sleep 5
  done
}

s5_membership() {
  local want rc=0
  all_live 120 || rc=1
  for os in $OSES; do echo "$os: $(live_nodes "$os")"; done
  want=$(node_table "$FIRST")
  for os in $others; do
    [ "$(node_table "$os")" = "$want" ] || { echo "$os's node table differs from $FIRST's:"; node_table "$os"; echo "--"; echo "$want"; rc=1; }
  done
  return $rc
}

# crash <os>: kill the service process outright; the service manager's restart policy
# (KeepAlive SuccessfulExit=false, Restart=always, the SCM failure actions) brings it back
crash() {
  case "$1" in
    win) vm_ssh win 'Stop-Process -Name tailraid -Force' | tr -d '\r' ;;
    *) vm_ssh "$1" "sudo pkill -9 -f 'tailraid serve'" ;;
  esac
}

s6_restart_after_join() {
  # a crash first (the #40 class: a fresh follower restarted before its first snapshot
  # has an empty table until the leader connects, and must still accept it), then a
  # clean stop && start of the non-voter (I-131)
  for os in $others; do
    echo "crashing $os"
    crash "$os"
  done
  for os in $others; do ready_or_log "$os" 90 || return 1; done
  echo "restarting $NONVOTER"
  svc_restart "$NONVOTER"
  for os in $OSES; do ready_or_log "$os" 90 || return 1; done
  local rc=0
  all_live 120 || rc=1
  for os in $OSES; do
    local l; l=$(live_nodes "$os"); echo "$os: $l"
    [ "$l" = "$EXPECT" ] || { echo "--- $os log tail"; tr_ "$os" logs -n 40 2>&1 | cut -c1-300 || true; }
  done
  return $rc
}

s7_data() {
  local hashes="$OUT/hashes-$FIRST.txt" # "name sha256" per file (macOS's bash 3.2 has no associative arrays)
  : > "$hashes"
  for os in $OSES; do
    data "$os" write "$os" "$RUN_ID" | tee -a "$hashes" | sed "s/^/$os wrote /"
  done
  sleep 5 # the writers' flushes
  for os in $OSES; do
    while read -r name h; do data "$os" read "$RUN_ID" "$name" "$h" | sed "s/^/$os reads /"; done < "$hashes"
  done
  # read-modify-write at a non-zero offset across gateways: the second guest rewrites
  # 64 KiB of the first's file at 1 MiB + 7 and the third (the first again, with two
  # guests) reads it back
  # shellcheck disable=SC2086 # OSES is a space-separated list
  set -- $OSES
  local a=$1 b=$2 c=${3:-$1}
  read -r _ h < <(data "$b" overwrite "$RUN_ID" "$a.bin")
  echo "$b overwrote $a.bin at 1 MiB + 7: $h"
  sleep 3
  data "$c" read "$RUN_ID" "$a.bin" "$h" | sed "s/^/$c reads /"
  data "$a" read "$RUN_ID" "$a.bin" "$h" | sed "s/^/$a reads /"
  data "$c" rename "$RUN_ID" "$b-small.txt" "$b-renamed.txt"
  sleep 2
  [ "$(data "$a" exists "$RUN_ID" "$b-renamed.txt")" = present ] || { echo "$a does not see the rename"; return 1; }
  [ "$(data "$b" exists "$RUN_ID" "$b-small.txt")" = absent ] || { echo "$b still sees the old name"; return 1; }
  data "$c" remove "$RUN_ID" "$b-renamed.txt"
  sleep 2
  [ "$(data "$a" exists "$RUN_ID" "$b-renamed.txt")" = absent ] || { echo "$a still sees the removed file"; return 1; }
  local m1 m2
  m1=$(data "$a" mtime "$RUN_ID" "$c.bin"); m2=$(data "$b" mtime "$RUN_ID" "$c.bin")
  [ "$m1" = "$m2" ] || { echo "mtime of $c.bin differs: $a $m1, $b $m2"; return 1; }
  echo "rename, remove and mtime agree across the three mounts"
}

# the non-voter becomes a voter: three voters, so verify's consensus phase can kill the
# leader with a quorum left and its partition phase has a minority to cut off
s7b_promote() {
  tr_ "$FIRST" node promote "tailraid-$NONVOTER"
  wait_for 60 "$N voters" sh -c "[ \"\$($S/round.sh --voters $FIRST)\" = $N ]" 2>/dev/null || true
  local v
  v=$(voters "$FIRST")
  echo "voters: $v"
  [ "$v" = "$N" ]
}
voters() { status_json "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["voters"])'; }

# verifier: the guest that runs verify, the OS after FIRST in the mac, linux, win cycle,
# so the leader (FIRST) is a target the consensus phase can kill, and over three rounds
# every OS drives the soak once through its own mount
verifier() {
  local prev="" first_os=""
  for os in $OSES; do
    [ -z "$first_os" ] && first_os=$os
    [ "$prev" = "$FIRST" ] && { echo "$os"; return; }
    prev=$os
  done
  echo "$first_os"
}

s8_verify() {
  local v flags="--destructive --restarts" rep="verify-$FIRST.html" res="verify-$FIRST.jsonl"
  v=$(verifier)
  [ "${FULL:-}" = 1 ] && flags="$flags --full"
  if [ "${SKIP_VERIFY:-}" = 1 ]; then echo "SKIP_VERIFY=1"; return 0; fi
  echo "verify from $v (first: $FIRST)"
  local rc=0
  case "$v" in
    win)
      vm_ssh win "tailraid mount | Out-Null; tailraid verify $flags --report C:\\tailraid\\$rep --results C:\\tailraid\\$res; \$rc = \$LASTEXITCODE; tailraid umount | Out-Null; exit \$rc" | tr -d '\r' || rc=$?
      vm_scp_from win "C:/tailraid/$rep" "$OUT/$rep"; vm_scp_from win "C:/tailraid/$res" "$OUT/$res"
      ;;
    *)
      vm_ssh "$v" "cd \$HOME && tailraid verify $flags --report $rep --results $res" || rc=$?
      vm_scp_from "$v" "$rep" "$OUT/$rep"; vm_scp_from "$v" "$res" "$OUT/$res"
      ;;
  esac
  # a capability skip other than the ones three guests cannot provide (parity kills m+1
  # hosts besides the gateway and wants one more holding slots, there is no diskless
  # host, the disk phase has one disk per host, the rig has no S3 bucket, and consensus
  # skips when leadership moved to the verifier) is a failure: every target runs
  # dev.enabled under a restarting manager, so the banner must be PASS
  grep -o '"kind":"phase"[^}]*"skipped":true[^}]*' "$OUT/$res" | grep -o '"phase":"[^"]*"' | sort -u > "$OUT/skipped-$FIRST.txt" || true
  echo "verify exit $rc; skipped phases: $(tr '\n' ' ' < "$OUT/skipped-$FIRST.txt")"
  while read -r p; do
    case "$p" in '"phase":"4 parity"'|'"phase":"13 disks"'|'"phase":"14 diskless host"'|'"phase":"15 s3 bucket"') ;;
      '"phase":"6 consensus"') echo "consensus skipped: the verifier was the leader" ;;
      *) echo "unexpected skip: $p"; rc=1 ;;
    esac
  done < "$OUT/skipped-$FIRST.txt"
  return $rc
}

# every node updates itself to the latest published release and back: the GitHub
# resolve, the minisign check, the swap, the service restart through the manager
# (I-61, I-128, I-131) and the rollback that puts the working-tree build back. The
# release is named explicitly with --allow-downgrade so the step runs whatever the
# working tree's version is; the rollback must land within the minute after the restart,
# after which serve forgets the previous binary.
s8b_update() {
  local tag dev
  tag=$(curl -fsS https://api.github.com/repos/zombar/tailraid/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
  [ -n "$tag" ] || { echo "could not resolve the latest release from GitHub"; return 1; }
  dev=$(tr_ "$FIRST" version)
  echo "latest release $tag; the guests run $dev"
  for os in $OSES; do
    tr_ "$os" update --yes --restart --version "$tag" --allow-downgrade || return 1
    local v; v=$(tr_ "$os" version)
    [ "$v" = "tailraid $tag" ] || { echo "$os runs \"$v\" after the update, want $tag"; return 1; }
    if ! ready_or_log "$os" 45; then
      # a release from before the volume's snapshot codec cannot start on it (docs/12
      # §6): the refusal is the designed outcome of a downgrade across a codec bump, and
      # the rollback below is the way back; anything else is a failure
      tr_ "$os" logs -n 40 2>/dev/null | grep -q "unsupported snapshot version" || return 1
      echo "$os: $tag refuses the volume's snapshot (its codec is newer than the release's, docs/12 §6); rolling back"
    fi
    # the rollback runs the release's own code: a release whose --restart cannot find
    # the service after the swap (v0.4.2 on Linux) leaves the restored binary in place
    # but the old image running, so the rig restarts it and says so
    tr_ "$os" update --rollback --restart || { echo "$os: the release's rollback --restart failed; restarting the restored binary"; svc_restart "$os" || return 1; }
    v=$(tr_ "$os" version)
    [ "$v" = "$dev" ] || { echo "$os runs \"$v\" after the rollback, want $dev"; return 1; }
    ready_or_log "$os" 45 || return 1
    echo "$os: updated to $tag and rolled back"
  done
  all_live 120 || { for os in $OSES; do echo "$os: $(live_nodes "$os")"; done; return 1; }
}

s9_disk_and_recover() {
  local home id
  home=$(guest_home "$FIRST")
  case "$FIRST" in
    win) tr_ win node add-disk "C:\\tailraid\\d2" --capacity 2G ;;
    *) tr_ "$FIRST" node add-disk "$home/tailraid-d2" --capacity 2G ;;
  esac
  local disks="$((N + 1))/$((N + 1))"
  wait_for 60 "the second disk live" sh -c "[ \"\$($S/round.sh --live $FIRST | cut -d' ' -f2)\" = $disks ]" 2>/dev/null || true
  live_nodes "$FIRST"
  [ "$(live_nodes "$FIRST" | cut -d' ' -f2)" = "$disks" ] || { echo "the added disk is not live"; return 1; }
  case "$FIRST" in
    win) tr_ win export "C:\\tailraid\\snap"; vm_ssh win 'Get-ChildItem -Recurse C:\tailraid\snap | Select-Object -ExpandProperty FullName' | tr -d '\r' ;;
    *) tr_ "$FIRST" export "$home/tailraid-snap"; vm_ssh "$FIRST" "ls -R $home/tailraid-snap" ;;
  esac
  id=$(node_id "$FIRST")
  tr_ "$FIRST" stop
  tr_ "$FIRST" recover --voters "$id"
  tr_ "$FIRST" start
  wait_ready "$FIRST" 90
  live_nodes "$FIRST"
  # a one-voter cluster whose other two nodes still run: every file whose shards survive
  # reads exactly (every file here: 2+1 over three disks)
  # shellcheck disable=SC2086 # OSES is a space-separated list
  set -- $OSES
  local a=$1
  [ "$FIRST" = "$a" ] && a=$2
  data "$FIRST" exists "$RUN_ID" "$a.bin" | grep -q present || { echo "$a.bin unreadable after recover"; return 1; }
  echo "recovered as a one-voter cluster; files still read"
}

# collect_logs: every guest's service log into the results directory, before the
# teardown wipes it; a failed step is diagnosed from there
collect_logs() {
  for os in $OSES; do
    case "$os" in
      mac) vm_scp_from mac "/Users/admin/Library/Logs/tailraid/serve.log" "$OUT/serve-$FIRST-$os.log" 2>/dev/null || true ;;
      linux) vm_ssh linux "sudo journalctl -u tailraid --no-pager -n 3000" > "$OUT/serve-$FIRST-$os.log" 2>/dev/null || true ;;
      win) vm_scp_from win "C:/ProgramData/tailraid/serve.log" "$OUT/serve-$FIRST-$os.log" 2>/dev/null || true ;;
    esac
  done
}

s10_teardown() {
  collect_logs
  for os in $OSES; do
    case "$os" in
      win)
        # shellcheck disable=SC2016 # PowerShell, expanded in the guest
        vm_ssh win 'tailraid umount 2>$null | Out-Null; tailraid uninstall; tailraid reset --yes --discard-dirty' | tr -d '\r' || true ;;
      *) vm_ssh "$os" "tailraid umount >/dev/null 2>&1; tailraid uninstall; tailraid reset --yes --discard-dirty" || true ;;
    esac
  done
  echo "torn down"
}

# --live <os> is an internal entry point for wait_for (a fresh shell cannot see functions)
if [ "$FIRST" = --live ]; then live_nodes "$2"; exit 0; fi
if [ "$FIRST" = --voters ]; then voters "$2"; exit 0; fi

log "round: FIRST=$FIRST, run $RUN_ID, results in $OUT"
step "1 binaries" s1_binaries &&
step "2 clean slate" s2_slate &&
step "3 init on $FIRST" s3_init &&
step "4 join by name" s4_join &&
step "5 membership" s5_membership &&
step "6 restart after join" s6_restart_after_join &&
step "7 cross-OS data" s7_data &&
step "7b promote" s7b_promote &&
step "8 verify" s8_verify &&
step "8b update --restart" s8b_update &&
step "9 add-disk, snapshot, recover" s9_disk_and_recover || true
step "10 teardown" s10_teardown || true

# the summary
{
  echo "# Round: first = $FIRST ($RUN_ID)"
  echo
  echo "| Step | Result | Detail |"
  echo "|---|---|---|"
  python3 - "$RESULTS" <<'EOF'
import json, sys
for line in open(sys.argv[1]):
    r = json.loads(line)
    detail = r["detail"].strip().splitlines()
    detail = (detail[-1] if detail else "").replace("|", "\\|")[:160]
    print("| %s | %s | %s |" % (r["step"], r["result"].upper(), detail))
EOF
} > "$OUT/round-$FIRST.md"
cat "$OUT/round-$FIRST.md"
[ -z "$failed" ] || die "round $FIRST failed: $failed"
log "round $FIRST passed"

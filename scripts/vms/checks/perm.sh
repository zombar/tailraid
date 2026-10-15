#!/usr/bin/env bash
# Runs inside a macOS or Linux guest through its own mount (docs/vms.md step 7c): what the
# mount's bound user may do under _vms/<run>/perm, where r carries a read grant, w a write
# grant and other none.
# usage: perm.sh <mount> setup <run>          makes perm/{r,w,other}/f (8 KiB each)
#        perm.sh <mount> probe <run>          one line: read-r= write-r= write-w= stat-other= list=
#        perm.sh <mount> read|write <run> <sub>   one probe of perm/<sub>/f: ok | refused | err:<text>
set -euo pipefail
mnt=$1; op=$2; run=$3; sub=${4:-}
root="$mnt/_vms/$run/perm"
# outcome <cmd...>: ok, refused (the OS's EACCES) or the error's last line
outcome() {
  local err rc=0
  err=$("$@" 2>&1 >/dev/null) || rc=$?
  if [ $rc -eq 0 ]; then echo ok
  elif printf '%s' "$err" | grep -qi 'permission denied\|not permitted\|access denied'; then echo refused
  else echo "err:$(printf '%s' "$err" | tail -1 | tr ' ' _)"; fi
}
rd() { cat "$1"; }
# a write at a non-zero offset: open for writing, 3 bytes at 4 KiB, no truncation
wr() { printf 'sam' | dd of="$1" bs=1 seek=4096 conv=notrunc status=none; }
st() { ls -ld "$1"; }
ls_() { find "$1" -mindepth 1 -maxdepth 1 -exec basename {} \; | sort | tr '\n' ' ' | sed 's/ $//'; }
case "$op" in
  setup)
    for d in r w other; do
      mkdir -p "$root/$d"
      head -c 8192 /dev/urandom > "$root/$d/f"
    done
    sync 2>/dev/null || true
    echo "setup ok"
    ;;
  probe)
    rr=$(outcome rd "$root/r/f"); wrr=$(outcome wr "$root/r/f"); ww=$(outcome wr "$root/w/f"); so=$(outcome st "$root/other/f")
    if l=$(ls_ "$root" 2>/dev/null); then :; else l=$(outcome ls_ "$root"); fi
    echo "read-r=$rr write-r=$wrr write-w=$ww stat-other=$so list=$l"
    ;;
  diag)
    for d in r w other; do
      l=$(find "$root/$d" -mindepth 1 -maxdepth 1 -exec basename {} \; 2>&1 | tr '\n' ' ')
      st=$(ls -ld "$root/$d/f" 2>&1)
      echo "diag $d: list=[$l] f: $st"
    done
    ;;
  read) outcome rd "$root/$sub/f" ;;
  write) outcome wr "$root/$sub/f" ;;
  *) echo "unknown op $op"; exit 2 ;;
esac

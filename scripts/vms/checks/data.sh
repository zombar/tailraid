#!/usr/bin/env bash
# Runs inside a macOS or Linux guest through its own mount (docs/vms.md step 7).
# usage: data.sh <mount> write <os> <run>      writes <os>.bin (3 MiB) and <os>-small.txt, prints sha256 of each
#        data.sh <mount> read <run> <name> <sha>  reads <name>, compares the hash
#        data.sh <mount> overwrite <run> <name>  overwrites 64 KiB at 1 MiB + 7, prints the new sha256
#        data.sh <mount> rename|remove|mtime <run> <name> [<new>|<epoch>]
set -euo pipefail
mnt=$1; op=$2; shift 2
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
case "$op" in
  write)
    os=$1; run=$2; dir="$mnt/_vms/$run"
    mkdir -p "$dir"
    head -c $((3 << 20)) /dev/urandom > "$dir/$os.bin"
    printf 'hello from %s at %s\n' "$os" "$(date -u +%FT%TZ)" > "$dir/$os-small.txt"
    sync "$dir/$os.bin" 2>/dev/null || sync
    echo "$os.bin $(sha "$dir/$os.bin")"
    echo "$os-small.txt $(sha "$dir/$os-small.txt")"
    ;;
  read)
    run=$1; name=$2; want=$3; f="$mnt/_vms/$run/$name"
    [ -f "$f" ] || { echo "missing $f"; exit 1; }
    got=$(sha "$f")
    [ "$got" = "$want" ] || { echo "$name: sha $got, want $want"; exit 1; }
    echo "$name ok"
    ;;
  overwrite)
    run=$1; name=$2; f="$mnt/_vms/$run/$name"
    head -c $((64 << 10)) /dev/urandom | dd of="$f" bs=1 seek=$(( (1 << 20) + 7 )) conv=notrunc status=none
    sync "$f" 2>/dev/null || sync
    echo "$name $(sha "$f")"
    ;;
  rename)
    run=$1; mv "$mnt/_vms/$run/$2" "$mnt/_vms/$run/$3"; echo "renamed $2 to $3"
    ;;
  remove)
    run=$1; rm "$mnt/_vms/$run/$2"; echo "removed $2"
    ;;
  exists)
    run=$1; [ -e "$mnt/_vms/$run/$2" ] && echo present || echo absent
    ;;
  mtime)
    run=$1; f="$mnt/_vms/$run/$2"
    if [ "$(uname)" = Darwin ]; then stat -f %m "$f"; else stat -c %Y "$f"; fi
    ;;
  *) echo "unknown op $op" >&2; exit 2 ;;
esac

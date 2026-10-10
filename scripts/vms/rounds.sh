#!/usr/bin/env bash
# rounds.sh [--keep-going] — three rounds, each OS first in turn (docs/vms.md), and a
# summary under docs/reports/vms/<date>/ with the verify reports beside it.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
S="$VMS_ROOT/scripts/vms"

keep=""
[ "${1:-}" = --keep-going ] && keep=1
export RUN_ID=${RUN_ID:-$(date +%Y%m%d-%H%M%S)}
OUT="$STATE/results/$RUN_ID"
REPORT="$VMS_ROOT/docs/reports/vms/${RUN_ID%%-*}"
mkdir -p "$OUT" "$REPORT"
version=$(cd "$VMS_ROOT" && git describe --tags --always --dirty)
status=0
: > "$OUT/outcomes" # "<first> PASS|FAIL" per round (macOS bash 3.2 has no associative arrays)
for first in ${ROUNDS:-mac linux win}; do
  if "$S/round.sh" "$first"; then echo "$first PASS" >> "$OUT/outcomes"; else echo "$first FAIL" >> "$OUT/outcomes"; status=1; [ -n "$keep" ] || break; fi
done
{
  echo "# Three-VM rounds: $RUN_ID ($version)"
  echo
  echo "| First | Outcome | Steps | Verify report |"
  echo "|---|---|---|---|"
  for first in mac linux win; do
    o=$(awk -v f="$first" '$1 == f { print $2 }' "$OUT/outcomes"); o=${o:-"not run"}
    steps=$(python3 -c '
import json, sys
try:
    rows = [json.loads(l) for l in open(sys.argv[1])]
except FileNotFoundError:
    rows = []
print(", ".join("%s %s" % (r["step"].split(" ")[0], r["result"]) for r in rows))' "$OUT/round-$first.jsonl")
    rep=""; [ -f "$OUT/verify-$first.html" ] && rep="[verify-$first.html](verify-$first.html)"
    echo "| $first | $o | $steps | $rep |"
  done
} > "$OUT/summary.md"
cp "$OUT"/*.md "$OUT"/*.html "$REPORT/" 2>/dev/null || true
cat "$OUT/summary.md"
echo "reports: $REPORT"
exit $status

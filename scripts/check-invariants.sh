#!/usr/bin/env bash
# check-invariants.sh — keep docs/*.md invariants + lifecycle cells and *_test.go citations in sync.
#
# Invariants (I-N):
#   definitions: lines "- **I-N** — text" under a "## Invariants" heading in docs/*.md
#   citations:   every I-N token on a "// Invariant:" comment line directly above a Test/Fuzz func
# Lifecycle cells (L-N):
#   definitions: table rows "| L-N | From | Event | To / Outcome | ... |" under a
#                "## Transition table" or "## Forbidden transitions" heading in docs/*.md
#   citations:   every L-N token on a "// Lifecycle:" or "// Regression: #id ..." comment line
#                directly above a Test/Fuzz func
#
# Fails if: a defined ID has no citation, a cited ID is undefined, or an ID is defined twice.
# Regenerates docs/invariants.md and docs/lifecycle.md; with --check, fails if either differs.
# Tolerates docs that do not exist yet (zero definitions is not an error).
#
# Usage: scripts/check-invariants.sh [--check]
# Byte-order collation everywhere (macOS sort is locale/case-aware by default), so the
# generated tables are identical on every platform.
export LC_ALL=C
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
MODE="${1:-}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0

# ---- definitions ------------------------------------------------------------------------
# idefs: "num<TAB>doc<TAB>text"      ldefs: "num<TAB>doc<TAB>from<TAB>event<TAB>to"
: > "$TMP/idefs"; : > "$TMP/ldefs"
for f in docs/[0-9][0-9]-*.md; do
  [ -f "$f" ] || continue
  awk -v doc="$f" -v idefs="$TMP/idefs" -v ldefs="$TMP/ldefs" '
    function trim(s) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", s); return s }
    function flush_def() { if (cur != "") { printf "%s\t%s\t%s\n", cur, doc, curtext >> idefs; cur = "" } }
    END { flush_def() }
    /^## / {
      inv = ($0 ~ /^## Invariants[[:space:]]*$/)
      lc  = ($0 ~ /^## (Transition table|Forbidden transitions)[[:space:]]*$/)
    }
    inv && match($0, /^- \*\*I-[0-9]+\*\* — /) {
      flush_def()
      line = $0; sub(/^- \*\*I-/, "", line)
      cur = line; sub(/\*\*.*/, "", cur)
      curtext = line; sub(/^[0-9]+\*\* — /, "", curtext)
      next
    }
    inv && cur != "" && /^  [^ ]/ { curtext = curtext " " trim($0); next }
    cur != "" { flush_def() }
    lc && /^\|/ {
      n = split($0, c, "|")
      # c[1] is empty (leading pipe); first cell is c[2]
      id = trim(c[2])
      if (id ~ /^L-[0-9]+$/) {
        sub(/^L-/, "", id)
        printf "%s\t%s\t%s\t%s\t%s\n", id, doc, trim(c[3]), trim(c[4]), trim(c[5]) >> ldefs
      }
    }' "$f"
done

# ---- citations: "num<TAB>TestName<TAB>file" ---------------------------------------------
: > "$TMP/icites"; : > "$TMP/lcites"
while IFS= read -r file; do
  awk -v file="$file" -v icites="$TMP/icites" -v lcites="$TMP/lcites" '
    function grab(s, re, arr,   n) {
      n = 0
      while (match(s, re)) { arr[++n] = substr(s, RSTART + 2, RLENGTH - 2); s = substr(s, RSTART + RLENGTH) }
      return n
    }
    /\/\/ Invariant:/ {
      s = $0; sub(/.*\/\/ Invariant:/, "", s)
      ni += grab(s, "I-[0-9]+", tmp); for (k in tmp) ip[++pi] = tmp[k]; delete tmp
      pending = 1; next
    }
    /\/\/ (Lifecycle:|Regression: #[A-Za-z0-9_-]+)/ {
      s = $0; sub(/.*\/\/ (Lifecycle:|Regression: #[A-Za-z0-9_-]+)/, "", s)
      nl += grab(s, "L-[0-9]+", tmp); for (k in tmp) lp[++pl] = tmp[k]; delete tmp
      pending = 1; next
    }
    pending && match($0, /^func (Test|Fuzz)[A-Za-z0-9_]+/) {
      name = substr($0, RSTART + 5, RLENGTH - 5)
      for (i = 1; i <= pi; i++) printf "%s\t%s\t%s\n", ip[i], name, file >> icites
      for (i = 1; i <= pl; i++) printf "%s\t%s\t%s\n", lp[i], name, file >> lcites
      pending = 0; pi = 0; pl = 0; delete ip; delete lp; next
    }
    pending && !/^[[:space:]]*\/\// { pending = 0; pi = 0; pl = 0; delete ip; delete lp }
  ' "$file"
done < <(find . -name '*_test.go' -not -path './vendor/*' -not -path './.git/*' | sort)

# ---- rule checks (shared) ---------------------------------------------------------------
check_set() { # prefix defs cites label
  local P="$1" DEFS="$2" CITES="$3" LABEL="$4"
  local dups missing undef num
  dups="$(cut -f1 "$DEFS" | sort -n | uniq -d || true)"
  if [ -n "$dups" ]; then
    echo "ERROR: $LABEL defined more than once:"
    for d in $dups; do awk -F'\t' -v n="$d" -v p="$P" '$1==n {print "  " p "-" $1 "  " $2}' "$DEFS"; done
    fail=1
  fi
  missing=""
  while IFS=$'\t' read -r num rest; do
    [ -n "$num" ] || continue
    if ! awk -F'\t' -v n="$num" '$1==n {f=1} END {exit !f}' "$CITES"; then
      missing="$missing $P-$num(${rest%%$'\t'*})"
    fi
  done < "$DEFS"
  if [ -n "$missing" ]; then
    echo "ERROR: $LABEL with no test citation:"
    for m in $missing; do echo "  $m"; done
    fail=1
  fi
  undef=""
  for num in $(cut -f1 "$CITES" | sort -n | uniq); do
    if ! awk -F'\t' -v n="$num" '$1==n {f=1} END {exit !f}' "$DEFS"; then undef="$undef $P-$num"; fi
  done
  if [ -n "$undef" ]; then
    echo "ERROR: tests cite $LABEL that no doc defines:"
    for u in $undef; do
      echo "  $u cited by:"; awk -F'\t' -v n="${u#*-}" '$1==n {print "    " $2 " (" $3 ")"}' "$CITES"
    done
    fail=1
  fi
}
check_set I "$TMP/idefs" "$TMP/icites" "invariants"
check_set L "$TMP/ldefs" "$TMP/lcites" "lifecycle cells"

tests_for() { # num cites
  local t
  t="$(awk -F'\t' -v n="$1" '$1==n {print $2}' "$2" | sort -u | sed 's/.*/`&`/' | paste -sd, - | sed 's/,/, /g')"
  [ -n "$t" ] && printf '%s' "$t" || printf '—'
}
esc() { printf '%s' "${1//|/\\|}"; }

# ---- regenerate docs/invariants.md ------------------------------------------------------
{
  echo "# Invariants"; echo
  echo "Generated by \`scripts/check-invariants.sh\`; do not edit by hand. Every invariant is"
  echo "defined in exactly one design doc and cited by at least one test via \`// Invariant: I-N\`."
  echo; echo "| Invariant | Doc | Statement | Tests |"; echo "|---|---|---|---|"
  sort -t$'\t' -k1,1n "$TMP/idefs" | while IFS=$'\t' read -r num doc text; do
    echo "| I-$num | [$(basename "$doc")]($(basename "$doc")) | $(esc "$text") | $(tests_for "$num" "$TMP/icites") |"
  done
} > "$TMP/invariants.md"

# ---- regenerate docs/lifecycle.md -------------------------------------------------------
{
  echo "# Chunk lifecycle cells"; echo
  echo "Generated by \`scripts/check-invariants.sh\`; do not edit by hand. Every cell is defined in"
  echo "exactly one design doc transition table and cited by at least one test via"
  echo "\`// Lifecycle: L-N\` or \`// Regression: #id L-N\`."
  echo; echo "| Cell | Doc | From | Event | To / Outcome | Tests |"; echo "|---|---|---|---|---|---|"
  sort -t$'\t' -k1,1n "$TMP/ldefs" | while IFS=$'\t' read -r num doc from event to; do
    echo "| L-$num | [$(basename "$doc")]($(basename "$doc")) | $(esc "$from") | $(esc "$event") | $(esc "$to") | $(tests_for "$num" "$TMP/lcites") |"
  done
} > "$TMP/lifecycle.md"

emit() { # tmpfile outfile
  if [ "$MODE" = "--check" ]; then
    if [ ! -f "$2" ] || ! diff -u "$2" "$1" > "$TMP/diff"; then
      echo "ERROR: $2 is out of date; run 'make invariants' and commit. Diff:"
      cat "$TMP/diff" 2>/dev/null || echo "  (file missing)"
      fail=1
    fi
  else
    cp "$1" "$2"
  fi
}
emit "$TMP/invariants.md" docs/invariants.md
emit "$TMP/lifecycle.md" docs/lifecycle.md

ni=$(wc -l < "$TMP/idefs" | tr -d ' '); nl=$(wc -l < "$TMP/ldefs" | tr -d ' ')
if [ "$fail" -ne 0 ]; then
  echo "check-invariants: FAILED ($ni invariants, $nl lifecycle cells defined)"
  exit 1
fi
echo "check-invariants: OK ($ni invariants, $nl lifecycle cells; all defined IDs cited, all citations defined)"

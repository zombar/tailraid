#!/usr/bin/env bash
# check-docs.sh — the lint for the prose: README.md, CLAUDE.md, docs/*.md (docs/09 §2).
#
# Fails if:
#   - a relative link points at a file that does not exist, or at a heading anchor that no
#     heading in the target file produces (GitHub's slug rule: lowercase, drop punctuation
#     except hyphens and spaces, spaces become hyphens);
#   - a design doc (docs/NN-*.md) is missing from the reading-order table in docs/README.md
#     or from the document map in docs/00-overview.md;
#   - prose mentions an invariant I-n or lifecycle cell L-n that no doc defines (the
#     generated docs/invariants.md and docs/lifecycle.md are the definition lists), except
#     on a line that says a number is intentionally unused;
#   - a code fence is left open;
#   - a heading text repeats within one file (the anchors would collide);
#   - README.md holds more bullet lists than README_MAX_LISTS (it is read top to bottom;
#     the design docs are exempt).
#
# No markdownlint: it is a node tool, and these are the rules that have bitten here.
export LC_ALL=C
set -uo pipefail
cd "$(dirname "$0")/.."

README_MAX_LISTS=2 # README.md is prose and tables; the two lists are the release steps and nothing else

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
errs="$TMP/errs"; : > "$errs"
err() { echo "docs: $*" >> "$errs"; }

files=(README.md CLAUDE.md docs/README.md docs/windows-checklist.md)
for f in docs/[0-9][0-9]-*.md; do files+=("$f"); done

slug() { # GitHub heading anchor (one per line: anchors_of feeds grep -x)
  printf '%s\n' "$1" | tr 'A-Z' 'a-z' | sed -E 's/[^a-z0-9 _-]//g; s/ /-/g'
}

anchors_of() { # file → one slug per line
  grep -E '^#{1,6} ' "$1" | sed -E 's/^#{1,6} +//; s/ +#+$//' | while IFS= read -r h; do slug "$h"; done
}

# ---- links and anchors -------------------------------------------------------------------
for f in "${files[@]}"; do
  dir=$(dirname "$f")
  awk '/^```/ { fence = !fence; next } !fence' "$f" | grep -oE '\]\([^)]+\)' | sed -E 's/^\]\(//; s/\)$//' | sort -u > "$TMP/links"
  while IFS= read -r target; do
    [ -n "$target" ] || continue
    case "$target" in
      http://*|https://*|mailto:*) continue ;;
    esac
    path=${target%%#*}
    anchor=""
    case "$target" in *#*) anchor=${target#*#} ;; esac
    if [ -n "$path" ]; then
      resolved="$dir/$path"
      [ -e "$resolved" ] || { err "$f: link target missing: $target"; continue; }
    else
      resolved=$f
    fi
    if [ -n "$anchor" ] && [[ "$resolved" == *.md ]]; then
      # grep -x without -q: under pipefail a -q that exits early would fail the pipeline on the match
      anchors_of "$resolved" | grep -x -- "$anchor" > /dev/null || err "$f: no heading for anchor: $target"
    fi
  done < "$TMP/links"
done

# ---- every design doc is listed ------------------------------------------------------------
for f in docs/[0-9][0-9]-*.md; do
  base=$(basename "$f")
  grep -q -- "$base" docs/README.md || err "docs/README.md does not list $base"
  [ "$base" = 00-overview.md ] || grep -q -- "$base" docs/00-overview.md || err "docs/00-overview.md's document map does not list $base"
done

# ---- invariant and cell mentions are defined ----------------------------------------------
{ grep -oE '^\| I-[0-9]+ ' docs/invariants.md; grep -oE '^\| L-[0-9]+ ' docs/lifecycle.md; } \
  | sed -E 's/^\| //; s/ $//' | sort -u > "$TMP/defined"
for f in "${files[@]}"; do
  grep -nE '\b[IL]-[0-9]+\b' "$f" | grep -v 'intentionally unused' > "$TMP/mentions" || true
  while IFS=: read -r line text; do
    for id in $(printf '%s' "$text" | grep -oE '\b[IL]-[0-9]+\b' | sort -u); do
      grep -qx -- "$id" "$TMP/defined" || err "$f:$line: $id is not defined in any doc"
    done
  done < "$TMP/mentions"
done

# ---- fences, headings, README lists --------------------------------------------------------
for f in "${files[@]}"; do
  n=$(grep -cE '^```' "$f")
  [ $((n % 2)) -eq 0 ] || err "$f: a code fence is left open"
  dup=$(grep -E '^#{1,6} ' "$f" | sed -E 's/^#{1,6} +//' | sort | uniq -d)
  [ -z "$dup" ] || err "$f: repeated heading(s): $(printf '%s' "$dup" | tr '\n' ';')"
done
lists=$(awk '
  /^```/ { fence = !fence; next }
  fence { inlist = 0; next }
  /^[ >]*([-*]|[0-9]+\.) / { if (!inlist) n++; inlist = 1; next }
  { inlist = 0 }
  END { print n + 0 }' README.md)
[ "$lists" -le "$README_MAX_LISTS" ] || err "README.md has $lists bullet lists; the limit is $README_MAX_LISTS (prose or a table instead)"

if [ -s "$errs" ]; then
  cat "$errs" >&2
  exit 1
fi
echo "docs: ok (${#files[@]} files)"

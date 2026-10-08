#!/usr/bin/env bash
# changelog.sh — CHANGELOG.md from the commit log, grouped by release tag.
#
#   scripts/changelog.sh gen [NEXT]   rewrite CHANGELOG.md; commits after the last tag go
#                                     under NEXT (e.g. v0.1.2) or "Unreleased"
#   scripts/changelog.sh notes TAG    print the section for TAG (release notes body)
#
# Entries are commit subjects. A "scope: subject" prefix sorts an entry into a group:
# docs → Documentation, ci/tests/test → Tooling, everything else → Changes. Milestone
# subjects ("M3: …") lead their section. Merge commits and "changelog:" commits are skipped.
set -euo pipefail
cd "$(dirname "$0")/.."
repo_url=$(git remote get-url origin 2>/dev/null | sed -E 's#^git@github.com:#https://github.com/#; s#\.git$##')

section() { # section TITLE RANGE DATE
  local title=$1 range=$2 date=$3
  local changes=() tooling=() docs=()
  while IFS=$'\t' read -r sha subject; do
    [ -z "$sha" ] && continue
    case "$subject" in
      changelog:*|Merge\ *) continue ;;
    esac
    local entry="- ${subject} ([${sha}](${repo_url}/commit/${sha}))"
    case "$subject" in
      docs:*|docs\ *|README*) docs+=("$entry") ;;
      ci:*|tests:*|test:*|Tidy*|tidy:*) tooling+=("$entry") ;;
      *) changes+=("$entry") ;;
    esac
  done < <(git log --no-merges --format='%h%x09%s' $range)
  if [ ${#changes[@]} -eq 0 ] && [ ${#tooling[@]} -eq 0 ] && [ ${#docs[@]} -eq 0 ]; then
    return
  fi
  echo "## ${title} (${date})"
  echo
  if [ ${#changes[@]} -gt 0 ]; then
    printf '### Changes\n\n'; printf '%s\n' "${changes[@]}"; echo
  fi
  if [ ${#docs[@]} -gt 0 ]; then
    printf '### Documentation\n\n'; printf '%s\n' "${docs[@]}"; echo
  fi
  if [ ${#tooling[@]} -gt 0 ]; then
    printf '### Tooling\n\n'; printf '%s\n' "${tooling[@]}"; echo
  fi
}

tags=$(git tag -l 'v*' --sort=-v:refname)
latest=$(echo "$tags" | head -1)

case "${1:-gen}" in
  gen)
    next=${2:-Unreleased}
    {
      echo "# Changelog"
      echo
      echo "Generated from the commit log by \`scripts/changelog.sh\`; do not edit by hand."
      echo
      if [ -n "$latest" ]; then
        section "$next" "${latest}..HEAD" "$(date -u +%Y-%m-%d)"
      else
        section "$next" "HEAD" "$(date -u +%Y-%m-%d)"
      fi
      prev=""
      for t in $tags; do
        d=$(git log -1 --format=%cs "$t")
        if [ -n "$prev" ]; then
          section "$prev" "${t}..${prev}" "$(git log -1 --format=%cs "$prev")"
        fi
        prev=$t
      done
      if [ -n "$prev" ]; then
        section "$prev" "$prev" "$(git log -1 --format=%cs "$prev")"
      fi
    } > CHANGELOG.md
    echo "wrote CHANGELOG.md"
    ;;
  notes)
    tag=${2:?tag}
    awk -v tag="$tag" '
      /^## / { on = ($2 == tag) ; if (on) { print; next } }
      on { print }
    ' CHANGELOG.md | sed '1d' | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
    ;;
  *)
    echo "usage: $0 gen [NEXT] | notes TAG" >&2; exit 2 ;;
esac

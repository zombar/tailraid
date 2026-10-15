#!/usr/bin/env bash
# ci-changes.sh — what a CI run must test, from the files it changes (.github/workflows/ci.yml).
#
#   scripts/ci-changes.sh EVENT REF SHA [BASE]
#
# EVENT is github.event_name and REF github.ref. BASE is the pull request's base sha, or
# github.event.before on a push to main; a tag diffs against its first parent instead.
# CI_LABELS holds a pull request's labels, space-separated. Prints, for $GITHUB_OUTPUT:
#
#   code=true|false   anything a test, a linter or the build reads changed
#   oses=[...]        the test matrix: Linux alone on a pull request unless it touches
#                     per-OS code or carries the full-ci label, all three otherwise
#   release=vX.Y.Z    a push to main that merges make tag's "changelog: vX.Y.Z" commit
#                     (as a merge's second parent or a squash) whose tag does not exist:
#                     the release job tags it and publishes (docs/12 §4); empty otherwise
#
# Only prose is "not code": Markdown at the root or under docs/ (but docs/invariants.md
# and docs/lifecycle.md, which tests read), docs/reports/ and LICENSE. A tag skips the
# tests only if its parent's push run on main passed, so the chain of tested commits holds;
# a changelog merge that releases holds to the same rule.
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1
event=$1 ref=$2 sha=$3 base=${4:-}

all='["ubuntu-latest","macos-latest","windows-latest"]'
linux='["ubuntu-latest"]'
release=''
out() { echo "code=$1"; echo "oses=$2"; echo "release=$release"; echo "ci-changes: $3" >&2; exit 0; }
known() { [ -n "$1" ] && [ "$1" != 0000000000000000000000000000000000000000 ] && git cat-file -e "$1^{commit}" 2>/dev/null; }

prose() {
  case "$1" in
    docs/invariants.md | docs/lifecycle.md) return 1 ;;
    docs/reports/* | docs/*.md | LICENSE) return 0 ;;
  esac
  [[ $1 != */* && $1 == *.md ]]
}

per_os() {
  case "$1" in
    *_windows.go | *_darwin.go | *_linux.go | *_unix.go | *_other.go) return 0 ;;
    internal/mount/* | internal/svc/* | internal/smbgw/* | third_party/* | test/mount/*) return 0 ;;
    go.mod | go.sum | Makefile | .github/workflows/*) return 0 ;;
    *.go) [ -f "$1" ] && grep -qE '^//go:build .*(windows|darwin|linux|unix)' "$1" ;;
    *) return 1 ;;
  esac
}

case "$event:$ref" in
  pull_request:*)
    known "$base" || out true "$all" "base $base unknown"
    base=$(git merge-base "$base" "$sha")
    ;;
  push:refs/heads/main)
    known "$base" || out true "$all" "no previous main commit"
    # make tag's changelog commit, merged (second parent) or squashed (the head itself)
    for subj in "$(git log -1 --format=%s "$sha^2" 2>/dev/null || true)" "$(git log -1 --format=%s "$sha")"; do
      if [[ $subj =~ ^changelog:\ (v[0-9]+\.[0-9]+\.[0-9]+)(\ \(#[0-9]+\))?$ ]]; then
        v=${BASH_REMATCH[1]}
        git rev-parse -q --verify "refs/tags/$v" >/dev/null || release=$v
        break
      fi
    done
    if [ -n "$release" ]; then
      concl=$(gh run list --workflow ci.yml --commit "$base" --event push --branch main \
        --json conclusion --jq '.[0].conclusion // ""')
      [ "$concl" = success ] || out true "$all" "release $release: parent $base has no green main run (${concl:-none})"
    fi
    ;;
  push:refs/tags/v*)
    git merge-base --is-ancestor "$sha" origin/main || out true "$all" "tag is not on main"
    base=$(git rev-parse "$sha^1")
    concl=$(gh run list --workflow ci.yml --commit "$base" --event push --branch main \
      --json conclusion --jq '.[0].conclusion // ""')
    [ "$concl" = success ] || out true "$all" "parent $base has no green main run (${concl:-none})"
    ;;
  *) out true "$all" "$event runs everything" ;;
esac

code=false os=false
while IFS= read -r f; do
  [ -z "$f" ] && continue
  prose "$f" || code=true
  per_os "$f" && os=true
done < <(git diff --name-only "$base" "$sha")
[[ " ${CI_LABELS:-} " == *" full-ci "* ]] && os=true
[ "$event" != pull_request ] && os=true

oses=$linux
[ "$os" = true ] && oses=$all
out "$code" "$oses" "base $base: code=$code os=$os${release:+ release=$release}"

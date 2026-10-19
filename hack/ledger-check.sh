#!/usr/bin/env bash
# Enforce the divergence ledger: every non-merge commit in upstream/main..HEAD must
# carry a `Divergence: <category>; <ADR>` trailer or be listed in .divergence-allow.
# `--render` prints the ledger table generated from git instead of checking.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
UPSTREAM_REF="${UPSTREAM_REMOTE:-upstream}/${UPSTREAM_BRANCH:-main}"
ALLOW=.divergence-allow
RENDER=0; [ "${1:-}" = "--render" ] && RENDER=1

allowed() {  # prints "category adr" if the sha is grandfathered
  [ -f "$ALLOW" ] || return 1
  awk -v s="$1" '!/^#/ && NF>=3 && length($1)>=7 && index(s,$1)==1 {print $2, $3; found=1; exit} END{exit !found}' "$ALLOW"
}

if [ "$RENDER" -eq 1 ]; then
  echo "| # | Patch | Title | Category | ADR |"
  echo "|---|---|---|---|---|"
fi
n=0; bad=0
while read -r sha; do
  n=$((n+1))
  trailer=$(git log -1 --format='%(trailers:key=Divergence,valueonly)' "$sha" | sed '/^$/d' | head -1)
  if [ -n "$trailer" ]; then
    cat=$(sed -E 's/^[[:space:]]*([a-z]+).*/\1/' <<<"$trailer")
    adr=$(sed -E 's/^[^;]*;[[:space:]]*//' <<<"$trailer")
  elif a=$(allowed "$sha"); then
    cat=${a%% *}; adr=${a#* }
  else
    echo "UNLEDGERED: $(git log -1 --format='%h %s' "$sha")" >&2; bad=$((bad+1)); continue
  fi
  case "$cat" in
    tooling|local|revert) ;;
    carry) echo "CARRY NOT ALLOWED (scope rule; use local): ${sha:0:7}" >&2; bad=$((bad+1)) ;;
    *)     echo "BAD CATEGORY '$cat': ${sha:0:7}" >&2; bad=$((bad+1)) ;;
  esac
  if [ "$RENDER" -eq 1 ]; then
    echo "| $n | \`${sha:0:7}\` | $(git log -1 --format=%s "$sha") | $cat | $adr |"
  fi
done < <(git rev-list --no-merges --reverse "$UPSTREAM_REF..HEAD")

[ "$RENDER" -eq 1 ] && exit 0
echo "ledger-check: $n fork commit(s), $bad problem(s)"
[ "$bad" -eq 0 ]

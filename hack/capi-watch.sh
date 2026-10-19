#!/usr/bin/env bash
# Contract-sync sensor: compare this repo's sigs.k8s.io/cluster-api version with the
# latest CAPI release and with the sibling fork. Opens/dedupes a `capi-lag` issue
# unless --no-issue. Writes _out/capi-watch.json. Never changes code.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
mkdir -p _out
OPEN_ISSUE=1; [ "${1:-}" = "--no-issue" ] && OPEN_ISSUE=0

minor() { sed -E 's/^v([0-9]+)\.([0-9]+)\..*/\1.\2/' <<<"$1"; }
capi_in() { awk '{for(i=1;i<NF;i++) if($i=="sigs.k8s.io/cluster-api"){print $(i+1); exit}}'; }

CURRENT=$(go list -m -f '{{.Version}}' sigs.k8s.io/cluster-api)
LATEST=$(gh api repos/kubernetes-sigs/cluster-api/releases/latest -q .tag_name)
SIBLING=""
if [ -n "${SIBLING_REPO:-}" ]; then
  SIBLING=$(gh api "repos/$SIBLING_REPO/contents/go.mod" -H 'Accept: application/vnd.github.raw' | capi_in || true)
fi

LAG=false;      [ "$(minor "$CURRENT")" != "$(minor "$LATEST")" ] && LAG=true
MISMATCH=false; [ -n "$SIBLING" ] && [ "$(minor "$CURRENT")" != "$(minor "$SIBLING")" ] && MISMATCH=true

jq -n --arg repo "$(basename "$PWD")" --arg date "$(date -u +%FT%TZ)" \
  --arg capi_current "$CURRENT" --arg capi_latest "$LATEST" \
  --arg sibling_repo "${SIBLING_REPO:-}" --arg capi_sibling "$SIBLING" \
  --argjson minor_lag "$LAG" --argjson sibling_mismatch "$MISMATCH" \
  '{kind:"capi-watch", phase:"0"} + $ARGS.named' > _out/capi-watch.json
cat _out/capi-watch.json

[ "$OPEN_ISSUE" -eq 1 ] || exit 0
if [ "$LAG" = true ] || [ "$MISMATCH" = true ]; then
  TITLE="CAPI drift: $(minor "$CURRENT") here, $(minor "$LATEST") latest${SIBLING:+, $(minor "$SIBLING") in sibling}"
  if gh issue list --state open --label capi-lag --json title -q '.[].title' | grep -Fxq "$TITLE"; then
    echo "issue already open: $TITLE"; exit 0
  fi
  {
    echo "Contract-sync sensor (Phase 0, informational)."
    echo; echo "- this repo: \`$CURRENT\`"; echo "- latest CAPI release: \`$LATEST\`"
    [ -n "$SIBLING" ] && echo "- sibling \`$SIBLING_REPO\`: \`$SIBLING\`"
    echo; echo "A CAPI minor bump is a dependency + contract migration, not a git merge."
    echo "Bump proposals arrive in Phase 2. CACPPT and CABPT must move to the same minor together."
  } > _out/capi-issue.md
  gh issue create --title "$TITLE" --body-file _out/capi-issue.md --label capi-lag
fi

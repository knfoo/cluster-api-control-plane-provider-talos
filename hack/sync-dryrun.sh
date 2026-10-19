#!/usr/bin/env bash
# Dry-run provider sync: merge SYNC_TARGET (default upstream/main) into BASE_REF on a
# throwaway branch, then open a draft PR (clean), an issue (conflicts), or record a
# no-op. NEVER merges into main, never promotes. Phase 0/1 tool only.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# gh defaults to a remote named `upstream`; pin it to our own repo so no PR or issue
# can ever be opened upstream (scope rule).
export GH_REPO="${GH_REPO:-${GITHUB_REPOSITORY:-$(git remote get-url origin | sed -E 's#^.*github\.com[:/]##; s#\.git$##')}}"

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
BASE_BRANCH="${BASE_BRANCH:-main}"
BASE_REF="${BASE_REF:-origin/$BASE_BRANCH}"       # override only for the forced day-4 exercise
SYNC_TARGET="${SYNC_TARGET:-$UPSTREAM_REMOTE/main}"
OUT=_out/sync; mkdir -p "$OUT"

# identity via env only, never `git config`, so a local run can't rewrite your clone
export GIT_AUTHOR_NAME=sync-bot GIT_COMMITTER_NAME=sync-bot
export GIT_AUTHOR_EMAIL=sync-bot@users.noreply.github.com GIT_COMMITTER_EMAIL=sync-bot@users.noreply.github.com

ORIG=$(git symbolic-ref -q --short HEAD || git rev-parse HEAD)
trap 'git merge --abort >/dev/null 2>&1 || true; git checkout -q "$ORIG" 2>/dev/null || true' EXIT

git fetch origin --prune
git fetch "$UPSTREAM_REMOTE" --prune --no-tags \
  "+refs/heads/*:refs/remotes/$UPSTREAM_REMOTE/*" "+refs/tags/*:refs/tags/$UPSTREAM_REMOTE/*"

TARGET_SHA=$(git rev-parse --verify "$SYNC_TARGET^{commit}")
BASE_SHA=$(git rev-parse --verify "$BASE_REF^{commit}")
SHORT=${TARGET_SHA:0:12}
BRANCH="sync/$SHORT"
UPSTREAM_COMMITS=$(git rev-list --count "$BASE_SHA..$TARGET_SHA")
capi_at() { git show "$1:go.mod" | awk '{for(i=1;i<NF;i++) if($i=="sigs.k8s.io/cluster-api"){print $(i+1); exit}}'; }

outcome() {  # $1=result, $2=newline-separated conflict files, $3=url
  local touched workflows
  touched=$(git diff --name-only "$BASE_SHA" "$TARGET_SHA" | jq -R . | jq -s .)
  workflows=$(git diff --name-only "$BASE_SHA" "$TARGET_SHA" -- .github/workflows | jq -R . | jq -s .)
  jq -n --arg repo "$(basename "$PWD")" --arg date "$(date -u +%FT%TZ)" \
    --arg base_ref "$BASE_REF" --arg base_sha "$BASE_SHA" \
    --arg target_ref "$SYNC_TARGET" --arg target_sha "$TARGET_SHA" \
    --argjson upstream_commits "$UPSTREAM_COMMITS" --arg result "$1" --arg url "${3:-}" \
    --arg capi_before "$(capi_at "$BASE_SHA")" --arg capi_after "$(capi_at "$TARGET_SHA")" \
    --argjson conflict_files "$(printf '%s' "${2:-}" | jq -R . | jq -s 'map(select(length>0))')" \
    --argjson files_touched "$touched" --argjson workflow_files_touched "$workflows" \
    '{kind:"sync-dryrun", phase:"0"} + $ARGS.named' > "$OUT/outcome.json"
  cat "$OUT/outcome.json"
}

# 1. no-op: target already contained in base (the tag-based v1 script crashed here)
if git merge-base --is-ancestor "$TARGET_SHA" "$BASE_SHA"; then
  echo "$BASE_REF already contains $SYNC_TARGET ($SHORT); nothing to sync"
  outcome noop; exit 0
fi

# 2. dedupe on upstream SHA, not on date
if [ -n "$(gh pr list --state open --head "$BRANCH" --json number -q '.[].number')" ] || \
   [ -n "$(gh issue list --state open --label sync-dryrun --search "$SHORT in:title" --json number -q '.[].number')" ]; then
  echo "open PR/issue already exists for $SHORT; nothing to do"
  outcome duplicate; exit 0
fi

# 3. trial merge
git checkout -q -B "$BRANCH" "$BASE_SHA"
set +e
git merge --no-ff --no-commit "$TARGET_SHA" > "$OUT/merge.log" 2>&1
RC=$?
set -e

if [ "$RC" -ne 0 ]; then
  CONFLICTS=$(git diff --name-only --diff-filter=U)
  git merge --abort
  git checkout -q "$ORIG"; git branch -D "$BRANCH"
  if [ -z "$CONFLICTS" ]; then
    echo "merge failed for a non-conflict reason:" >&2; cat "$OUT/merge.log" >&2; exit 1
  fi
  {
    echo "Dry-run sync of \`$SYNC_TARGET\` (\`$SHORT\`, $UPSTREAM_COMMITS upstream commits) hit conflicts."
    echo "Expected until AVO (Phase 3). Conflicted files:"
    echo; echo '```'; echo "$CONFLICTS"; echo '```'
    echo; echo "If a conflicted file is covered by a \`local\` ADR, apply that ADR's revisit rule."
    echo "Informational only; no branch was created."
  } > "$OUT/body.md"
  URL=$(gh issue create --title "sync dry-run conflicts: $SHORT" --body-file "$OUT/body.md" --label sync-dryrun)
  outcome conflicts "$CONFLICTS" "$URL"; exit 0
fi

git commit -q -m "sync: merge $SYNC_TARGET ($SHORT) (dry-run)"
{
  echo "Dry-run sync of \`$SYNC_TARGET\` (\`$SHORT\`, $UPSTREAM_COMMITS upstream commits): clean merge."
  echo; echo '```'; git diff --stat "$BASE_SHA"..HEAD; echo '```'
  echo; echo "CAPI: \`$(capi_at "$BASE_SHA")\` → \`$(capi_at "$TARGET_SHA")\`"
  echo; echo "**DO NOT MERGE.** Phase 0/1 dry run. Promotion gates arrive in Phase 2."
} > "$OUT/body.md"
git push -q origin "$BRANCH"
URL=$(gh pr create --draft --title "sync dry-run: $SHORT" --body-file "$OUT/body.md" \
  --label sync-dryrun --base "$BASE_BRANCH" --head "$BRANCH")
outcome clean "" "$URL"

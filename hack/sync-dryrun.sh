#!/usr/bin/env bash
# Nightly dry-run sync: merge the latest upstream tag and open a PR (clean) or
# an issue (conflicts). NEVER merges, never promotes. Phase 0/1 tool only.
set -euo pipefail

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
BASE_BRANCH="${BASE_BRANCH:-main}"
BODY=/tmp/sync-dryrun-body.md

git fetch "$UPSTREAM_REMOTE" --tags --prune
UPSTREAM_TAG=$(git -c versionsort.suffix=- tag --sort=-v:refname --list 'v*' | head -1)
BRANCH="sync/$(date +%F)-${UPSTREAM_TAG}"

if git ls-remote --heads origin "$BRANCH" | grep -q .; then
  echo "sync branch $BRANCH already exists; nothing to do"; exit 0
fi

git config user.name "sync-bot"
git config user.email "sync-bot@users.noreply.github.com"
git checkout -B "$BRANCH" "origin/$BASE_BRANCH"

CONFLICTED=0
if ! git merge --no-ff --no-commit "$UPSTREAM_TAG" >/tmp/merge.log 2>&1; then
  CONFLICTED=1
  git status --porcelain > /tmp/sync-conflicts.txt || true
  git merge --abort
fi

if [ "$CONFLICTED" -eq 0 ]; then
  git commit -m "sync: merge upstream ${UPSTREAM_TAG} (dry-run)"
  {
    echo "Dry-run sync from upstream **${UPSTREAM_TAG}** — clean merge."
    echo; echo '```'; git diff --stat "origin/${BASE_BRANCH}"..HEAD; echo '```'
    echo; echo "**DO NOT MERGE**: Phase 0/1 dry run. Promotion gates arrive in Phase 2."
  } > "$BODY"
  git push origin "$BRANCH"
  command -v gh >/dev/null 2>&1 && \
    gh pr create --title "sync dry-run: ${UPSTREAM_TAG}" --body-file "$BODY" \
      --label sync-dryrun --base "$BASE_BRANCH" || echo "PR already exists"
else
  {
    echo "Dry-run sync to upstream **${UPSTREAM_TAG}** hit conflicts. Expected until AVO (Phase 3). Files:"
    echo; echo '```'; cat /tmp/sync-conflicts.txt; echo '```'
    echo; echo "Informational only — no branch was created."
  } > "$BODY"
  git checkout "$BASE_BRANCH"
  git branch -D "$BRANCH"
  command -v gh >/dev/null 2>&1 && \
    gh issue create --title "sync dry-run conflicts: ${UPSTREAM_TAG}" \
      --body-file "$BODY" --label sync-dryrun || true
fi

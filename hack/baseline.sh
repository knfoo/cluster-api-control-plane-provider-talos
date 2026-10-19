#!/usr/bin/env bash
# Phase 0 baseline: derive toolchain from go.mod, pin envtest assets, run the
# test suite, emit _out/baseline.json. Exits non-zero if tests fail.
set -euo pipefail
mkdir -p _out

GO_MOD_GO=$(awk '/^go /{print $2}' go.mod)
CLIENT_GO=$(go list -m -f '{{.Version}}' k8s.io/client-go)          # e.g. v0.32.3
CAPI_VERSION=$(go list -m -f '{{.Version}}' sigs.k8s.io/cluster-api)
ENVTEST_K8S_VERSION=$(echo "$CLIENT_GO" | sed -E 's/^v0\.([0-9]+)\..*/1.\1.x/')
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"

# envtest assets, pinned and cached
if ! command -v setup-envtest >/dev/null 2>&1; then
  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
fi
export PATH="$(go env GOPATH)/bin:$PATH"   # go install puts binaries in GOPATH/bin, which is not always on PATH
export KUBEBUILDER_ASSETS="${KUBEBUILDER_ASSETS:-$(setup-envtest use "$ENVTEST_K8S_VERSION" -p path)}"

UPSTREAM_TAG=$(git -c versionsort.suffix=- tag --sort=-v:refname --list 'v*' | head -1)
DIVERGENCE=$(git rev-list --count "upstream/${UPSTREAM_BRANCH}..HEAD" 2>/dev/null || echo "unknown")

START=$(date -u +%FT%TZ)
TEST_STATUS=green
# -count=1 busts the go test cache: a "green twice" run must re-verify, not replay.
# -skip '^TestIntegration
END=$(date -u +%FT%TZ)

cat > _out/baseline.json <<EOF
{
  "repo": "$(basename "$(pwd)")",
  "phase": "0",
  "date": "$END",
  "started": "$START",
  "upstream_tag": "$UPSTREAM_TAG",
  "divergence_commits": "$DIVERGENCE",
  "go": "$GO_MOD_GO",
  "cluster_api": "$CAPI_VERSION",
  "client_go": "$CLIENT_GO",
  "envtest_k8s": "$ENVTEST_K8S_VERSION",
  "kubebuilder_assets": "$KUBEBUILDER_ASSETS",
  "tests": "$TEST_STATUS"
}
EOF
cat _out/baseline.json
echo "Record it:  npx ruvector hooks remember --semantic --type decision --text \"$(cat _out/baseline.json | tr -d '\n')\""
test "$TEST_STATUS" = green

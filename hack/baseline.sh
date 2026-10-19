#!/usr/bin/env bash
# Phase 0 baseline: derive toolchain from go.mod, pin envtest, run the suite with
# `go test -json`, refuse vacuous greens, write _out/baseline.json.
# Exits non-zero unless the run is green AND plausible.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
mkdir -p _out
for t in go git jq; do command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 2; }; done

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"
UPSTREAM_REF="$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
MIN_DURATION_S="${MIN_DURATION_S:-2}"
MUST_RUN_FILE="${MUST_RUN_FILE:-hack/must-run.txt}"
PREV_BASELINE="${PREV_BASELINE:-evidence/baseline/baseline.json}"
RATCHET_ALLOW="${RATCHET_ALLOW:-}"       # set to an ADR id to accept a lower test count
KNOWN_RED_ALLOW="${KNOWN_RED_ALLOW:-}"   # set to an ADR id to accept zero passing tests as known-red

# --- toolchain, derived from go.mod ----------------------------------------
modver() { go list -m -f '{{.Version}}' "$1" 2>/dev/null || echo "absent"; }
GO_MOD_GO=$(awk '/^go /{print $2}' go.mod)
CLIENT_GO=$(modver k8s.io/client-go)
CAPI_VERSION=$(modver sigs.k8s.io/cluster-api)
CR_VERSION=$(modver sigs.k8s.io/controller-runtime)
# a replace directive wins over the require version, so record what is actually built
CABPT_PIN=$(go list -m -f '{{with .Replace}}{{.Path}}@{{.Version}}{{else}}{{.Version}}{{end}}' \
  github.com/siderolabs/cluster-api-bootstrap-provider-talos 2>/dev/null || echo "absent")  # empty in CABPT itself

# --- envtest: K8s version from client-go, setup-envtest pinned to controller-runtime
# Called by absolute path, so GOPATH/bin on PATH no longer matters (day-2 lesson 1).
# Branches before release-0.19 download from a frozen bucket, so floor at 0.19.
ENVTEST_K8S_VERSION=$(sed -E 's/^v0\.([0-9]+)\..*/1.\1.x/' <<<"$CLIENT_GO")
CR_MINOR=$(sed -E 's/^v0\.([0-9]+)\..*/\1/' <<<"$CR_VERSION")
[[ "$CR_MINOR" =~ ^[0-9]+$ ]] || CR_MINOR=19
if [ "$CR_MINOR" -lt 19 ]; then CR_BRANCH=release-0.19; else CR_BRANCH="release-0.$CR_MINOR"; fi
ENVTEST_BIN="$PWD/_out/bin/$CR_BRANCH"
if [ ! -x "$ENVTEST_BIN/setup-envtest" ]; then
  GOBIN="$ENVTEST_BIN" go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@$CR_BRANCH"
fi
if [ -z "${KUBEBUILDER_ASSETS:-}" ]; then
  KUBEBUILDER_ASSETS=$("$ENVTEST_BIN/setup-envtest" use "$ENVTEST_K8S_VERSION" -p path)
else
  # a value left over in the shell can be for another K8s minor (day-4 finding)
  case "$KUBEBUILDER_ASSETS" in
    */"${ENVTEST_K8S_VERSION%.x}".*) ;;
    *) echo "warning: preset KUBEBUILDER_ASSETS is not envtest $ENVTEST_K8S_VERSION: $KUBEBUILDER_ASSETS" >&2 ;;
  esac
fi
if [ -z "$KUBEBUILDER_ASSETS" ] || [ ! -d "$KUBEBUILDER_ASSETS" ]; then
  echo "KUBEBUILDER_ASSETS empty or missing: '$KUBEBUILDER_ASSETS'" >&2; exit 2
fi
export KUBEBUILDER_ASSETS

# --- git position -----------------------------------------------------------
HEAD_SHA=$(git rev-parse HEAD)
UPSTREAM_SHA=$(git rev-parse --verify -q "$UPSTREAM_REF" || echo "unknown")
AHEAD=$(git rev-list --count "$UPSTREAM_REF..HEAD" 2>/dev/null || echo "unknown")
BEHIND=$(git rev-list --count "HEAD..$UPSTREAM_REF" 2>/dev/null || echo "unknown")
# not `git describe`: it prints garbage for annotated tags fetched under upstream/*
# ("externally known as v0.9.0"). versionsort.suffix sorts v1.0.0-rc below v1.0.0.
NEAREST_TAG=$(git -c versionsort.suffix=- tag --merged HEAD --sort=-v:refname -l 'upstream/v*' | sed -n 1p)
NEAREST_TAG=${NEAREST_TAG:-none}

# --- tests -----------------------------------------------------------------
# -count=1 busts the go test cache: a "green twice" run must re-verify, not replay.
# -skip '^TestIntegration$' excludes the capi-utils integration test, which
# provisions real infrastructure (tier-3 e2e, out of scope for the baseline).
# The ^...$ anchors are load-bearing: unanchored, the pattern also matches
# TestTalosControlPlaneTemplateWebhookIntegration and yields a vacuous green.
DEFAULT_SKIP='^TestIntegration$'
TEST_SKIP="${TEST_SKIP:-$DEFAULT_SKIP}"
START=$(date -u +%FT%TZ); START_EPOCH=$(date +%s)
set +e
go test -count=1 -json -skip "$TEST_SKIP" ./... > _out/test.raw 2> _out/test.stderr
RC=$?
set -e
END=$(date -u +%FT%TZ); END_EPOCH=$(date +%s)
DURATION=$((END_EPOCH - START_EPOCH))

jq -cR 'fromjson? | objects' _out/test.raw > _out/test.json         # drop any non-JSON lines
jq -rj 'select(.Action=="output") | .Output // ""' _out/test.json > _out/test.log
[ -s _out/test.stderr ] && cat _out/test.stderr >&2

echo "--- packages ---"
jq -r 'select(.Test==null and (.Action=="pass" or .Action=="fail" or .Action=="skip"))
       | "\(.Action)\t\(.Elapsed // 0)s\t\(.Package)"' _out/test.json
jq -r 'select(.Test!=null and .Action=="fail") | "FAIL\t\(.Package)\t\(.Test)"' _out/test.json

count() { jq -s "$1" _out/test.json; }
PKGS_OK=$(count '[.[]|select(.Test==null and .Action=="pass")]|length')
PKGS_FAIL=$(count '[.[]|select(.Test==null and .Action=="fail")]|length')
TESTS_PASSED=$(count '[.[]|select(.Test!=null and .Action=="pass")|.Package+"/"+.Test]|unique|length')
TESTS_FAILED=$(count '[.[]|select(.Test!=null and .Action=="fail")|.Package+"/"+.Test]|unique|length')
TESTS_SKIPPED=$(count '[.[]|select(.Test!=null and .Action=="skip")|.Package+"/"+.Test]|unique|length')

# --- guards: green must be earned, not asserted --------------------------------
TEST_STATUS=green
REASONS=""
fail() { TEST_STATUS=failed; REASONS="${REASONS}${REASONS:+; }$1"; echo "GUARD: $1" >&2; }

if [ "$RC" -ne 0 ]; then fail "go test exited $RC"; fi
if [ "$PKGS_OK" -eq 0 ]; then fail "no package passed"; fi
# a package passes even when every test in it is skipped (day-4: CABPT on siderolabs)
KNOWN_RED=0
if [ "$TESTS_PASSED" -eq 0 ]; then
  if [ -n "$KNOWN_RED_ALLOW" ]; then
    KNOWN_RED=1; echo "known-red: no test passed, accepted per $KNOWN_RED_ALLOW" >&2
  else
    fail "no test passed"
  fi
fi
# day-2 lesson 4: a sub-floor green means the suite never ran
if [ "$DURATION" -lt "$MIN_DURATION_S" ]; then fail "suite finished in ${DURATION}s (< ${MIN_DURATION_S}s floor)"; fi

# day-2 lesson 6: named load-bearing tests must actually PASS (not skip, not be filtered)
MUST_RUN_MISSING=""
if [ -f "$MUST_RUN_FILE" ]; then
  PASSED_NAMES=$(count '[.[]|select(.Test!=null and .Action=="pass")|.Test]|unique|.[]' | jq -r '.')
  while IFS= read -r line; do
    t=$(echo "${line%%#*}" | xargs)
    if [ -z "$t" ]; then continue; fi
    if ! grep -qxF "$t" <<<"$PASSED_NAMES"; then MUST_RUN_MISSING="${MUST_RUN_MISSING}${MUST_RUN_MISSING:+ }$t"; fi
  done < "$MUST_RUN_FILE"
  if [ -n "$MUST_RUN_MISSING" ]; then fail "must-run test(s) did not pass: $MUST_RUN_MISSING"; fi
else
  echo "warning: $MUST_RUN_FILE not found; must-run guard inactive" >&2
fi

# ratchet: passed-test count may not drop below the last committed baseline
PREV_PASSED=""
if [ -f "$PREV_BASELINE" ]; then PREV_PASSED=$(jq -r '.tests_passed // empty' "$PREV_BASELINE"); fi
if [ -n "$PREV_PASSED" ] && [ "$TESTS_PASSED" -lt "$PREV_PASSED" ]; then
  if [ -n "$RATCHET_ALLOW" ]; then
    echo "ratchet: $TESTS_PASSED < $PREV_PASSED accepted per $RATCHET_ALLOW" >&2
  else
    fail "passed tests dropped $PREV_PASSED -> $TESTS_PASSED (set RATCHET_ALLOW=ADR-NNNN if intended)"
  fi
fi

# a known-red run passed every other guard; record it as such, never as green
if [ "$TEST_STATUS" = green ] && [ "$KNOWN_RED" -eq 1 ]; then TEST_STATUS=known-red; fi

# --- evidence ----------------------------------------------------------------
jq -n \
  --arg repo "$(basename "$PWD")" --arg started "$START" --arg date "$END" \
  --argjson duration_s "$DURATION" \
  --arg head_sha "$HEAD_SHA" --arg upstream_ref "$UPSTREAM_REF" --arg upstream_sha "$UPSTREAM_SHA" \
  --arg ahead "$AHEAD" --arg behind "$BEHIND" --arg nearest_upstream_tag "$NEAREST_TAG" \
  --arg go "$GO_MOD_GO" --arg cluster_api "$CAPI_VERSION" --arg client_go "$CLIENT_GO" \
  --arg controller_runtime "$CR_VERSION" --arg setup_envtest "$CR_BRANCH" \
  --arg envtest_k8s "$ENVTEST_K8S_VERSION" --arg kubebuilder_assets "$KUBEBUILDER_ASSETS" \
  --arg cabpt_pin "$CABPT_PIN" --arg test_skip "$TEST_SKIP" \
  --argjson packages_passed "$PKGS_OK" --argjson packages_failed "$PKGS_FAIL" \
  --argjson tests_passed "$TESTS_PASSED" --argjson tests_failed "$TESTS_FAILED" \
  --argjson tests_skipped "$TESTS_SKIPPED" --arg prev_tests_passed "$PREV_PASSED" \
  --arg must_run_missing "$MUST_RUN_MISSING" --arg ratchet_allow "$RATCHET_ALLOW" \
  --arg known_red_allow "$KNOWN_RED_ALLOW" \
  --arg tests "$TEST_STATUS" --arg guard_reasons "$REASONS" \
  '{kind:"baseline", phase:"0"} + $ARGS.named' > _out/baseline.json

cat _out/baseline.json
[ "$TEST_STATUS" = green ] || [ "$TEST_STATUS" = known-red ]

# ADR 0001: Phase 0 baseline for knfoo/cluster-api-control-plane-provider-talos

- **Status:** accepted (2026-10-19)
- **Context:** This repo is maintained as a fork by the self-improving fork-maintenance
  loop (blueprint, section 11). Upstream sync target: https://github.com/sidero-community/cluster-api-control-plane-provider-talos (branch main).
  A trusted baseline is required before any automated proposal can be evaluated.
- **Decision:** Baseline pinned to upstream tag v0.9.0 at 5d23387eacc2ed998762ed43d0c07006dd38ccd7 (fast-forwarded on 2026-10-19).
  Toolchain from go.mod: Go 1.26.5, CAPI v1.14.2, controller-runtime v0.24.1, client-go v0.36.3, envtest K8s 1.36.x.
  CABPT pin: v0.7.0-alpha.1.0.20260417161437-1f98f533c6f6 (replaced by github.com/sidero-community/cluster-api-bootstrap-provider-talos v0.9.0).
  Test suite green with kit v3: baseline.json shows tests_passed=116, packages_passed=6, duration_s=23-24.
- **CAPI position:** CACPPT on v1.14.2, latest CAPI release v1.14.3 (no minor lag), sibling fork CABPT on v1.12.11 (mismatch).
- **Evidence:** `_out/baseline.json`, `evidence/baseline/baseline.json` (kit v3, committed), `evidence/sync/outcome.json` (sync-dryrun noop), `evidence/capi/capi-watch.json` (CAPI position).
- **Consequences:** Phase 1 (sensing + dry-run syncs) may start. CAPI alignment (v1.14 vs v1.12 mismatch) is first Phase 1 agenda item.
- **Go/no-go:** GO. Kit intact, baseline reproducible, divergence ledger enforced, dry-run sync logic verified (PR creation requires PAT for GraphQL - Phase 1 task), CAPI position known. All Phase 0 criteria met.

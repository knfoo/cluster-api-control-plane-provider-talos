# ADR 0001: Phase 0 baseline for knfoo/cluster-api-control-plane-provider-talos

- **Status:** accepted (2026-10-19; re-baselined onto siderolabs the same day, ADR-0003)
- **Context:** This repo is a fork maintained by the self-improving fork-maintenance
  loop (blueprint, section 11). It has two upstreams: the provider upstream
  https://github.com/siderolabs/cluster-api-control-plane-provider-talos (git sync,
  branch `main`; sidero-community until ADR-0003) and kubernetes-sigs/cluster-api
  (contract sync, via go.mod). A trusted baseline is required before any automated
  proposal can be evaluated.
- **Decision:** Baseline is `upstream/main` at 8144d35 (nearest upstream tag v0.5.14),
  re-baselined on 2026-10-19. Divergence: all commits tooling, all trailered.
  Toolchain from go.mod: CAPI v1.10.9, CABPT v0.6.13, envtest K8s 1.32.x.
  Suite green twice locally and in CI: 38 tests passed, must-run tests passed.
  CAPI position: v1.10 here, v1.12 in the CABPT fork, v1.14 latest release.
- **Evidence:** `evidence/baseline/baseline.json` (CI run 39298752937, green);
  `evidence/sync/outcome.json` (no-op on siderolabs, run 39299236232);
  `evidence/sync/forced-outcome.json` (forced exercise on the earlier base: clean,
  draft PR #2 opened by the sync App, `baseline` green on it; the mechanism does not
  depend on the upstream); `evidence/capi/capi-watch.json` (run 39299306386, opened
  `capi-lag` #3). All recorded in the repo's RuVector store.
- **Consequences:** Phase 1 (sensing, dry-run syncs, CAPI bump proposals) may start.
- **Go/no-go:** GO. Kit self-check passes locally and in CI; baseline green twice
  locally and in CI with must-run and ratchet guards (ratchet reset per ADR-0003);
  `ledger-check` green; no-op sync on siderolabs and the forced PR path exercised, and
  the sync PR got CI; CAPI position known and the cross-fork mismatch explained
  (siderolabs: CACPPT v1.10, CABPT v1.12; latest v1.14), tracked in `capi-lag` #3;
  evidence committed and in RuVector. Not yet exercised: the conflicts-to-issue path.
  First Phase 1 work: carry a CAPI upgrade (open upstream PRs #236/#244/#251, smog
  patch 0001) so both forks reach the same minor.

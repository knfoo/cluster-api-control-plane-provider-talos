# ADR 0001: Phase 0 baseline for knfoo/cluster-api-control-plane-provider-talos

- **Status:** proposed (re-baselined onto siderolabs 2026-10-19 per ADR-0003;
  → accepted once CI baseline and a dry-run sync pass on the new base)
- **Context:** This repo is a fork maintained by the self-improving fork-maintenance
  loop (blueprint, section 11). It has two upstreams: the provider upstream
  https://github.com/siderolabs/cluster-api-control-plane-provider-talos (git sync,
  branch `main`; sidero-community until ADR-0003) and kubernetes-sigs/cluster-api
  (contract sync, via go.mod). A trusted baseline is required before any automated
  proposal can be evaluated.
- **Decision:** Baseline is `upstream/main` at 8144d35 (nearest upstream tag v0.5.14),
  re-baselined on 2026-10-19. Divergence: all commits tooling, all trailered.
  Toolchain from go.mod: CAPI v1.10.9, CABPT v0.6.13, envtest K8s 1.32.x.
  Suite green locally: 38 tests passed, must-run tests passed.
  CAPI position: v1.10 here, v1.12 in the CABPT fork, v1.14 latest release.
- **Evidence:** `evidence/baseline/`; `evidence/sync/` and `evidence/capi/` predate
  the re-baseline and get refreshed from CI.
- **Consequences:** Phase 1 (sensing, dry-run syncs, CAPI bump proposals) may start
  only after this ADR is accepted.
- **Go/no-go:** pending: CI baseline and a no-op dry-run sync on the siderolabs base.
  The forced PR path was proven on 2026-10-19 (PR #2) and is upstream-independent.

# ADR 0001: Phase 0 baseline for knfoo/cluster-api-control-plane-provider-talos

- **Status:** accepted (2026-10-19)
- **Context:** This repo is a fork maintained by the self-improving fork-maintenance
  loop (blueprint, section 11). It has two upstreams: the provider upstream
  https://github.com/sidero-community/cluster-api-control-plane-provider-talos (git
  sync, branch `main`) and kubernetes-sigs/cluster-api (contract sync, via go.mod).
  A trusted baseline is required before any automated proposal can be evaluated.
- **Decision:** Baseline is `upstream/main` at 5d23387eacc2ed998762ed43d0c07006dd38ccd7
  (upstream tag v0.9.0), fast-forwarded on 2026-10-19. Divergence: all commits
  ledgered (tooling, plus one `local` test fix per ADR-0002). Toolchain from go.mod:
  Go 1.26.5, CAPI v1.14.2, controller-runtime v0.24.1, client-go v0.36.3, envtest
  K8s 1.36.x. CABPT: go.mod requires the siderolabs module path but `replace`s it
  with github.com/sidero-community/cluster-api-bootstrap-provider-talos v0.9.0.
  Suite green twice locally and in CI: 116 tests passed in 6 packages (~22s),
  must-run test passed.
  CAPI position: v1.14.2 here, v1.14.3 latest release, v1.14.2 in the sibling CABPT
  fork (no lag, no mismatch). The earlier "v1.12 sibling" reading came from CABPT
  tracking the deprecated siderolabs upstream; see CABPT ADR-0004.
- **Evidence:** `evidence/baseline/baseline.json`; `evidence/sync/outcome.json`
  (no-op); `evidence/sync/forced-outcome.json` (forced exercise: clean, draft PR #2
  opened by the sync App, `baseline` green on it, run 39289825813);
  `evidence/capi/capi-watch.json` (CI run 39290560964). All recorded in the repo's
  RuVector store.
- **Consequences:** Phase 1 (sensing, dry-run syncs, CAPI bump proposals) may start.
- **Go/no-go:** GO. Kit self-check passes locally and in CI; baseline green twice
  locally and in CI with must-run and ratchet guards; `ledger-check` green; no-op and
  forced PR paths exercised without manual steps in the job, and the sync PR got CI;
  CAPI position known with no cross-fork mismatch; evidence committed and in
  RuVector. Not yet exercised: the conflicts-to-issue path (both forced runs merged
  cleanly). First Phase 1 decision: whether CACPPT's CABPT `replace` should point at
  knfoo/cluster-api-bootstrap-provider-talos instead of sidero-community.

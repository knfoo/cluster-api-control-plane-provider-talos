# ADR 0001: Phase 0 baseline for knfoo/cluster-api-control-plane-provider-talos

- **Status:** proposed (→ accepted on day 4)
- **Context:** This repo is maintained as a fork by the self-improving fork-maintenance
  loop (blueprint, section 11). Upstream sync target: <URL>. A trusted baseline is
  required before any automated proposal can be evaluated.
- **Decision:** Baseline pinned to upstream tag <tag>; toolchain derived from go.mod;
  test suite green on <date> with KUBEBUILDER_ASSETS at <envtest k8s version>.
- **Evidence:** `_out/baseline.json` (CI artifact of the first green run).
- **Consequences:** Phase 1 (sensing + dry-run syncs) may start only after this ADR
  is `accepted` and the ledger is written.

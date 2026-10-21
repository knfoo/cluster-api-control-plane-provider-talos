# ADR 0004: Track the latest Cluster API release

- **Status:** accepted (2026-10-21; owner decision Q9 in the harness review
  `docs/research/review-2026-10-21-independent.md`)
- **Context:** The fork's objective is to follow kubernetes-sigs/cluster-api at its
  latest release (v1.14.3 today) while siderolabs `main` stays on v1.10.9 and its
  own CAPI-upgrade PRs (#236, #244, #251) stay open. Every CAPI bump the harness
  applies therefore diverges from upstream until upstream catches up. The migration
  for one minor is mechanical (import moves, deprecated condition and patch
  helpers, the cluster cache, contract-versioned references, compile-level test
  fixes, RBAC the new contract needs) and must not change the TalosControlPlane
  API, CRD schema or behaviour; behaviour-changing adoptions are separate
  decisions.
- **Decision:**
  - CAPI-bump commits, and the changes a bump requires, carry
    `Divergence: local; ADR-0004`. One CAPI minor per commit; the baseline is
    green after each.
  - The same trailer covers preparatory commits a bump needs on the seed, such as
    RBAC for reading CustomResourceDefinitions (v1.11+ contract-versioned
    references).
  - Adoptable, behaviour-changing CAPI capabilities (ClusterClass template,
    in-place updates, v1beta2 contract for our own API) get their own ADR.
  - When upstream merges an equivalent change, upstream's version is preferred and
    ours is dropped; the revert carries the same trailer.
- **Evidence:** per bump commit, `evidence/baseline/` and the harness run record
  (`talos-capi-harness`, `spikes/model-replay/runs/` and the trajectory store).
- **Consequences:** `hack/ledger-check.sh` accepts the bump commits; the
  `capi-lag` issue closes when `capi-watch` reports no lag. The CABPT fork carries
  the same rule as its ADR-0007.

# ADR 0003: Track siderolabs as CACPPT's provider upstream

- **Status:** accepted (2026-10-19)
- **Context:** Phase 0 started with `sidero-community/cluster-api-control-plane-provider-talos`
  as the provider upstream. On 2026-10-19 the siderolabs Slack community advised
  against building on sidero-community, so both forks move to the siderolabs repos.
  siderolabs CACPPT `main` (v0.5.x line) is on CAPI v1.10.9 and pins CABPT v0.6.13.
  sidero-community branched from it at `9e552e4` (2026-08-14); since then siderolabs
  added 8 commits and sidero-community 48 (CAPI v1.14, KCP-style machine lifecycle).
  siderolabs has CAPI-upgrade PRs open for months (#228 v1.11, #236 v1.12, #244 and
  #251 v1.13), so CAPI upgrades are what this fork will have to carry.
- **Decision:** The `upstream` remote and `UPSTREAM_URL` point at
  https://github.com/siderolabs/cluster-api-control-plane-provider-talos.git, branch
  `main`. Our tooling commits were replayed onto `upstream/main` and `main` was
  force-pushed; commits that predated the trailer rule gained their trailer during
  the replay, so `.divergence-allow` is empty. The ADR-0002 test fix (`3811324`) was
  dropped: the test it fixes does not exist on siderolabs. The sidero-community
  history is kept locally as `backup/sidero-community-main`.
- **Evidence:** `evidence/baseline/baseline.json`: 38 tests passed (envtest
  `TestSuite` subtests plus etcd/scale unit tests), CAPI v1.10.9, CABPT v0.6.13.
- **Consequences:** ADR-0002 is superseded. CAPI position is v1.10 here vs v1.12 in
  the CABPT fork vs v1.14 latest: real drift, and the first Phase 1 work item
  (carry a CAPI upgrade, starting from the open upstream PRs and the
  `feat/template-bootstrap-config` work).

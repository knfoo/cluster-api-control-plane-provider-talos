# Divergence Ledger — cluster-api-control-plane-provider-talos

- **Repo:** knfoo/cluster-api-control-plane-provider-talos
- **Upstream:** https://github.com/sidero-community/cluster-api-control-plane-provider-talos (branch: main)
- **Upstream push:** disabled (`git remote set-url --push upstream no-push`)
- **Baseline date:** 2026-10-19
- **Measured at baseline:** 0 commits ahead / 52 behind — caught up via fast-forward
- **Evidence:** ADR-0001; day-2 baseline `_out/baseline.json` (green twice, 22s real runs)

## Standing rule

Every commit that lands in `upstream/main..HEAD` must appear in the table below
on the day it lands, with an ADR. The sync bot rebases this series every cycle.
If upstream absorbs a patch, drop it, close the ADR, and record the drop here.
A commit that is not in this table is a bug — either it shouldn't exist, or this
file is out of date.
Exception: commits whose only diff is this file (ledger bookkeeping) are exempt —
that is the ledger operating on itself, and exempting it prevents infinite regress.

## Ledger

Audited 2026-10-19 via `git diff --stat upstream/main..HEAD`: 8 files diverge —
the kit (hack/, workflows, docs, this file) plus exactly one test file (row 5).
No provider code modified.
   # | Patch | Title | ADR | Category | Absorbed upstream? |
 |---|---|---|---|---|---|
 | 1 | `80af87e` | docs: record kit commits in divergence ledger | ADR-0001 | tooling (bookkeeping, exempt) | n/a |
 | 2 | `e07eba7` | docs(phase0): add ADR-0001 (proposed) | ADR-0001 | tooling | n/a |
 | 3 | `50d08b3` | chore(phase0): add fork-maintenance kit (baseline, sync dry-run, workflows) | ADR-0001 | tooling | n/a |
 | 4 | `5689939` | chore(phase0): fix kit - baseline (go test invocation, anchored skip) | ADR-0001 | tooling | n/a |
 | 5 | `3811324` | fix: make invalid-template webhook fixture reach the webhook (CRD-schema-valid ControlPlaneConfig) | ADR-0002 | **local** | no |
 | 6 | `47c1442` | fix(test): ADR-0002 + ledger row for the local fix (docs-only) | ADR-0002 | tooling | n/a |
 | 7 | `a28f508` | docs: record kit commits in divergence ledger | ADR-0001 | tooling (bookkeeping, exempt) | n/a |
 | 8 | `56a4e10` | chore(phase0): fix kit - baseline (duration_s + vacuous-green guard) | ADR-0001 | tooling | n/a |

## Categories

- **tooling** — fork-maintenance additions only (`hack/`, sync workflows, this
  file, ADRs). Never touches provider code; merge conflicts with upstream
  should be impossible.
- **carry** — a fix/feature we run ahead of upstream. MUST link the upstream
  PR/issue proposing it; entry deleted when upstream merges it. **Unused in
  this project** — upstream contributions (PRs/issues) are out of scope; see
  ADR-0002.
- **local** — deliberate fork-only behavior. Must state why it won't go
  upstream — because it *can't* (repo-specific), or because contributing is
  out of scope (row 5 / ADR-0002: upstreamable in principle, out of scope
  here). Either way, be most suspicious of this category. Revisit rule per
  ADR-0002: if upstream changes this test, prefer upstream's version and drop
  the patch; delete the row when upstream fixes it independently.
- **revert** — an upstream change we temporarily refuse. Requires a revisit date.

## Not in scope

Upstream's own changes, anything not on `main`, and local scratch branches —
though stray branches should be enumerated and deleted, not left to rot.

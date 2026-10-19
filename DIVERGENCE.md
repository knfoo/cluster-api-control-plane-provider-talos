# Divergence Ledger — cluster-api-control-plane-provider-talos

- **Repo:** knfoo/cluster-api-control-plane-provider-talos
- **Upstream (the `upstream` remote; the diff rule is relative to THIS repo):**
  https://github.com/sidero-community/cluster-api-control-plane-provider-talos
  (branch: main — v0.9.0 / CAPI v1beta2 era)
- **Upstream push:** disabled (`git remote set-url --push upstream no-push`)
- **Baseline date:** 2026-10-19
- **Measured at baseline:** 0 commits ahead / 52 behind — caught up via fast-forward
- **Evidence:** ADR-0001

## Standing rule

Every commit that lands in `upstream/main..HEAD` must appear in the table below
on the day it lands, with an ADR. The sync bot rebases this series every cycle.
If upstream absorbs a patch, drop it, close the ADR, and record the drop here.
A commit that is not in this table is a bug — either it shouldn't exist, or this
file is out of date.

## Ledger

| # | Patch | Title | ADR | Category | Absorbed upstream? |
|---|---|---|---|---|---|
| 1 | `<sha of the kit commit>` | phase0: fork-maintenance kit (hack/, workflows, DIVERGENCE.md) | ADR-0001 | tooling | n/a |

## Categories

- **tooling** — fork-maintenance additions only (`hack/`, sync workflows, this
  file, ADRs). Never touches provider code; merge conflicts with upstream
  should be impossible.
- **carry** — a fix/feature we run ahead of upstream. MUST link the upstream
  PR/issue proposing it; entry deleted when upstream merges it.
- **local** — deliberate fork-only behavior. Must state why it cannot go
  upstream; be most suspicious of this category.
- **revert** — an upstream change we temporarily refuse. Requires a revisit date.

## Not in scope

Upstream's own changes, anything not on `main`, and local scratch branches —
though stray branches should be enumerated and deleted, not left to rot.

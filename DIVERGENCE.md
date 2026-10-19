# Divergence Ledger — cluster-api-control-plane-provider-talos

- **Repo:** knfoo/cluster-api-control-plane-provider-talos
- **Provider upstream (`upstream` remote):** https://github.com/sidero-community/cluster-api-control-plane-provider-talos, branch `main`
- **Contract upstream:** kubernetes-sigs/cluster-api (tracked by `hack/capi-watch.sh`)
- **Upstream push:** disabled (`git remote set-url --push upstream no-push`)
- **Baseline date:** 2026-10-19
- **Measured at baseline:** 0 ahead / 52 behind → fast-forwarded to upstream/main
- **Evidence:** ADR-0001, ADR-0002, `evidence/`

## Standing rule (enforced by CI)

Every non-merge commit in `upstream/main..HEAD` carries a trailer:

    Divergence: <tooling|local|revert>; <ADR-NNNN>

`hack/ledger-check.sh` fails CI if one is missing or uses an unknown category.
Commits that predate the rule are listed in `.divergence-allow`. The table below
is generated with `hack/ledger-check.sh --render`; git is the source of truth,
this table is a convenience view and may lag by a commit.

In Phase 0/1, upstream is merged into dry-run branches only; `main` is never
synced automatically. Rebase-vs-merge for promotion is decided in Phase 2 (own ADR).

If upstream fixes something we carry as `local`, prefer upstream's version, drop
our patch (the revert commit carries the same trailer), close the ADR, and note
the drop below.

## Ledger

| # | Patch | Title | Category | ADR |
|---|---|---|---|---|
| 1 | `80af87e` | docs: record kit commits in divergence ledger | tooling | ADR-0001 |
| 2 | `e07eba7` | docs(phase0): add ADR-0001 (proposed) | tooling | ADR-0001 |
| 3 | `50d08b3` | chore(phase0): add fork-maintenance kit (baseline, sync dry-run, workflows) | tooling | ADR-0001 |
| 4 | `a28f508` | docs: record kit commits in divergence ledger | tooling | ADR-0001 |
| 5 | `5689939` | chore(phase0): fix fork-maintenance kit - baseline | tooling | ADR-0001 |
| 6 | `3811324` | fix: make invalid-template webhook fixture pass CRD schema validation | local | ADR-0002 |
| 7 | `47c1442` | fix(test): reach the webhook in invalid-template fixture (ADR-0002, local) | tooling | ADR-0002 |
| 8 | `56a4e10` | chore(phase0): fix fork-maintenance kit - baseline | tooling | ADR-0001 |
| 9 | `bee62d1` | docs: audit ledger — 8/8 commits categorized (3811324 local/ADR-0002) | tooling | ADR-0001 |
| 10 | `b85eea6` | chore(phase0): kit v3 (json guards, sync on main, capi-watch, ledger-check) | tooling | ADR-0001 |
| 11 | `2823e95` | docs(phase0): baseline evidence (kit v3) | tooling | ADR-0001 |
| 12 | `75f1c25` | chore(phase0): fix sync-dryrun workflow to use GITHUB_TOKEN | tooling | ADR-0001 |
| 13 | `e9799e8` | chore(phase0): handle label errors gracefully in sync-dryrun | tooling | ADR-0001 |
| 14 | `79ca8ef` | chore(phase0): update workflow permissions | tooling | ADR-0001 |
| 15 | `865dc0c` | chore(phase0): fix workflow permissions | tooling | ADR-0001 |
| 16 | `2e07fdc` | docs(phase0): update ADR-0001 to accepted with Day 4 evidence and go/no-go note | tooling | ADR-0001 |
| 17 | `0b92f50` | fix(phase0): pin gh to our own repo, restore the GitHub App token for sync | tooling | ADR-0001 |
| 18 | `d752b3a` | docs(phase0): accept ADR-0001 with dry-run and capi evidence | tooling | ADR-0001 |
| 19 | `d353fef` | fix(phase0): kit review fixes (zero-test guard, tag, pin, touched files) | tooling | ADR-0001 |

## Dropped / absorbed upstream

| Patch | Superseded by upstream commit | Date |
|---|---|---|

## Categories

- **tooling** — fork-maintenance only (`hack/`, our workflows, this file, ADRs,
  `evidence/`, `.divergence-allow`). Never touches provider code.
- **carry** — unused in this project. It requires an upstream PR/issue link,
  and upstream contributions are out of scope.
- **local** — deliberate fork-only behavior. Must state why it won't go upstream:
  it can't (repo-specific), or contributing is out of scope (e.g. ADR-0002).
  Be most suspicious of this category.
- **revert** — an upstream change we temporarily refuse. Requires a revisit date.

## Not in scope

Upstream's own changes and anything not on `main`. Stray branches are enumerated
and deleted, not left to rot.

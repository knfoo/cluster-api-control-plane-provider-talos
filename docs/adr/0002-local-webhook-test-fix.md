# ADR 0002: Local fix for upstream's webhook envtest bug

- **Status:** superseded by ADR-0003 (2026-10-19). The patch was dropped when the
  fork moved to siderolabs, whose `main` does not have this test.
- **Context:** `TestTalosControlPlaneTemplateWebhookIntegration` fails on
  upstream code as shipped. The `invalid-template` fixture leaves
  `ControlPlaneConfig` empty, so CRD schema validation rejects
  `generateType: ""` before the webhook runs; the assertion on the
  rollout-strategy "valid values" error can therefore never pass. Reproduced
  2026-10-19 with KUBEBUILDER_ASSETS 1.36.2-linux-amd64 (envtest 1.36.x).
- **Decision:** Fix the fixture in our fork — give it a valid
  `ControlPlaneConfig` (`talosConfigSpec("controlplane", nil)`) so the object
  survives schema validation and reaches the webhook under test. Ledger
  category: **local**. The fix is upstreamable in principle, but upstream
  contributions (PRs/issues) are out of scope for this project.
- **Evidence:** suite green in a real ~10s run (anchored `-skip '^TestIntegration$'`,
  `-count=1`), after the failure was first reproduced and diagnosed. The test is
  listed in `hack/must-run.txt`, so every baseline proves it actually passed.
- **Consequences:** The suite is green for the right reason, not by skipping.
  The patch stays ledgered and travels with every sync.
- **Revisit rule:** If upstream ever changes this test, prefer upstream's
  version and drop this patch. A conflict on this file in a dry-run sync is a
  signal to re-evaluate, not to force ours. (This only works because the sync
  targets `upstream/main`; a tag-based sync would never surface it.) Drop the
  patch when upstream fixes the test independently.

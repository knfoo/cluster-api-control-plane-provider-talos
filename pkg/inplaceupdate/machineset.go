// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// CanUpdateMachineSet, the MachineDeployment hook that plans the in-place update of a
// MachineSet, i.e. of the worker Machines of a MachineDeployment.
//
// The MachineDeployment controller calls CanUpdateMachineSet when a MachineSet is rolled
// out ("This hook is called by the MachineDeployment controller when performing the 'can
// update in-place' for all the Machines controlled by a MachineSet",
// docs/book/src/tasks/experimental-features/runtime-sdk/implement-in-place-update-hooks.md),
// applies the returned patches to the current objects of the request and then compares
// current and desired (machinedeployment_canupdatemachineset.go: canExtensionsUpdateMachineSet,
// applyPatchesToRequest, matchesMachineSet). Only the spec of the objects is compared, the
// bootstrap config reference of the MachineSet spec is cleaned up before the diff, so the
// extension patches the TalosConfigTemplate's spec.template.spec and the Kubernetes version
// of the MachineSet template. Everything else - an infrastructure machine template, a
// Talos version, a machine configuration change Talos can only apply with a reboot - gets
// no patch, which leaves current and desired different and makes Cluster API roll the
// MachineSet out (docs/adr/ADR-0003).
//
// The request carries no Machine objects, so the machine configuration of the rollout is
// rendered from the MachineSet template spec and the Machines of the machine set are read
// from the management cluster through the MachineSetNameLabel (pkg/inplaceupdate/metadata.go).
// The rendered desired configuration is DryRun against a live Machine of the machine set,
// the owner's condition of docs/adr/ADR-0003 that a change is handled in place only when
// Talos accepts it with mode ApplyConfigurationRequest_NO_REBOOT: a node that needs a
// reboot, or that refuses the configuration in immediate mode, gets no patch and the
// MachineSet is rolled out.
package inplaceupdate

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// CanUpdateMachineSet is the CanUpdateMachineSet hook. It shares the pipeline of the
// CanUpdateMachine hook (pkg/inplaceupdate/handlers.go: planInPlaceUpdate): decode the
// machine configuration spec of the current and the desired TalosConfigTemplate, render the
// machine configuration, diff it and check the reboot guard; only the DryRun against a node
// of the machine set is specific to a MachineSet rollout.
func (h *Handler) CanUpdateMachineSet(req *runtimehooksv1.CanUpdateMachineSetRequest, resp *runtimehooksv1.CanUpdateMachineSetResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	machineSet := req.Current.MachineSet.Name

	if reason := machineSetRolloutReason(req); reason != "" {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	// The machine configuration of a MachineSet rollout is the configuration the template
	// renders for the machines the template describes. A configuration the extension cannot
	// render is answered with Success and no patch, which leaves the MachineSet different
	// from its desired state and makes Cluster API roll it out.
	plan, err := h.planInPlaceUpdate(
		ctx,
		req.Current.BootstrapConfigTemplate,
		req.Desired.BootstrapConfigTemplate,
		templateMachine(req.Current.MachineSet),
		templateMachine(req.Desired.MachineSet),
	)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if plan.reason != "" {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", plan.reason)

		return
	}

	// The reboot path guard of planInPlaceUpdate is the cheap half; this DryRun against a
	// live Machine of the machine set is the authoritative one, and it is what decides a
	// change Talos only applies after a reboot, such as machine/env on Talos v1.12. A
	// failed DryRun is a rollout reason here rather than a hook failure: no node of the
	// machine set has approved the configuration.
	if len(plan.ops) > 0 {
		reason := h.machineSetDryRunReason(ctx, req, plan.config)

		if reason != "" {
			h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

			return
		}

		patch, err := configPatch(req.Current.BootstrapConfigTemplate, req.Desired.BootstrapConfigTemplate)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}

		resp.BootstrapConfigTemplatePatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	if plan.versionChange {
		patch, err := jsonPatch([]jsonPatchOp{{Op: "replace", Path: "/spec/template/spec/version", Value: req.Desired.MachineSet.Spec.Template.Spec.Version}})
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}

		resp.MachineSetPatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	h.log.Info("machine set can be updated in place", "machineSet", machineSet, "operations", len(plan.ops), "version", req.Desired.MachineSet.Spec.Template.Spec.Version)
}

// machineSetDryRunReason DryRuns the rendered desired machine configuration against the live
// Machines of the machine set and returns why the MachineSet has to be rolled out, or ""
// when a node accepts the configuration with mode ApplyConfigurationRequest_NO_REBOOT.
//
// Talos answers a configuration it can only apply after a boot with a mode other than
// NO_REBOOT (pkg/inplaceupdate/talos.go: request), and an older Talos release refuses an
// immediate-mode apply outright - machine/env on Talos v1.12 with "this config change can't
// be applied in immediate mode". Either answer means the node can never finish the in-place
// update, so the hook returns no patch and Cluster API rolls the MachineSet out.
//
// An in-place update nobody DryRunned is not approved either: when the Machines of the
// machine set cannot be read, or when none of them can be reached, the hook returns a
// rollout reason. It falls back to the cheap reboot path guard of planInPlaceUpdate only in
// the two cases where there is nothing to ask: the extension has no reader for the Machines
// of the machine set at all, exactly like the other optional capabilities of the metadata
// reader, and the machine set has no Machines, so there is no node to update in place.
func (h *Handler) machineSetDryRunReason(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, config []byte) string {
	machineSet := req.Current.MachineSet.Name

	reader, ok := h.metadata.(MachineSetMachineReader)
	if !ok {
		h.log.Info("the extension has no reader for the machines of the machine set, so the machine configuration is not DryRun", "machineSet", machineSet)

		return ""
	}

	machines, err := reader.MachineSetMachines(ctx, req.Current.MachineSet.Namespace, machineSet)
	if err != nil {
		return fmt.Sprintf("the machines of the machine set %q cannot be read, so no node DryRuns the machine configuration: %v", machineSet, err)
	}

	if len(machines) == 0 {
		h.log.Info("the machine set has no machines, so there is no node to DryRun the machine configuration", "machineSet", machineSet)

		return ""
	}

	for _, machine := range machines {
		endpoint, err := h.resolveEndpoint(req.Settings, machine)
		if err != nil {
			h.log.Info("the Talos address of a machine of the machine set cannot be determined, so it is not asked about the machine configuration", "machine", machine.Name, "error", err.Error())

			continue
		}

		reason, err := h.dryRunReason(ctx, endpoint, config)
		if err != nil {
			return fmt.Sprintf("machine %q of the machine set does not answer the DryRun of the machine configuration: %v", machine.Name, err)
		}

		return reason
	}

	return fmt.Sprintf("no machine of the machine set %q can be reached, so the machine configuration is not DryRun", machineSet)
}

// machineSetRolloutReason returns why a MachineSet cannot be updated by patching its
// TalosConfigTemplate and its Kubernetes version, or "" when it can.
func machineSetRolloutReason(req *runtimehooksv1.CanUpdateMachineSetRequest) string {
	current := req.Current.MachineSet.Spec.Template.Spec
	desired := req.Desired.MachineSet.Spec.Template.Spec

	if !equalJSON(current.InfrastructureRef, desired.InfrastructureRef) {
		return "the infrastructure machine template of the machine set changes"
	}

	// The extension never patches an infrastructure machine template, so every change of
	// one is a rollout.
	if !equalJSON(infraSpec(req.Current.InfrastructureMachineTemplate), infraSpec(req.Desired.InfrastructureMachineTemplate)) {
		return "the infrastructure machine template spec changes"
	}

	if !current.Bootstrap.ConfigRef.IsDefined() || !desired.Bootstrap.ConfigRef.IsDefined() {
		return "the machine set has no bootstrap configuration template"
	}

	// A machine set bootstrap configuration that is not a TalosConfigTemplate, or is
	// bootstrap data instead of a template, is not an error: it is just a change this
	// extension cannot patch, so the hook answers Success with no patch and Cluster API
	// rolls the MachineSet out.
	if configKind(req.Current.BootstrapConfigTemplate) != "TalosConfigTemplate" || configKind(req.Desired.BootstrapConfigTemplate) != "TalosConfigTemplate" {
		return "the bootstrap configuration template of the machine set is not a TalosConfigTemplate"
	}

	if current.Version != desired.Version && !kubernetesVersionUpgrade(current.Version, desired.Version) {
		return "the Kubernetes version changes"
	}

	return ""
}

// templateMachine builds the Machine a MachineSet template describes. CanUpdateMachineSet
// carries no Machine objects, so the machine configuration of the rollout is rendered from
// the template spec.
func templateMachine(machineSet clusterv1.MachineSet) clusterv1.Machine {
	return clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineSet.Name,
			Namespace: machineSet.Namespace,
		},
		Spec: machineSet.Spec.Template.Spec,
	}
}

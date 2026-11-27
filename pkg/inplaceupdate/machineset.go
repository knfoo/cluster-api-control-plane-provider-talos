// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// CanUpdateMachineSet, the MachineDeployment hook that plans the in-place update of a
// MachineSet, i.e. of the worker Machines of a MachineDeployment.
//
// The MachineDeployment controller calls CanUpdateMachineSet before rolling out a MachineSet,
// applies the returned patches to the current objects of the request and then compares current
// and desired. Only the spec of the objects is compared and the bootstrap config reference of
// the MachineSet spec is cleaned up before the diff, so the hook patches the
// TalosConfigTemplate's spec.template.spec and the Kubernetes version of the MachineSet
// template. Everything else - an infrastructure machine template, a Talos version, a machine
// configuration change Talos can only apply with a reboot - gets no patch, which leaves
// current and desired different and makes Cluster API roll the MachineSet out.
//
// The request carries no Machine objects, so the machine configuration of the rollout is
// rendered from the MachineSet template spec and the Machines of the machine set are read from
// the management cluster through the MachineSetNameLabel. The rendered desired configuration is
// DryRun against a live Machine of the machine set: a change is handled in place only when
// Talos accepts it with mode ApplyConfigurationRequest_NO_REBOOT, so a node that needs a
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
// CanUpdateMachine hook, planInPlaceUpdate: decode the machine configuration spec of the
// current and the desired TalosConfigTemplate, render the machine configuration, diff it and
// check the reboot guard; only the DryRun against a node of the machine set is specific to a
// MachineSet rollout.
func (h *Handler) CanUpdateMachineSet(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, resp *runtimehooksv1.CanUpdateMachineSetResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	if err := h.canUpdateMachineSet(ctx, req, resp); err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()
	}
}

// canUpdateMachineSet plans the in-place update of a MachineSet. A refusal is Success with no
// patch: current and desired stay different, so Cluster API rolls the MachineSet out. Only a
// failure of the hook itself is returned as an error.
func (h *Handler) canUpdateMachineSet(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, resp *runtimehooksv1.CanUpdateMachineSetResponse) error {
	machineSet := req.Current.MachineSet.Name

	refuse := func(reason string) error {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

		return nil
	}

	current := req.Current.MachineSet.Spec.Template.Spec
	desired := req.Desired.MachineSet.Spec.Template.Spec

	if current.InfrastructureRef != desired.InfrastructureRef {
		return refuse("the infrastructure machine template of the machine set changes")
	}

	// The hook never patches an infrastructure machine template, so every change of one is a
	// rollout.
	if !equalJSON(infraSpec(req.Current.InfrastructureMachineTemplate), infraSpec(req.Desired.InfrastructureMachineTemplate)) {
		return refuse("the infrastructure machine template spec changes")
	}

	if !current.Bootstrap.ConfigRef.IsDefined() || !desired.Bootstrap.ConfigRef.IsDefined() {
		return refuse("the machine set has no bootstrap configuration template")
	}

	// A machine set bootstrap configuration that is not a TalosConfigTemplate, or is bootstrap
	// data instead of a template, is not an error: it is just a change this hook cannot patch, so
	// the hook answers Success with no patch and Cluster API rolls the MachineSet out.
	if configKind(req.Current.BootstrapConfigTemplate) != "TalosConfigTemplate" || configKind(req.Desired.BootstrapConfigTemplate) != "TalosConfigTemplate" {
		return refuse("the bootstrap configuration template of the machine set is not a TalosConfigTemplate")
	}

	// A Kubernetes version change is handled in place only when it is the next minor version, the
	// way talosctl upgrade-k8s upgrades a node.
	if current.Version != desired.Version && !kubernetesVersionUpgrade(current.Version, desired.Version) {
		return refuse("the Kubernetes version changes")
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	// The machine configuration of a MachineSet rollout is the configuration the template renders
	// for the machines the template describes. A configuration the hooks cannot render is answered
	// with Success and no patch, which leaves the MachineSet different from its desired state and
	// makes Cluster API roll it out.
	plan, err := h.planInPlaceUpdate(ctx, req.Current.BootstrapConfigTemplate, req.Desired.BootstrapConfigTemplate, templateMachine(req.Current.MachineSet), templateMachine(req.Desired.MachineSet))
	if err != nil {
		return err
	}

	if plan.reason != "" {
		return refuse(plan.reason)
	}

	// The reboot path guard of planInPlaceUpdate is the cheap half; this DryRun against a live
	// Machine of the machine set is the authoritative one, and it is what decides a change Talos
	// only applies after a reboot, such as machine/env on Talos v1.12. A failed DryRun is a rollout
	// reason here rather than a hook failure: no node of the machine set has approved the
	// configuration.
	if len(plan.ops) > 0 {
		if reason := h.machineSetDryRunReason(ctx, req, plan.config); reason != "" {
			return refuse(reason)
		}

		patch, err := configPatch(req.Current.BootstrapConfigTemplate, req.Desired.BootstrapConfigTemplate)
		if err != nil {
			return err
		}

		resp.BootstrapConfigTemplatePatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	if plan.versionChange {
		patch, err := jsonPatch([]jsonPatchOp{{Op: "replace", Path: "/spec/template/spec/version", Value: desired.Version}})
		if err != nil {
			return err
		}

		resp.MachineSetPatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	h.log.Info("machine set can be updated in place", "machineSet", machineSet, "operations", len(plan.ops), "version", desired.Version)

	return nil
}

// machineSetDryRunReason DryRuns the rendered desired machine configuration against the live
// Machines of the machine set and returns why the MachineSet has to be rolled out, or ""
// when a node accepts the configuration with mode ApplyConfigurationRequest_NO_REBOOT.
//
// Talos answers a configuration it can only apply after a boot with a mode other than
// NO_REBOOT, and an older Talos release refuses an immediate-mode apply outright, for example
// machine/env on Talos v1.12. Either answer means the node can never finish the in-place
// update, so the hook returns no patch and Cluster API rolls the MachineSet out.
//
// An in-place update nobody DryRunned is not approved either: when the Machines of the
// machine set cannot be read, or when none of them can be reached, the hook returns a rollout
// reason. It falls back to the cheap reboot path guard of planInPlaceUpdate only in the two
// cases where there is nothing to ask: the reader has no access to the Machines of the
// machine set, exactly like the other capabilities of the metadata reader, and the machine set
// has no Machines, so there is no node to update in place.
func (h *Handler) machineSetDryRunReason(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, config []byte) string {
	machineSet := req.Current.MachineSet.Name

	reader, ok := h.metadata.(MachineSetMachineReader)
	if !ok {
		h.log.Info("there is no reader for the machines of the machine set, so the machine configuration is not DryRun", "machineSet", machineSet)

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

	// The first Machine that can be reached decides: its node is the one that would run the
	// in-place update of the machine configuration.
	for _, machine := range machines {
		endpoint, applier, err := h.node(ctx, machine)
		if err != nil {
			h.log.Info("a machine of the machine set cannot be reached for the DryRun of the machine configuration", "machine", machine.Name, "error", err.Error())

			continue
		}

		reason, err := h.dryRunReason(ctx, applier, endpoint, config)
		if err != nil {
			return fmt.Sprintf("machine %q of the machine set does not answer the DryRun of the machine configuration: %v", machine.Name, err)
		}

		return reason
	}

	return fmt.Sprintf("no machine of the machine set %q can be reached, so the machine configuration is not DryRun", machineSet)
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

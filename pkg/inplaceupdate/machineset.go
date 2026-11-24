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
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// CanUpdateMachineSet is the CanUpdateMachineSet hook.
func (h *Handler) CanUpdateMachineSet(req *runtimehooksv1.CanUpdateMachineSetRequest, resp *runtimehooksv1.CanUpdateMachineSetResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	machineSet := req.Current.MachineSet.Name

	if reason := machineSetRolloutReason(req); reason != "" {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

		return
	}

	currentSpec, err := talosConfigTemplateSpec(req.Current.BootstrapConfigTemplate)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	desiredSpec, err := talosConfigTemplateSpec(req.Desired.BootstrapConfigTemplate)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if currentSpec.TalosVersion != desiredSpec.TalosVersion {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", "the Talos version changes")

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	// The machine configuration of a MachineSet rollout is the configuration the template
	// renders for the machines the template describes. A configuration the extension cannot
	// render is answered with Success and no patch, which leaves the MachineSet different
	// from its desired state and makes Cluster API roll it out.
	currentConfig, _, err := h.renderMachineConfig(ctx, currentSpec, templateMachine(req.Current.MachineSet))
	if err != nil {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", err.Error())

		return
	}

	desiredConfig, _, err := h.renderMachineConfig(ctx, desiredSpec, templateMachine(req.Desired.MachineSet))
	if err != nil {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", err.Error())

		return
	}

	ops, err := configDiff(currentConfig, desiredConfig)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	// The Talos versions are equal (checked above), so the version of the current template
	// is the version the machines of the machine set run.
	if reason := rebootRequiredReason(ops); reason != "" {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

		return
	}

	currentVersion := req.Current.MachineSet.Spec.Template.Spec.Version
	desiredVersion := req.Desired.MachineSet.Spec.Template.Spec.Version
	versionUpgrade := currentVersion != desiredVersion

	if len(ops) == 0 && !versionUpgrade {
		h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", "the machine configuration of the machine set does not change")

		return
	}

	// The owner's condition of docs/adr/ADR-0003: a machine configuration change is handled
	// in place only when Talos accepts it with mode ApplyConfigurationRequest_NO_REBOOT.
	// The reboot path guard above is the cheap half; this DryRun against a live Machine of
	// the machine set is the authoritative one, and it is what decides a change Talos only
	// applies after a reboot, such as machine/env on Talos v1.12.
	if len(ops) > 0 {
		if reason := h.machineSetDryRunReason(ctx, req, desiredConfig); reason != "" {
			h.log.Info("machine set cannot be updated in place", "machineSet", machineSet, "reason", reason)

			return
		}
	}

	if len(ops) > 0 {
		patch, err := templateSpecPatch(req.Current.BootstrapConfigTemplate, req.Desired.BootstrapConfigTemplate)
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

	if versionUpgrade {
		patch, err := jsonPatch([]jsonPatchOp{{Op: "replace", Path: "/spec/template/spec/version", Value: desiredVersion}})
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

	h.log.Info("machine set can be updated in place", "machineSet", machineSet, "operations", len(ops), "version", desiredVersion)
}

// machineSetDryRunReason DryRuns the rendered desired machine configuration against a live
// Machine of the machine set and returns why the MachineSet has to be rolled out, or ""
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
// rollout reason. It falls back to the cheap reboot path guard only in the two cases where
// there is nothing to ask: the extension has no reader for the Machines of the machine set
// at all, exactly like the other optional capabilities of the metadata reader, and the
// machine set has no Machines, so there is no node to update in place.
func (h *Handler) machineSetDryRunReason(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, desiredConfig []byte) string {
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

		status, err := h.applier.DryRun(ctx, endpoint, desiredConfig)
		if err != nil {
			return fmt.Sprintf("machine %q of the machine set does not accept the machine configuration without a reboot: %v", machine.Name, err)
		}

		if status.RebootRequired {
			return fmt.Sprintf("Talos needs a reboot to apply the machine configuration of machine %q of the machine set: %s", machine.Name, status.Details)
		}

		return ""
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

	if !isTalosConfigTemplate(req.Current.BootstrapConfigTemplate) || !isTalosConfigTemplate(req.Desired.BootstrapConfigTemplate) {
		return "the bootstrap configuration template of the machine set is not a TalosConfigTemplate"
	}

	if current.Version != desired.Version && !kubernetesVersionUpgrade(current.Version, desired.Version) {
		return "the Kubernetes version changes"
	}

	return ""
}

// isTalosConfigTemplate reports whether a bootstrap config template object of a
// CanUpdateMachineSet request is a TalosConfigTemplate. A machine set bootstrap
// configuration is not a TalosConfigTemplate, or is bootstrap data instead of a template:
// that is not an error, it is just a change this extension cannot patch, so the hook
// answers Success with no patch and Cluster API rolls the MachineSet out.
func isTalosConfigTemplate(raw runtime.RawExtension) bool {
	data := raw.Raw

	if data == nil && raw.Object != nil {
		data, _ = json.Marshal(raw.Object)
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}

	return object["kind"] == "TalosConfigTemplate"
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

// talosConfigTemplateSpec reads the TalosConfigTemplate spec of a bootstrap config template
// object of a CanUpdateMachineSet request. The Runtime SDK passes the templates as
// unstructured objects (machinedeployment_canupdatemachineset.go: createRequest,
// cleanupUnstructured), and the machine configuration of a template is the spec of its
// template, the same spec a TalosConfig carries.
func talosConfigTemplateSpec(raw runtime.RawExtension) (cabptv1.TalosConfigSpec, error) {
	spec, err := templateSpecObject(raw)
	if err != nil {
		return cabptv1.TalosConfigSpec{}, err
	}

	data, err := json.Marshal(spec)
	if err != nil {
		return cabptv1.TalosConfigSpec{}, errors.WithStack(err)
	}

	talosSpec := cabptv1.TalosConfigSpec{}
	if err := json.Unmarshal(data, &talosSpec); err != nil {
		return cabptv1.TalosConfigSpec{}, errors.Wrap(err, "decoding the TalosConfigTemplate spec of the machine set")
	}

	return talosSpec, nil
}

// templateSpecObject returns the spec.template.spec of a bootstrap config template object.
func templateSpecObject(raw runtime.RawExtension) (map[string]any, error) {
	data := raw.Raw

	if data == nil && raw.Object != nil {
		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	if len(data) == 0 {
		return nil, errors.New("the machine set has no bootstrap configuration template")
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, errors.Wrap(err, "decoding the bootstrap configuration template of the machine set")
	}

	if kind, _ := object["kind"].(string); kind != "TalosConfigTemplate" {
		return nil, errors.Errorf("the bootstrap configuration template of the machine set is a %q, not a TalosConfigTemplate", kind)
	}

	unstructuredObject := &unstructured.Unstructured{Object: object}

	spec, found, err := unstructured.NestedMap(unstructuredObject.Object, "spec", "template", "spec")
	if err != nil || !found {
		return nil, errors.New("the bootstrap configuration template of the machine set has no spec.template.spec")
	}

	return spec, nil
}

// templateSpecPatch computes the JSON patch that turns the current TalosConfigTemplate into
// the desired one. Only spec.template.spec is patched, as the hook contract requires and as
// applyPatchesToRequest of the MachineDeployment controller restricts the patch to.
func templateSpecPatch(current, desired runtime.RawExtension) ([]byte, error) {
	currentSpec, err := templateSpecObject(current)
	if err != nil {
		return nil, err
	}

	desiredSpec, err := templateSpecObject(desired)
	if err != nil {
		return nil, err
	}

	return jsonPatch(diffValue("/spec/template/spec", currentSpec, desiredSpec))
}

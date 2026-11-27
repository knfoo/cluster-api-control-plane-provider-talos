// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"github.com/siderolabs/cluster-api-control-plane-provider-talos/pkg/inplaceupdate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/feature"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// canUpdateMachine decides whether a Machine can be updated in place instead of being rolled out,
// following KubeadmControlPlane's canUpdateMachine: after the feature gate and the single-extension
// rules, the registered CanUpdateMachine extension is called with the current and the desired objects
// of the Machine, and the answer is true only when the patches the extension returns make the current
// objects equal the desired ones.
//
// Nothing calls this while the InPlaceUpdates feature gate is off, which is the default.
func (r *TalosControlPlaneReconciler) canUpdateMachine(ctx context.Context, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (bool, error) {
	if r.overrideCanUpdateMachine != nil {
		return r.overrideCanUpdateMachine(ctx, machine)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("Machine", klog.KObj(machine))

	if !feature.Gates.Enabled(feature.InPlaceUpdates) {
		return false, nil
	}

	if r.RuntimeClient == nil {
		return false, nil
	}

	// The update cannot run when it could not compute all the objects it would write, e.g. because
	// the InfraMachine or the TalosConfig was deleted. The guard runs before the extension lookup,
	// like KubeadmControlPlane's.
	if machineUpToDateResult.DesiredMachine == nil ||
		machineUpToDateResult.CurrentInfraMachine == nil ||
		machineUpToDateResult.DesiredInfraMachine == nil ||
		machineUpToDateResult.CurrentTalosConfig == nil ||
		machineUpToDateResult.DesiredTalosConfig == nil {
		return false, nil
	}

	extensionHandlers, err := r.RuntimeClient.GetAllExtensions(ctx, runtimehooksv1.CanUpdateMachine, machine)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get CanUpdateMachine extensions for Machine %s", machine.Name)
	}
	if len(extensionHandlers) == 0 {
		return false, nil
	}
	if len(extensionHandlers) > 1 {
		return false, errors.Errorf("found multiple CanUpdateMachine hooks (%s): only one hook is supported", strings.Join(extensionHandlers, ","))
	}
	handler := extensionHandlers[0]

	reason, req, err := createCanUpdateMachineRequest(ctx, r.Client, machine, machineUpToDateResult)
	if err != nil {
		return false, errors.Wrapf(err, "failed to generate CanUpdateMachine request")
	}
	// The dry-run of the InfraMachine can itself rule out an in-place update.
	if reason != "" {
		log.Info(fmt.Sprintf("Machine %s cannot be updated in-place", klog.KObj(machine)), "reason", reason)

		return false, nil
	}

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	if err := r.RuntimeClient.CallExtension(ctx, runtimehooksv1.CanUpdateMachine, machine, handler, req, resp); err != nil {
		return false, err
	}

	// Apply the patches the extension returned to the current objects of the request.
	if err := applyPatchesToRequest(req, resp); err != nil {
		return false, errors.Wrapf(err, "failed to apply patches from extension %s to the CanUpdateMachine request", handler)
	}

	canUpdate, reasons, err := matchesMachine(req)
	if err != nil {
		return false, errors.Wrapf(err, "failed to compare current and desired objects after calling extension %s", handler)
	}
	if !canUpdate {
		log.Info(fmt.Sprintf("Machine %s cannot be updated in-place by extensions", klog.KObj(machine)), "reason", strings.Join(reasons, ","))

		return false, nil
	}

	return true, nil
}

// createCanUpdateMachineRequest builds the CanUpdateMachine request from the current and desired
// objects of the Machine, cleaning them up the way KubeadmControlPlane's createRequest does: only the
// GVK, name, namespace, labels, annotations and spec are sent, and the current objects take the labels
// and annotations of the desired ones because the Machine controller syncs them and they are not part
// of the in-place update.
//
// KubeadmControlPlane additionally runs a server-side apply dry-run on the Machine and the InfraMachine
// to pick up defaulting. The TalosConfig has no defaulting webhook (no +kubebuilder:default in the
// bootstrap provider's v1alpha3 API types), so this provider compares the TalosConfig as computed. The
// InfraMachine is compared with the dry-run, like KubeadmControlPlane does: the infrastructure provider
// writes fields - e.g. providerID - on the InfraMachine that the object computed from the InfraMachine
// template does not have, and without the dry-run they read as a change the in-place update would have
// to make.
func createCanUpdateMachineRequest(ctx context.Context, c client.Client, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (string, *runtimehooksv1.CanUpdateMachineRequest, error) {
	currentTalosConfig := cleanupTalosConfig(machineUpToDateResult.CurrentTalosConfig)
	desiredTalosConfig := cleanupTalosConfig(machineUpToDateResult.DesiredTalosConfig)
	currentInfraMachine := machineUpToDateResult.CurrentInfraMachine.DeepCopy()
	desiredInfraMachine := machineUpToDateResult.DesiredInfraMachine.DeepCopy()

	// Sync the metadata the Machine controller writes from the desired to the current objects.
	currentMetadata := currentTalosConfig["metadata"].(map[string]any)
	desiredMetadata := desiredTalosConfig["metadata"].(map[string]any)
	currentMetadata["labels"] = desiredMetadata["labels"]
	currentMetadata["annotations"] = desiredMetadata["annotations"]
	currentInfraMachine.SetLabels(desiredInfraMachine.GetLabels())
	currentInfraMachine.SetAnnotations(desiredInfraMachine.GetAnnotations())

	// Apply the defaulting and the fields other controllers wrote on the InfraMachine to both sides of
	// the diff, so only a real change in the InfraMachine spec counts as a rollout.
	if err := dryRunApply(ctx, c, currentInfraMachine); err != nil {
		// A validating webhook that rejects the InfraMachine also rejects the apply the in-place update
		// would have to make, so the Machine is rolled out instead.
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			return fmt.Sprintf("server side apply dry-run for current InfraMachine failed: %s", err), nil, nil
		}

		return "", nil, errors.Wrapf(err, "server side apply dry-run failed for current InfraMachine")
	}
	if err := dryRunApply(ctx, c, desiredInfraMachine); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			return fmt.Sprintf("server side apply dry-run for desired InfraMachine failed: %s", err), nil, nil
		}

		return "", nil, errors.Wrapf(err, "server side apply dry-run failed for desired InfraMachine")
	}

	req := &runtimehooksv1.CanUpdateMachineRequest{
		Current: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine: *cleanupMachine(machine.DeepCopy()),
		},
		Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine: *cleanupMachine(machineUpToDateResult.DesiredMachine.DeepCopy()),
		},
	}

	var err error

	req.Current.BootstrapConfig, err = convertToRawExtension(currentTalosConfig)
	if err != nil {
		return "", nil, err
	}
	req.Desired.BootstrapConfig, err = convertToRawExtension(desiredTalosConfig)
	if err != nil {
		return "", nil, err
	}
	req.Current.InfrastructureMachine, err = convertToRawExtension(cleanupUnstructured(currentInfraMachine))
	if err != nil {
		return "", nil, err
	}
	req.Desired.InfrastructureMachine, err = convertToRawExtension(cleanupUnstructured(desiredInfraMachine))
	if err != nil {
		return "", nil, err
	}

	return "", req, nil
}

// dryRunApply runs a dry-run server-side apply of obj with the field manager of the TalosControlPlane
// controller, which writes the object the API server would store back into obj. Managed fields are
// dropped first because the API server rejects a request that carries them.
func dryRunApply(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	if c == nil {
		return nil
	}

	obj.SetManagedFields(nil)

	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.DryRunAll, client.FieldOwner(tcpManagerName))
}

// cleanupMachine mirrors KubeadmControlPlane's cleanupMachine.
func cleanupMachine(machine *clusterv1.Machine) *clusterv1.Machine {
	return &clusterv1.Machine{
		// Set GVK because the object is later marshalled with json.Marshal.
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        machine.Name,
			Namespace:   machine.Namespace,
			Labels:      machine.Labels,
			Annotations: machine.Annotations,
		},
		Spec: *machine.Spec.DeepCopy(),
	}
}

// cleanupTalosConfig mirrors KubeadmControlPlane's cleanupKubeadmConfig for the
// TalosConfig of the bootstrap provider: the status is dropped and only the spec is sent.
func cleanupTalosConfig(talosConfig *cabptv1.TalosConfig) map[string]any {
	return map[string]any{
		"apiVersion": cabptv1.GroupVersion.String(),
		"kind":       "TalosConfig",
		"metadata": map[string]any{
			"name":        talosConfig.Name,
			"namespace":   talosConfig.Namespace,
			"labels":      talosConfig.Labels,
			"annotations": talosConfig.Annotations,
		},
		"spec": talosConfig.Spec,
	}
}

// cleanupUnstructured mirrors KubeadmControlPlane's cleanupUnstructured.
func cleanupUnstructured(u *unstructured.Unstructured) *unstructured.Unstructured {
	cleanedUpU := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": u.GetAPIVersion(),
			"kind":       u.GetKind(),
			"spec":       u.Object["spec"],
		},
	}
	cleanedUpU.SetName(u.GetName())
	cleanedUpU.SetNamespace(u.GetNamespace())
	cleanedUpU.SetLabels(u.GetLabels())
	cleanedUpU.SetAnnotations(u.GetAnnotations())

	return cleanedUpU
}

// convertToRawExtension marshals an object into a RawExtension, the fork's copy of
// ConvertToRawExtension of sigs.k8s.io/cluster-api/internal/util/patch.
func convertToRawExtension(obj any) (runtime.RawExtension, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return runtime.RawExtension{}, errors.Wrap(err, "failed to marshal object for the CanUpdateMachine request")
	}

	return runtime.RawExtension{Raw: data}, nil
}

// applyPatchesToRequest applies the patches of the CanUpdateMachine response to the current objects
// of the request, mirroring KubeadmControlPlane's applyPatchesToRequest. Only the spec of an object
// may be patched, as the hook contract requires.
func applyPatchesToRequest(req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) error {
	if resp.MachinePatch.IsDefined() {
		if err := applySpecPatch(&req.Current.Machine, resp.MachinePatch); err != nil {
			return err
		}
	}

	if resp.BootstrapConfigPatch.IsDefined() {
		if err := applySpecPatch(&req.Current.BootstrapConfig, resp.BootstrapConfigPatch); err != nil {
			return err
		}
	}

	if resp.InfrastructureMachinePatch.IsDefined() {
		if err := applySpecPatch(&req.Current.InfrastructureMachine, resp.InfrastructureMachinePatch); err != nil {
			return err
		}
	}

	return nil
}

// applySpecPatch applies a hook patch to the JSON of one of the request's current objects. A
// runtime.RawExtension holds that JSON, a typed object is marshalled and unmarshalled around the
// patch.
func applySpecPatch(obj any, patch runtimehooksv1.Patch) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return errors.Wrap(err, "failed to marshal object before applying a patch")
	}

	patched, err := patchJSON(data, patch)
	if err != nil {
		return err
	}

	if raw, ok := obj.(*runtime.RawExtension); ok {
		raw.Raw = patched
		raw.Object = nil

		return nil
	}

	return json.Unmarshal(patched, obj)
}

// patchJSON applies an RFC 6902 or RFC 7386 patch to a JSON document, rejecting the operations that
// write outside the spec of the object, the restriction KubeadmControlPlane enforces with the allowed
// paths of its patch helper.
func patchJSON(document []byte, patch runtimehooksv1.Patch) ([]byte, error) {
	switch patch.PatchType {
	case runtimehooksv1.JSONPatchType:
		rfcPatch, err := jsonpatch.DecodePatch(patch.Patch)
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode JSON patch")
		}

		for _, op := range rfcPatch {
			path, err := op.Path()
			if err != nil {
				return nil, errors.Wrap(err, "failed to decode JSON patch")
			}

			if !strings.HasPrefix(path, "/spec") {
				return nil, errors.Errorf("patch path %q is not allowed: only the spec of an object can be patched", path)
			}
		}

		return rfcPatch.Apply(document)
	case runtimehooksv1.JSONMergePatchType:
		merge := map[string]any{}
		if err := json.Unmarshal(patch.Patch, &merge); err != nil {
			return nil, errors.Wrap(err, "failed to decode JSON merge patch")
		}

		for key := range merge {
			if key != "spec" {
				return nil, errors.Errorf("patch key %q is not allowed: only the spec of an object can be patched", key)
			}
		}

		return jsonpatch.MergePatch(document, patch.Patch)
	default:
		return nil, errors.Errorf("unsupported patch type %q", patch.PatchType)
	}
}

// matchesMachine reports whether the patched current objects of the request equal the desired ones,
// following KubeadmControlPlane's matchesMachine.
func matchesMachine(req *runtimehooksv1.CanUpdateMachineRequest) (bool, []string, error) {
	var reasons []string

	// The Machine spec is compared after cleanupMachineSpecForDiff, because fields the Machine
	// controller writes (e.g. bootstrap.dataSecretName) are never the reason for an in-place update.
	match, diff, err := specDiff(cleanupMachineSpecForDiff(&req.Current.Machine.Spec), cleanupMachineSpecForDiff(&req.Desired.Machine.Spec))
	if err != nil {
		return false, nil, errors.Wrapf(err, "failed to match Machine")
	}
	if !match {
		reasons = append(reasons, fmt.Sprintf("Machine cannot be updated in-place: %s", diff))
	}

	match, diff, err = matchRawExtensionSpec(req.Current.BootstrapConfig, req.Desired.BootstrapConfig)
	if err != nil {
		return false, nil, errors.Wrapf(err, "failed to match TalosConfig")
	}
	if !match {
		reasons = append(reasons, fmt.Sprintf("TalosConfig cannot be updated in-place: %s", diff))
	}

	match, diff, err = matchRawExtensionSpec(req.Current.InfrastructureMachine, req.Desired.InfrastructureMachine)
	if err != nil {
		return false, nil, errors.Wrapf(err, "failed to match InfrastructureMachine")
	}
	if !match {
		reasons = append(reasons, fmt.Sprintf("InfraMachine cannot be updated in-place: %s", diff))
	}

	return len(reasons) == 0, reasons, nil
}

// cleanupMachineSpecForDiff mirrors Cluster API's CleanupMachineSpecForDiff, a helper of its
// internal in-place update utility that a provider cannot import: fields that are never the reason
// for an in-place update are zeroed before the Machine spec is compared.
func cleanupMachineSpecForDiff(spec *clusterv1.MachineSpec) *clusterv1.MachineSpec {
	spec = spec.DeepCopy()

	// Should never change.
	spec.ClusterName = ""

	// Machine: should never change.
	// MachineSet: not responsibility of the in-place update extension.
	spec.Bootstrap = clusterv1.Bootstrap{}
	spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{}

	// Machine: should never change.
	// MachineSet: should not be set.
	spec.ProviderID = ""

	// Fields that are mutated in-place without a rollout.
	spec.MinReadySeconds = nil
	spec.ReadinessGates = nil
	spec.Deletion.NodeDrainTimeoutSeconds = nil
	spec.Deletion.NodeVolumeDetachTimeoutSeconds = nil
	spec.Deletion.NodeDeletionTimeoutSeconds = nil
	spec.Taints = nil

	return spec
}

// matchRawExtensionSpec compares the spec of two objects carried as RawExtension.
func matchRawExtensionSpec(patched, desired runtime.RawExtension) (bool, string, error) {
	patchedSpec, err := specOfRawExtension(patched)
	if err != nil {
		return false, "", err
	}

	desiredSpec, err := specOfRawExtension(desired)
	if err != nil {
		return false, "", err
	}

	return specDiff(patchedSpec, desiredSpec)
}

func specOfRawExtension(raw runtime.RawExtension) (map[string]any, error) {
	object := map[string]any{}
	if err := json.Unmarshal(raw.Raw, &object); err != nil {
		return nil, errors.Wrap(err, "failed to decode object")
	}

	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("object has no spec")
	}

	return spec, nil
}

func specDiff(patched, desired any) (bool, string, error) {
	diff, err := inplaceupdate.JSONPatchBetween(patched, desired)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to compute the diff between the patched and the desired spec")
	}

	if string(diff) == "[]" {
		return true, "", nil
	}

	return false, string(diff), nil
}

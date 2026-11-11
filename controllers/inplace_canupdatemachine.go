// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"github.com/siderolabs/cluster-api-control-plane-provider-talos/pkg/inplaceupdate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/feature"
	ctrl "sigs.k8s.io/controller-runtime"
)

// canUpdateMachine decides whether a Machine can be updated in place instead of being rolled out. It
// ports canUpdateMachine of KubeadmControlPlane (inplace_canupdatemachine.go:44-87): after the feature
// gate and the single-extension rules, the registered CanUpdateMachine extension is called with the
// current and the desired objects of the Machine, and the answer is true only when the patches the
// extension returns make the current objects equal the desired ones.
//
// Nothing calls this while the InPlaceUpdates feature gate is off, which is the default (ADR-0003).
func (r *TalosControlPlaneReconciler) canUpdateMachine(ctx context.Context, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (bool, error) {
	if overrideCanUpdateMachine != nil {
		return overrideCanUpdateMachine(ctx, machine)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("Machine", klog.KObj(machine))

	// Machine cannot be updated in-place if the feature gate is not enabled (inplace_canupdatemachine.go:52).
	if !feature.Gates.Enabled(feature.InPlaceUpdates) {
		return false, nil
	}

	if r.RuntimeClient == nil {
		return false, nil
	}

	// Machine cannot be updated in-place if the update could not compute all the objects it would
	// write, e.g. because the InfraMachine or the TalosConfig was deleted (inplace_canupdatemachine.go:58-64).
	// The guard runs before the extension lookup, as in the reference.
	if machineUpToDateResult.DesiredMachine == nil ||
		machineUpToDateResult.CurrentInfraMachine == nil ||
		machineUpToDateResult.DesiredInfraMachine == nil ||
		machineUpToDateResult.CurrentTalosConfig == nil ||
		machineUpToDateResult.DesiredTalosConfig == nil {
		return false, nil
	}

	// Machine cannot be updated in-place if no CanUpdateMachine extensions are registered (inplace_canupdatemachine.go:66-73).
	extensionHandlers, err := r.RuntimeClient.GetAllExtensions(ctx, runtimehooksv1.CanUpdateMachine, machine)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get CanUpdateMachine extensions for Machine %s", machine.Name)
	}
	if len(extensionHandlers) == 0 {
		return false, nil
	}
	// Only one extension per hook is supported (inplace_canupdatemachine.go:74-76).
	if len(extensionHandlers) > 1 {
		return false, errors.Errorf("found multiple CanUpdateMachine hooks (%s): only one hook is supported", strings.Join(extensionHandlers, ","))
	}

	canUpdate, reasons, err := r.canExtensionsUpdateMachine(ctx, machine, machineUpToDateResult, extensionHandlers)
	if err != nil {
		return false, err
	}
	if !canUpdate {
		log.Info(fmt.Sprintf("Machine %s cannot be updated in-place by extensions", klog.KObj(machine)), "reason", strings.Join(reasons, ","))

		return false, nil
	}

	return true, nil
}

// canExtensionsUpdateMachine calls the CanUpdateMachine extension and compares the patched current
// objects with the desired ones, following canExtensionsUpdateMachine in inplace_canupdatemachine.go:92-134.
// A failed call is an error, as in KubeadmControlPlane.
func (r *TalosControlPlaneReconciler) canExtensionsUpdateMachine(ctx context.Context, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult, extensionHandlers []string) (bool, []string, error) {
	log := ctrl.LoggerFrom(ctx)

	req, err := createCanUpdateMachineRequest(machine, machineUpToDateResult)
	if err != nil {
		return false, nil, errors.Wrapf(err, "failed to generate CanUpdateMachine request")
	}

	var reasons []string

	for _, extensionHandler := range extensionHandlers {
		resp := &runtimehooksv1.CanUpdateMachineResponse{}

		if err := r.RuntimeClient.CallExtension(ctx, runtimehooksv1.CanUpdateMachine, machine, extensionHandler, req, resp); err != nil {
			return false, nil, err
		}

		// Apply the patches the extension returned to the current objects of the request
		// (inplace_canupdatemachine.go:116-119).
		if err := applyPatchesToRequest(req, resp); err != nil {
			return false, nil, errors.Wrapf(err, "failed to apply patches from extension %s to the CanUpdateMachine request", extensionHandler)
		}

		var matches bool

		matches, reasons, err = matchesMachine(req)
		if err != nil {
			return false, nil, errors.Wrapf(err, "failed to compare current and desired objects after calling extension %s", extensionHandler)
		}
		if matches {
			return true, nil, nil
		}

		log.V(5).Info(fmt.Sprintf("Machine cannot be updated in-place yet after calling extension %s: %s", extensionHandler, strings.Join(reasons, ",")), "Machine", klog.KObj(&req.Current.Machine))
	}

	return false, reasons, nil
}

// createCanUpdateMachineRequest builds the CanUpdateMachine request from the current and desired
// objects of the Machine, cleaning them up the way createRequest in inplace_canupdatemachine.go:136-221
// does: only the GVK, name, namespace, labels, annotations and spec are sent, and the current objects
// take the labels and annotations of the desired ones because the Machine controller syncs them and
// they are not part of the in-place update (inplace_canupdatemachine.go:146-152).
//
// KubeadmControlPlane additionally runs a server-side apply dry-run on the Machine and the InfraMachine
// to pick up defaulting (inplace_canupdatemachine.go:160-185). The TalosConfig has no defaulting webhook
// (no +kubebuilder:default in the bootstrap provider's v1alpha3 API types), so the fork compares the
// TalosConfig as computed. The InfraMachine is likewise compared without a dry-run: fields the
// infrastructure provider wrote on it are matched only when the extension patches them.
func createCanUpdateMachineRequest(machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (*runtimehooksv1.CanUpdateMachineRequest, error) {
	currentMachine := cleanupMachine(machine.DeepCopy())
	desiredMachine := cleanupMachine(machineUpToDateResult.DesiredMachine.DeepCopy())

	currentTalosConfig := cleanupTalosConfig(machineUpToDateResult.CurrentTalosConfig)
	desiredTalosConfig := cleanupTalosConfig(machineUpToDateResult.DesiredTalosConfig)

	currentInfraMachine := cleanupUnstructured(machineUpToDateResult.CurrentInfraMachine.DeepCopy())
	desiredInfraMachine := cleanupUnstructured(machineUpToDateResult.DesiredInfraMachine.DeepCopy())

	// Sync the metadata the Machine controller writes from the desired to the current objects.
	currentMetadata := currentTalosConfig["metadata"].(map[string]any)
	currentMetadata["labels"] = desiredTalosConfig["metadata"].(map[string]any)["labels"]
	currentMetadata["annotations"] = desiredTalosConfig["metadata"].(map[string]any)["annotations"]
	currentInfraMachine.SetLabels(desiredInfraMachine.GetLabels())
	currentInfraMachine.SetAnnotations(desiredInfraMachine.GetAnnotations())

	req := &runtimehooksv1.CanUpdateMachineRequest{
		Current: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine: *currentMachine,
		},
		Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine: *desiredMachine,
		},
	}

	var err error

	req.Current.BootstrapConfig, err = convertToRawExtension(currentTalosConfig)
	if err != nil {
		return nil, err
	}
	req.Desired.BootstrapConfig, err = convertToRawExtension(desiredTalosConfig)
	if err != nil {
		return nil, err
	}
	req.Current.InfrastructureMachine, err = convertToRawExtension(currentInfraMachine)
	if err != nil {
		return nil, err
	}
	req.Desired.InfrastructureMachine, err = convertToRawExtension(desiredInfraMachine)
	if err != nil {
		return nil, err
	}

	return req, nil
}

// cleanupMachine mirrors cleanupMachine of inplace_canupdatemachine.go:223-238.
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

// cleanupTalosConfig mirrors cleanupKubeadmConfig of inplace_canupdatemachine.go:240-255 for the
// TalosConfig of the bootstrap provider: the status is dropped and only the spec is sent.
func cleanupTalosConfig(talosConfig *cabptv1.TalosConfig) map[string]any {
	metadata := map[string]any{
		"name":      talosConfig.Name,
		"namespace": talosConfig.Namespace,
	}
	if talosConfig.Labels != nil {
		metadata["labels"] = talosConfig.Labels
	}
	if talosConfig.Annotations != nil {
		metadata["annotations"] = talosConfig.Annotations
	}

	return map[string]any{
		"apiVersion": cabptv1.GroupVersion.String(),
		"kind":       "TalosConfig",
		"metadata":   metadata,
		"spec":       talosConfig.Spec,
	}
}

// cleanupUnstructured mirrors cleanupUnstructured of inplace_canupdatemachine.go:257-270.
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
// of the request, mirroring applyPatchesToRequest of inplace_canupdatemachine.go:272-292. Only the
// spec of an object may be patched, as the hook contract requires.
func applyPatchesToRequest(req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) error {
	if resp.MachinePatch.IsDefined() {
		if err := applyPatchToMachine(&req.Current.Machine, resp.MachinePatch); err != nil {
			return err
		}
	}

	if resp.BootstrapConfigPatch.IsDefined() {
		if err := applyPatchToRawExtension(&req.Current.BootstrapConfig, resp.BootstrapConfigPatch); err != nil {
			return err
		}
	}

	if resp.InfrastructureMachinePatch.IsDefined() {
		if err := applyPatchToRawExtension(&req.Current.InfrastructureMachine, resp.InfrastructureMachinePatch); err != nil {
			return err
		}
	}

	return nil
}

// applyPatchToMachine patches the spec of a typed object.
func applyPatchToMachine(obj any, patch runtimehooksv1.Patch) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return errors.Wrap(err, "failed to marshal object before applying a patch")
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return errors.Wrap(err, "failed to decode object before applying a patch")
	}

	if err := checkPatchOnlyTouchesSpec(patch); err != nil {
		return err
	}

	if err := applyPatch(object, patch); err != nil {
		return err
	}

	patched, err := json.Marshal(object)
	if err != nil {
		return errors.Wrap(err, "failed to marshal object after applying a patch")
	}

	return json.Unmarshal(patched, obj)
}

// applyPatchToRawExtension patches the object carried by a RawExtension.
func applyPatchToRawExtension(raw *runtime.RawExtension, patch runtimehooksv1.Patch) error {
	data := raw.Raw

	if data == nil {
		if raw.Object == nil {
			return errors.New("the object to patch is empty")
		}

		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return errors.Wrap(err, "failed to marshal object before applying a patch")
		}
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return errors.Wrap(err, "failed to decode object before applying a patch")
	}

	if err := checkPatchOnlyTouchesSpec(patch); err != nil {
		return err
	}

	if err := applyPatch(object, patch); err != nil {
		return err
	}

	patched, err := json.Marshal(object)
	if err != nil {
		return errors.Wrap(err, "failed to marshal object after applying a patch")
	}

	raw.Raw = patched
	raw.Object = nil

	return nil
}

// applyPatch dispatches on the patch type, as the hook contract defines JSONPatchType and
// JSONMergePatchType.
func applyPatch(object map[string]any, patch runtimehooksv1.Patch) error {
	switch patch.PatchType {
	case runtimehooksv1.JSONPatchType:
		return inplaceupdate.ApplyJSONPatch(object, patch.Patch)
	case runtimehooksv1.JSONMergePatchType:
		return inplaceupdate.ApplyJSONMergePatch(object, patch.Patch)
	default:
		return errors.Errorf("unsupported patch type %q", patch.PatchType)
	}
}

// checkPatchOnlyTouchesSpec rejects patches that write outside the spec of an object, the restriction
// KubeadmControlPlane enforces with the allowed paths of its patch helper
// (inplace_canupdatemachine.go:274, 281, 286).
func checkPatchOnlyTouchesSpec(patch runtimehooksv1.Patch) error {
	switch patch.PatchType {
	case runtimehooksv1.JSONPatchType:
		ops := []struct {
			Path string `json:"path"`
		}{}

		if err := json.Unmarshal(patch.Patch, &ops); err != nil {
			return errors.Wrap(err, "failed to decode JSON patch")
		}

		for _, op := range ops {
			if !strings.HasPrefix(op.Path, "/spec") {
				return errors.Errorf("patch path %q is not allowed: only the spec of an object can be patched", op.Path)
			}
		}
	case runtimehooksv1.JSONMergePatchType:
		merge := map[string]any{}
		if err := json.Unmarshal(patch.Patch, &merge); err != nil {
			return errors.Wrap(err, "failed to decode JSON merge patch")
		}

		for key := range merge {
			if key != "spec" {
				return errors.Errorf("patch key %q is not allowed: only the spec of an object can be patched", key)
			}
		}
	default:
		return errors.Errorf("unsupported patch type %q", patch.PatchType)
	}

	return nil
}

// matchesMachine reports whether the patched current objects of the request equal the desired ones,
// following matchesMachine of inplace_canupdatemachine.go:294-323.
func matchesMachine(req *runtimehooksv1.CanUpdateMachineRequest) (bool, []string, error) {
	var reasons []string

	// The Machine spec is compared after CleanupMachineSpecForDiff (inplace_canupdatemachine.go:325-335),
	// because fields the Machine controller writes (e.g. bootstrap.dataSecretName) are never the reason
	// for an in-place update.
	match, diff, err := matchSpec(cleanupMachineSpecForDiff(&req.Current.Machine.Spec), cleanupMachineSpecForDiff(&req.Desired.Machine.Spec))
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

	if len(reasons) > 0 {
		return false, reasons, nil
	}

	return true, nil, nil
}

// cleanupMachineSpecForDiff mirrors CleanupMachineSpecForDiff of
// sigs.k8s.io/cluster-api/internal/util/inplace (inplace_canupdatemachine.go:325-335): fields that are
// never the reason for an in-place update are zeroed before the Machine spec is compared.
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

// matchSpec compares two specs and returns the operations that would still be needed as the diff.
func matchSpec(patched, desired any) (bool, string, error) {
	patchedSpec, err := decodeSpec(patched)
	if err != nil {
		return false, "", err
	}

	desiredSpec, err := decodeSpec(desired)
	if err != nil {
		return false, "", err
	}

	return specDiff(patchedSpec, desiredSpec)
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
	data := raw.Raw

	if data == nil && raw.Object != nil {
		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal object")
		}
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, errors.Wrap(err, "failed to decode object")
	}

	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("object has no spec")
	}

	return spec, nil
}

func decodeSpec(obj any) (map[string]any, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal object")
	}

	spec := map[string]any{}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, errors.Wrap(err, "failed to decode object")
	}

	return spec, nil
}

func specDiff(patched, desired map[string]any) (bool, string, error) {
	patchedJSON, err := json.Marshal(patched)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to marshal patched spec")
	}

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to marshal desired spec")
	}

	if string(patchedJSON) == string(desiredJSON) {
		return true, "", nil
	}

	diff, err := inplaceupdate.JSONPatchBetween(patched, desired)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to compute the diff between the patched and the desired spec")
	}

	return false, string(diff), nil
}

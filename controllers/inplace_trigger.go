// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	capicontrollerutil "sigs.k8s.io/cluster-api/util/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// triggerInPlaceUpdate writes the desired Machine, InfraMachine and TalosConfig of a control plane
// Machine, marks all of them with the UpdateInProgressAnnotation and marks the UpdateMachine hook
// pending on the Machine, which is what the control plane contract asks of a provider that supports
// in-place updates (control-plane.md:717-722). It follows triggerInPlaceUpdate of KubeadmControlPlane
// (inplace_trigger.go:46-137).
//
// Nothing calls it while the InPlaceUpdates feature gate is off, which is the default.
func (r *TalosControlPlaneReconciler) triggerInPlaceUpdate(ctx context.Context, controlPlane *ControlPlane, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) error {
	if overrideTriggerInPlaceUpdate != nil {
		return overrideTriggerInPlaceUpdate(ctx, machine, machineUpToDateResult)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("Machine", klog.KObj(machine))
	log.Info(fmt.Sprintf("Triggering in-place update for Machine %s", klog.KObj(machine)))

	// Mark the Machine for the in-place update (inplace_trigger.go:51-63). Intentionally a MergeFrom
	// patch and not server-side apply: the Machine controller removes the annotation when the update
	// is done, and an apply would race with it over the ownership of the annotation.
	if _, ok := machine.Annotations[clusterv1.UpdateInProgressAnnotation]; !ok {
		orig := machine.DeepCopy()
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations[clusterv1.UpdateInProgressAnnotation] = ""

		if err := r.Client.Patch(ctx, machine, client.MergeFrom(orig)); err != nil {
			return errors.Wrapf(err, "failed to trigger in-place update for Machine %s by setting the %s annotation", klog.KObj(machine), clusterv1.UpdateInProgressAnnotation)
		}

		// Wait until the cache observed the Machine with the UpdateInProgressAnnotation, so that the next
		// reconcile sees it and does not trigger a second in-place update concurrently
		// (inplace_trigger.go:61-63).
		r.deferUntilCacheUpToDate(controlPlane, capicontrollerutil.StructuredObject(clusterv1.GroupVersion, "Machine"), machine.ResourceVersion)
	}

	desiredMachine := machineUpToDateResult.DesiredMachine
	desiredInfraMachine := machineUpToDateResult.DesiredInfraMachine
	desiredTalosConfig := machineUpToDateResult.DesiredTalosConfig

	// The Machine cannot be updated in-place if the desired objects could not be computed, e.g.
	// because the InfraMachine or the TalosConfig was deleted (inplace_trigger.go:86-91).
	if desiredInfraMachine == nil {
		return errors.Errorf("failed to complete triggering in-place update for Machine %s, could not compute desired InfraMachine", klog.KObj(machine))
	}
	if desiredTalosConfig == nil {
		return errors.Errorf("failed to complete triggering in-place update for Machine %s, could not compute desired TalosConfig", klog.KObj(machine))
	}

	// Write the InfraMachine first: it is the write most likely to fail (inplace_trigger.go:92-104).
	// The Machine controller waits for the UpdateInProgressAnnotation on the Machine and its related
	// objects before starting the in-place update.
	if err := r.applyOwned(ctx, withUpdateInProgressAnnotation(desiredInfraMachine)); err != nil {
		return errors.Wrapf(err, "failed to complete triggering in-place update for Machine %s", klog.KObj(machine))
	}

	// Write the TalosConfig.
	if err := r.applyOwned(ctx, withUpdateInProgressAnnotation(desiredTalosConfig)); err != nil {
		return errors.Wrapf(err, "failed to complete triggering in-place update for Machine %s", klog.KObj(machine))
	}

	// Write the Machine, with the same apply the controller uses to sync the machine template.
	if err := r.applyMachine(ctx, desiredMachine, machine); err != nil {
		return errors.Wrapf(err, "failed to complete triggering in-place update for Machine %s", klog.KObj(machine))
	}

	// Once the pending hook is written the Machine controller starts the in-place update, and the
	// UpdateMachine hook is called by the registered extension (inplace_trigger.go:127-132).
	if err := MarkAsPending(ctx, r.Client, desiredMachine, true, runtimehooksv1.UpdateMachine); err != nil {
		return errors.Wrapf(err, "failed to complete triggering in-place update for Machine %s", klog.KObj(machine))
	}

	// MarkAsPending patched the Machine, so its resource version changed. Waiting for the cache to
	// observe it keeps the next reconcile from acting on a stale Machine (inplace_trigger.go:130-134).
	r.deferUntilCacheUpToDate(controlPlane, capicontrollerutil.StructuredObject(clusterv1.GroupVersion, "Machine"), desiredMachine.ResourceVersion)

	log.Info(fmt.Sprintf("Completed triggering in-place update for Machine %s", klog.KObj(machine)))

	// The event KubeadmControlPlane emits when an in-place update is triggered (inplace_trigger.go:137).
	if r.recorder != nil {
		r.recorder.Event(machine, corev1.EventTypeNormal, "SuccessfulStartInPlaceUpdate", "Machine starting in-place update")
	}

	return nil
}

// deferUntilCacheUpToDate tells the controller not to reconcile the TalosControlPlane again before the
// cache observed the object the in-place update just wrote, mirroring KubeadmControlPlane's
// DeferNextReconcileUntilCacheUpToDate calls (inplace_trigger.go:61-63, 134).
//
// Both handles are nil when the reconciler was built without a manager, which is how the tests of this
// fork construct it, and then there is nothing to defer.
func (r *TalosControlPlaneReconciler) deferUntilCacheUpToDate(controlPlane *ControlPlane, writtenObject capicontrollerutil.GroupVersionKindType, resourceVersion string) {
	if r.controller == nil || controlPlane == nil || controlPlane.TCP == nil {
		return
	}

	r.controller.DeferNextReconcileUntilCacheUpToDate(controlPlane.TCP, writtenObject, resourceVersion)
}

// withUpdateInProgressAnnotation returns a copy of obj with the UpdateInProgressAnnotation set, keeping
// the annotations the object already carries.
func withUpdateInProgressAnnotation(obj client.Object) client.Object {
	copied, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		copied = obj
	}

	annotations := maps.Clone(copied.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[clusterv1.UpdateInProgressAnnotation] = ""
	copied.SetAnnotations(annotations)

	return copied
}

// applyOwned writes the spec and the update-in-progress annotation of an object with server-side apply
// under the field manager of the TalosControlPlane, forcing ownership of the fields it sets.
//
// The trigger owns only the spec and the UpdateInProgressAnnotation, the way KubeadmControlPlane's
// trigger writes the spec and the annotations it manages and nothing else (inplace_trigger.go:89-114).
// The machine template's labels and annotations are applied by the metadata field manager,
// capi-taloscontrolplane-metadata (machine_apply.go:138-165), so they are deliberately left out of
// this apply: the two managers never own the same fields, and a label or annotation removed from the
// template is removed from the object by the metadata manager rather than left behind by the trigger.
//
// InfraMachines and TalosConfigs are created with Create by this controller, so their spec is owned by
// the manager that created them; an apply that is not forced would conflict with it and could not
// unset fields (20240807-in-place-updates-implementation-notes.md:194-227).
func (r *TalosControlPlaneReconciler) applyOwned(ctx context.Context, obj client.Object) error {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return errors.Wrapf(err, "failed to convert %s %s to unstructured", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		gvks, _, err := r.Client.Scheme().ObjectKinds(obj)
		if err != nil {
			return errors.Wrapf(err, "failed to get the kind of %s", obj.GetName())
		}

		gvk = gvks[0]
	}

	modified := &unstructured.Unstructured{Object: map[string]any{
		"spec": content["spec"],
	}}

	modified.SetGroupVersionKind(gvk)
	modified.SetName(obj.GetName())
	modified.SetNamespace(obj.GetNamespace())
	modified.SetUID(obj.GetUID())
	modified.SetOwnerReferences(obj.GetOwnerReferences())

	// Only the update-in-progress annotation and the annotations that record the template the object was
	// cloned from are owned here; the labels and the rest of the annotations belong to the metadata field
	// manager (machine_apply.go:138-165). KubeadmControlPlane's trigger keeps the cloned-from annotations
	// for the same reason (inplace_trigger.go:92-99): rollouts match machines by their infrastructure
	// template through them.
	annotations := map[string]string{}
	for key, value := range obj.GetAnnotations() {
		if key == clusterv1.TemplateClonedFromNameAnnotation ||
			key == clusterv1.TemplateClonedFromGroupKindAnnotation ||
			key == clusterv1.UpdateInProgressAnnotation {
			annotations[key] = value
		}
	}

	if len(annotations) > 0 {
		modified.SetAnnotations(annotations)
	}

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(modified), client.ForceOwnership, client.FieldOwner(tcpManagerName)); err != nil {
		return errors.Wrapf(err, "failed to apply %s %s", gvk.Kind, obj.GetName())
	}

	return nil
}

// MarkAsPending adds the intent to call hooks to the PendingHooksAnnotation of an object and patches
// it. Cluster API keeps this helper in internal/hooks, which a provider cannot import, so the fork
// carries its own copy of MarkAsPending, MarkObjectAsPending and addToCommaSeparatedList, following
// the upstream implementation.
func MarkAsPending(ctx context.Context, c client.Client, obj client.Object, updateResourceVersionOnObject bool, hooks ...runtimecatalog.Hook) error {
	hookNames := make([]string, 0, len(hooks))
	for _, hook := range hooks {
		hookNames = append(hookNames, runtimecatalog.HookName(hook))
	}

	orig := obj.DeepCopyObject().(client.Object)

	if changed := markObjectAsPending(obj, hooks...); !changed {
		return nil
	}

	// When the caller does not want the resource version of the object it passed to change, patch a
	// copy of it instead.
	if !updateResourceVersionOnObject {
		obj = obj.DeepCopyObject().(client.Object)
	}

	if err := c.Patch(ctx, obj, client.MergeFrom(orig)); err != nil {
		return errors.Wrapf(err, "failed to mark %q hook(s) as pending", strings.Join(hookNames, ","))
	}

	return nil
}

// markObjectAsPending mirrors MarkObjectAsPending of internal/hooks and reports whether the annotation
// changed.
func markObjectAsPending(obj client.Object, hooks ...runtimecatalog.Hook) bool {
	hookNames := make([]string, 0, len(hooks))
	for _, hook := range hooks {
		hookNames = append(hookNames, runtimecatalog.HookName(hook))
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	newAnnotationValue := addToCommaSeparatedList(annotations[runtimev1beta2.PendingHooksAnnotation], hookNames...)

	if annotations[runtimev1beta2.PendingHooksAnnotation] == newAnnotationValue {
		return false
	}

	annotations[runtimev1beta2.PendingHooksAnnotation] = newAnnotationValue
	obj.SetAnnotations(annotations)

	return true
}

func addToCommaSeparatedList(original string, elems ...string) string {
	if len(elems) == 0 {
		return original
	}

	if original == "" {
		return strings.Join(elems, ",")
	}

	return original + "," + strings.Join(elems, ",")
}

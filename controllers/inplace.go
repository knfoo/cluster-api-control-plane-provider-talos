// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RuntimeClient is the part of Cluster API's Runtime SDK client that the control-plane provider needs
// to decide whether a Machine can be updated in place. Cluster API keeps its own client and extension
// registry in internal/ packages, which a provider cannot import, so the extension lookup is mirrored
// in this file: it follows GetAllExtensions in sigs.k8s.io/cluster-api/internal/runtime/client and
// GetAllExtensionConfigs in sigs.k8s.io/cluster-api/internal/runtime/registry.
type RuntimeClient interface {
	// GetAllExtensions returns the canonical names of the extension handlers registered for hook.
	GetAllExtensions(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object) ([]string, error)
}

// NewRuntimeClient returns a Runtime SDK client that reads the extensions registered in the cluster.
func NewRuntimeClient(c client.Client) RuntimeClient {
	return &extensionRuntimeClient{client: c}
}

type extensionRuntimeClient struct {
	client client.Client
}

// GetAllExtensions mirrors the upstream client of the same name: the handlers of an extension are the
// ones the Runtime SDK discovered into the status of its ExtensionConfig object.
func (c *extensionRuntimeClient) GetAllExtensions(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object) ([]string, error) {
	extensionConfigs := &runtimev1beta2.ExtensionConfigList{}
	if err := c.client.List(ctx, extensionConfigs); err != nil {
		return nil, errors.Wrap(err, "failed to list ExtensionConfig objects")
	}

	hookName := runtimecatalog.HookName(hook)

	handlers := []string{}
	for _, extensionConfig := range extensionConfigs.Items {
		// Handlers are only known after the extension was discovered by the Runtime SDK.
		if !conditions.IsTrue(&extensionConfig, runtimev1beta2.ExtensionConfigDiscoveredCondition) {
			continue
		}
		if extensionConfig.Spec.NamespaceSelector != nil && !c.namespaceMatches(ctx, extensionConfig.Spec.NamespaceSelector, forObject) {
			continue
		}
		for _, handler := range extensionConfig.Status.Handlers {
			if handler.RequestHook.Hook != hookName {
				continue
			}
			// Canonical handler name "HANDLER_NAME.EXTENSION_NAME", as in upstream NameForHandler.
			handlers = append(handlers, handler.Name+"."+extensionConfig.Name)
		}
	}

	return handlers, nil
}

func (c *extensionRuntimeClient) namespaceMatches(ctx context.Context, selector *metav1.LabelSelector, forObject client.Object) bool {
	namespace := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: forObject.GetNamespace()}, namespace); err != nil {
		return false
	}

	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false
	}

	return parsed.Matches(labels.Set(namespace.Labels))
}

// canUpdateMachine decides whether a Machine can be updated in place instead of being rolled out. It
// follows KubeadmControlPlane's canUpdateMachine (KCP reference inplace_canupdatemachine.go:52-76).
// Stage L2 only wires the Runtime SDK client and the gate/single-extension rules: the CanUpdateMachine
// hook call and the trigger come in the next stage, so a registered extension still falls back to a
// rollout. Nothing calls this while the InPlaceUpdates feature gate is off, which is the default.
func (r *TalosControlPlaneReconciler) canUpdateMachine(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
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

	// Machine cannot be updated in-place if no CanUpdateMachine extensions are registered (inplace_canupdatemachine.go:70-73).
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

	log.Info("Machine cannot be updated in-place yet: the CanUpdateMachine hook is not implemented", "extension", extensionHandlers[0])

	return false, nil
}

// UpToDateResult carries the current and desired objects of one control plane Machine that is not up
// to date, the fork's counterpart of the UpToDateResult KubeadmControlPlane builds before it decides
// between an in-place update and a rollout (inplace.go:44-53, inplace_trigger.go:81-84).
//
// A nil DesiredInfraMachine or DesiredTalosConfig means the objects the in-place update would write
// could not be computed, which makes the Machine roll out like today.
type UpToDateResult struct {
	DesiredMachine      *clusterv1.Machine
	CurrentInfraMachine *unstructured.Unstructured
	DesiredInfraMachine *unstructured.Unstructured
	CurrentTalosConfig  *cabptv1.TalosConfig
	DesiredTalosConfig  *cabptv1.TalosConfig
}

// Test seams for the in-place update path. KubeadmControlPlane keeps the equivalent fields on its
// Reconciler (inplace.go:39-41), used by its tests (inplace_test.go:40-66); the reconciler struct of
// this fork belongs to stage L2, so the seams are package level here. They are nil unless a test sets
// them, so the production path always runs canUpdateMachine and triggerInPlaceUpdate.
var (
	overrideTryInPlaceUpdate     func(ctx context.Context, controlPlane *ControlPlane, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (bool, error)
	overrideCanUpdateMachine     func(ctx context.Context, machine *clusterv1.Machine) (bool, error)
	overrideTriggerInPlaceUpdate func(ctx context.Context, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) error
)

// tryInPlaceUpdate decides between an in-place update and the scale down that rolls a Machine out,
// following tryInPlaceUpdate in inplace.go:44-53.
//
// It returns fallbackToScaleDown, which tells the caller to delete the Machine as today. Once the
// UpdateInProgressAnnotation is written the in-place update is in progress and the Machine is never
// rolled out for that change: canUpdateMachine is only called again if writing the annotation or the
// scale down itself fails.
func (r *TalosControlPlaneReconciler) tryInPlaceUpdate(
	ctx context.Context,
	controlPlane *ControlPlane,
	machineToInPlaceUpdate *clusterv1.Machine,
	machineUpToDateResult UpToDateResult,
) (bool, error) {
	if overrideTryInPlaceUpdate != nil {
		return overrideTryInPlaceUpdate(ctx, controlPlane, machineToInPlaceUpdate, machineUpToDateResult)
	}

	// A Machine cannot be updated in-place when one of the objects the update would write is missing,
	// e.g. when the InfraMachine or the TalosConfig was deleted (inplace_canupdatemachine.go:58-63).
	if machineUpToDateResult.DesiredMachine == nil ||
		machineUpToDateResult.CurrentInfraMachine == nil ||
		machineUpToDateResult.DesiredInfraMachine == nil ||
		machineUpToDateResult.CurrentTalosConfig == nil ||
		machineUpToDateResult.DesiredTalosConfig == nil {
		return true, nil
	}

	canUpdate, err := r.canUpdateMachine(ctx, machineToInPlaceUpdate)
	if err != nil {
		return false, errors.Wrapf(err, "failed to determine if Machine %s can be updated in-place", machineToInPlaceUpdate.Name)
	}

	if !canUpdate {
		return true, nil
	}

	return false, r.triggerInPlaceUpdate(ctx, machineToInPlaceUpdate, machineUpToDateResult)
}

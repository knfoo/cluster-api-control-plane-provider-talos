// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

const (
	inPlaceInfraGroupVersion = "infrastructure.cluster.x-k8s.io/v1beta1"
	inPlaceInfraMachineKind  = "GenericInfrastructureMachine"
	inPlaceInfraTemplateKind = "GenericInfrastructureMachineTemplate"
)

// inPlaceUpdateObjects returns the Machine, its InfraMachine and its TalosConfig, and the
// UpToDateResult of a TalosConfig-only change: the desired Machine, InfraMachine and TalosConfig an
// in-place update writes while the Machine is kept.
func inPlaceUpdateObjects() (*clusterv1.Machine, *unstructured.Unstructured, *cabptv1.TalosConfig, UpToDateResult, error) {
	tcp := &controlplanev1.TalosControlPlane{
		TypeMeta: metav1.TypeMeta{Kind: "TalosControlPlane", APIVersion: controlplanev1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-control-plane",
			Namespace: metav1.NamespaceDefault,
			UID:       "tcp-uid",
		},
		Spec: controlplanev1.TalosControlPlaneSpec{
			Version:  "v1.30.0",
			Replicas: ptr.To[int32](3),
			ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					Data: "in-place updated talosconfig",
				},
			},
		},
	}

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{Kind: "Machine", APIVersion: clusterv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "cluster-1",
			},
			Annotations: map[string]string{
				"annotation-1": "annotation-value-1",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(tcp, controlplanev1.GroupVersion.WithKind("TalosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "cluster-1",
			Version:     "v1.30.0",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io",
				Kind:     inPlaceInfraMachineKind,
				Name:     "machine-to-in-place-update",
			},
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: cabptv1.GroupVersion.Group,
					Kind:     "TalosConfig",
					Name:     "machine-to-in-place-update",
				},
			},
		},
		Status: clusterv1.MachineStatus{
			NodeRef: clusterv1.MachineNodeReference{Name: "machine-to-in-place-update"},
		},
	}

	currentInfraMachine := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": inPlaceInfraGroupVersion,
			"kind":       inPlaceInfraMachineKind,
			"metadata": map[string]interface{}{
				"name":      "machine-to-in-place-update",
				"namespace": metav1.NamespaceDefault,
				"annotations": map[string]interface{}{
					clusterv1.TemplateClonedFromNameAnnotation:      "infra-machine-template",
					clusterv1.TemplateClonedFromGroupKindAnnotation: inPlaceInfraTemplateKind + ".infrastructure.cluster.x-k8s.io",
				},
			},
			"spec": map[string]interface{}{
				"foo": "hello world",
			},
		},
	}

	// The desired InfraMachine is the one computeDesiredInfraMachine builds from the InfraMachine
	// template of the control plane: the same object, cloned from the same template, with the changed
	// spec.
	desiredInfraMachine := currentInfraMachine.DeepCopy()
	if err := unstructured.SetNestedField(desiredInfraMachine.Object, "hello in-place updated world", "spec", "foo"); err != nil {
		return nil, nil, nil, UpToDateResult{}, err
	}

	currentTalosConfig := &cabptv1.TalosConfig{
		TypeMeta: metav1.TypeMeta{Kind: "TalosConfig", APIVersion: cabptv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: cabptv1.TalosConfigSpec{
			Data: "old talosconfig",
		},
	}

	desiredTalosConfig := currentTalosConfig.DeepCopy()
	desiredTalosConfig.Spec = tcp.Spec.ControlPlaneConfig.ControlPlaneConfig

	desiredMachine := machine.DeepCopy()
	desiredMachine.Spec.MinReadySeconds = ptr.To[int32](10)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, nil, UpToDateResult{}, err
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, UpToDateResult{}, err
	}
	if err := controlplanev1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, UpToDateResult{}, err
	}
	if err := cabptv1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, UpToDateResult{}, err
	}

	return machine, currentInfraMachine, currentTalosConfig, UpToDateResult{
		DesiredMachine:      desiredMachine,
		CurrentInfraMachine: currentInfraMachine,
		DesiredInfraMachine: desiredInfraMachine,
		CurrentTalosConfig:  currentTalosConfig,
		DesiredTalosConfig:  desiredTalosConfig,
	}, nil
}

// inPlaceUpdateScheme returns the scheme the in-place update path needs: the Cluster API core and
// runtime types, the TalosControlPlane and the TalosConfig.
func inPlaceUpdateScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := runtimev1beta2.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := controlplanev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := cabptv1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return scheme, nil
}

// Test_triggerInPlaceUpdate mirrors the trigger test of KubeadmControlPlane (inplace_trigger_test.go):
// the Machine, its InfraMachine and its TalosConfig carry the UpdateInProgressAnnotation, the
// UpdateMachine hook is marked pending on the Machine, and no Machine is deleted.
func Test_triggerInPlaceUpdate(t *testing.T) {
	tests := []struct {
		name                string
		withoutInfraMachine bool
		withoutTalosConfig  bool
		wantError           bool
		wantErrorMessage    string
	}{
		{
			name: "Trigger the in-place update",
		},
		{
			name:                "Return error if the desired InfraMachine is nil",
			withoutInfraMachine: true,
			wantError:           true,
			wantErrorMessage:    "failed to complete triggering in-place update for Machine default/machine-to-in-place-update, could not compute desired InfraMachine",
		},
		{
			name:               "Return error if the desired TalosConfig is nil",
			withoutTalosConfig: true,
			wantError:          true,
			wantErrorMessage:   "failed to complete triggering in-place update for Machine default/machine-to-in-place-update, could not compute desired TalosConfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			machine, infraMachine, talosConfig, result, err := inPlaceUpdateObjects()
			g.Expect(err).ToNot(HaveOccurred())

			if tt.withoutInfraMachine {
				result.DesiredInfraMachine = nil
			}
			if tt.withoutTalosConfig {
				result.DesiredTalosConfig = nil
			}

			scheme, err := inPlaceUpdateScheme()
			g.Expect(err).ToNot(HaveOccurred())

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(machine.DeepCopy(), infraMachine.DeepCopy(), talosConfig.DeepCopy()).
				Build()

			r := &TalosControlPlaneReconciler{Client: c}

			err = r.triggerInPlaceUpdate(ctx, machine, result)
			if tt.wantError {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tt.wantErrorMessage))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())

			gotMachine := &clusterv1.Machine{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), gotMachine)).To(Succeed())
			g.Expect(gotMachine.DeletionTimestamp).To(BeNil())
			g.Expect(gotMachine.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
			g.Expect(gotMachine.Annotations[runtimev1beta2.PendingHooksAnnotation]).To(Equal("UpdateMachine"))
			g.Expect(gotMachine.Spec.MinReadySeconds).To(BeComparableTo(ptr.To[int32](10)))

			gotInfraMachine := &unstructured.Unstructured{}
			gotInfraMachine.SetGroupVersionKind(infraMachine.GroupVersionKind())
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(infraMachine), gotInfraMachine)).To(Succeed())
			g.Expect(gotInfraMachine.GetAnnotations()[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
			g.Expect(gotInfraMachine.GetAnnotations()[clusterv1.TemplateClonedFromNameAnnotation]).To(Equal("infra-machine-template"))
			g.Expect(gotInfraMachine.Object["spec"]).To(BeComparableTo(result.DesiredInfraMachine.Object["spec"]))

			gotTalosConfig := &cabptv1.TalosConfig{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(talosConfig), gotTalosConfig)).To(Succeed())
			g.Expect(gotTalosConfig.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
			g.Expect(gotTalosConfig.Spec).To(BeComparableTo(result.DesiredTalosConfig.Spec))
		})
	}
}

// Test_MarkAsPending checks the copy of Cluster API's internal hook helper the fork carries: the hook
// is added to the PendingHooksAnnotation of the object, and several hooks are comma separated.
func Test_MarkAsPending(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-with-pending-hooks",
			Namespace: metav1.NamespaceDefault,
		},
	}

	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()

	g.Expect(MarkAsPending(ctx, c, machine, true, runtimehooksv1.UpdateMachine)).To(Succeed())

	gotMachine := &clusterv1.Machine{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), gotMachine)).To(Succeed())
	g.Expect(gotMachine.Annotations[runtimev1beta2.PendingHooksAnnotation]).To(Equal("UpdateMachine"))

	g.Expect(MarkAsPending(ctx, c, gotMachine, true, runtimehooksv1.CanUpdateMachine)).To(Succeed())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), gotMachine)).To(Succeed())
	g.Expect(gotMachine.Annotations[runtimev1beta2.PendingHooksAnnotation]).To(Equal("UpdateMachine,CanUpdateMachine"))
}

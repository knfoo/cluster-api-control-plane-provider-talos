// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	utilfeature "k8s.io/component-base/featuregate/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

// upToDateControlPlane returns a Machine, its InfraMachine, its TalosConfig and the ControlPlane of a
// control plane whose Kubernetes version is controlPlaneVersion.
func upToDateControlPlane(controlPlaneVersion string) (*ControlPlane, *clusterv1.Machine) {
	tcp := &controlplanev1.TalosControlPlane{
		TypeMeta: metav1.TypeMeta{Kind: "TalosControlPlane", APIVersion: controlplanev1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-control-plane",
			Namespace: metav1.NamespaceDefault,
			UID:       "tcp-uid",
		},
		Spec: controlplanev1.TalosControlPlaneSpec{
			Version:  controlPlaneVersion,
			Replicas: ptr.To[int32](1),
			ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					Data: "talosconfig of the control plane",
				},
			},
		},
	}

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{Kind: "Machine", APIVersion: clusterv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "control-plane-machine",
			Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "cluster-1",
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
				Name:     "control-plane-machine",
			},
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: cabptv1.GroupVersion.Group,
					Kind:     "TalosConfig",
					Name:     "control-plane-machine",
				},
			},
		},
	}

	infraMachine := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": inPlaceInfraGroupVersion,
			"kind":       inPlaceInfraMachineKind,
			"metadata": map[string]interface{}{
				"name":      "control-plane-machine",
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

	infraTemplate := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": inPlaceInfraGroupVersion,
			"kind":       inPlaceInfraTemplateKind,
			"metadata": map[string]interface{}{
				"name":      "infra-machine-template",
				"namespace": metav1.NamespaceDefault,
			},
			"spec": map[string]interface{}{
				"foo": "hello world",
			},
		},
	}

	talosConfig := &cabptv1.TalosConfig{
		TypeMeta: metav1.TypeMeta{Kind: "TalosConfig", APIVersion: cabptv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "control-plane-machine",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: cabptv1.TalosConfigSpec{
			Data: "talosconfig of the control plane",
		},
	}

	return &ControlPlane{
		TCP:           tcp,
		Cluster:       &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: metav1.NamespaceDefault}},
		Machines:      map[string]*clusterv1.Machine{machine.Name: machine},
		infraObjects:  map[string]*unstructured.Unstructured{machine.Name: infraMachine},
		talosConfigs:  map[string]*cabptv1.TalosConfig{machine.Name: talosConfig},
		infraTemplate: infraTemplate,
	}, machine
}

// Test_inPlaceUpdateResultCarriesTheControlPlaneVersion checks that the desired Machine of an in-place
// update has the Kubernetes version of the TalosControlPlane. computeDesiredMachine keeps the version
// of the existing Machine, so without the version of the control plane the in-place update writes the
// Machine unchanged: canUpdateMachine then reports the Machine as up to date, the trigger writes
// nothing, and the Machine never gets the new Kubernetes version.
func Test_inPlaceUpdateResultCarriesTheControlPlaneVersion(t *testing.T) {
	g := NewWithT(t)

	controlPlane, machine := upToDateControlPlane("v1.31.0")

	result, err := controlPlane.inPlaceUpdateResult(machine)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.DesiredMachine).ToNot(BeNil())
	g.Expect(result.DesiredMachine.Spec.Version).To(Equal("v1.31.0"))
	g.Expect(result.DesiredMachine.Spec.InfrastructureRef).To(Equal(machine.Spec.InfrastructureRef))
	g.Expect(result.DesiredInfraMachine).ToNot(BeNil())
	g.Expect(result.DesiredTalosConfig).ToNot(BeNil())
}

// Test_inPlaceUpdateResultKeepsTheVersionOfAnUpToDateMachine checks that a Machine that already has the
// Kubernetes version of the control plane keeps it.
func Test_inPlaceUpdateResultKeepsTheVersionOfAnUpToDateMachine(t *testing.T) {
	g := NewWithT(t)

	controlPlane, machine := upToDateControlPlane("v1.30.0")

	result, err := controlPlane.inPlaceUpdateResult(machine)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.DesiredMachine).ToNot(BeNil())
	g.Expect(result.DesiredMachine.Spec.Version).To(Equal(machine.Spec.Version))
}

// Test_tryInPlaceUpdateWritesTheControlPlaneVersionToTheMachine reproduces the failure of e2e run
// a0892e4-03356e7 end to end, without envtest, the way KubeadmControlPlane tests the decision in
// inplace_test.go: a Machine of a control plane that has a newer Kubernetes version than the Machine,
// the fake client holding the Machine and its objects, and the test seams of the in-place path
// (inplace.go:403-406) standing in for the extension, which patches the Machine's version when the
// request carries a Kubernetes version change (pkg/inplaceupdate/handlers.go:181, 235).
//
// Without the fix the desired Machine has the version of the existing Machine: the extension sees no
// change, canUpdateMachine reports the Machine as up to date, the trigger writes nothing and the
// reconcile repeats forever.
func Test_tryInPlaceUpdateWritesTheControlPlaneVersionToTheMachine(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	ctx := context.Background()
	controlPlane, machine := upToDateControlPlane("v1.31.0")

	result, err := controlPlane.inPlaceUpdateResult(machine)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.DesiredMachine.Spec.Version).ToNot(Equal(machine.Spec.Version))

	scheme, err := inPlaceUpdateScheme()
	g.Expect(err).ToNot(HaveOccurred())

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine.DeepCopy(), result.CurrentInfraMachine, result.CurrentTalosConfig).
		Build()

	r := &TalosControlPlaneReconciler{
		Client: c,
		RuntimeClient: &hookRuntimeClient{
			handlers: []string{"talos-update.canupdatemachine"},
			response: runtimehooksv1.CanUpdateMachineResponse{
				MachinePatch: jsonPatch(fmt.Sprintf(`[{"op":"replace","path":"/spec/version","value":"%s"}]`, result.DesiredMachine.Spec.Version)),
			},
		},
	}

	fallbackToScaleDown, err := r.tryInPlaceUpdate(ctx, controlPlane, machine, result)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fallbackToScaleDown).To(BeFalse())

	gotMachine := &clusterv1.Machine{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), gotMachine)).To(Succeed())
	g.Expect(gotMachine.Spec.Version).To(Equal("v1.31.0"))
	g.Expect(gotMachine.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	g.Expect(gotMachine.Annotations[runtimev1beta2.PendingHooksAnnotation]).To(Equal("UpdateMachine"))
}

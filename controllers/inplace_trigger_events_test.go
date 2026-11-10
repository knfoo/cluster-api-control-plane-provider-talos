// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capicontrollerutil "sigs.k8s.io/cluster-api/util/controller"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

// deferredWrite records one DeferNextReconcileUntilCacheUpToDate call, the cache wait
// KubeadmControlPlane performs after an in-place update is triggered (inplace_trigger.go:61-63, 134).
type deferredWrite struct {
	reconciledObject string
	writtenKind      string
	resourceVersion  string
}

// recordingController stands in for the controller handle of the TalosControlPlane controller and
// records the deferrals the in-place update path asks for.
type recordingController struct {
	*capicontrollerutil.FakeController

	deferred []deferredWrite
}

func (c *recordingController) DeferNextReconcileUntilCacheUpToDate(reconciledObject metav1.Object, writtenObject capicontrollerutil.GroupVersionKindType, resourceVersion string) {
	c.deferred = append(c.deferred, deferredWrite{
		reconciledObject: reconciledObject.GetNamespace() + "/" + reconciledObject.GetName(),
		writtenKind:      writtenObject.Kind,
		resourceVersion:  resourceVersion,
	})
}

// Test_triggerInPlaceUpdateEventAndCacheWait checks the two parts KubeadmControlPlane adds around the
// trigger writes (inplace_trigger.go:61-63, 134-137): the SuccessfulStartInPlaceUpdate event and the
// wait for the cache to observe the Machine the trigger wrote.
func Test_triggerInPlaceUpdateEventAndCacheWait(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	machine, infraMachine, talosConfig, result, err := inPlaceUpdateObjects()
	g.Expect(err).NotTo(HaveOccurred())

	tcp := &controlplanev1.TalosControlPlane{
		TypeMeta: metav1.TypeMeta{Kind: "TalosControlPlane", APIVersion: controlplanev1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-control-plane",
			Namespace: metav1.NamespaceDefault,
			UID:       "tcp-uid",
		},
	}

	scheme, err := inPlaceUpdateScheme()
	g.Expect(err).NotTo(HaveOccurred())

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine.DeepCopy(), infraMachine.DeepCopy(), talosConfig.DeepCopy(), tcp.DeepCopy()).
		Build()

	recorder := record.NewFakeRecorder(10)
	controller := &recordingController{FakeController: capicontrollerutil.NewFakeController()}

	r := &TalosControlPlaneReconciler{
		Client:     c,
		recorder:   recorder,
		controller: controller,
	}

	controlPlane := &ControlPlane{TCP: tcp, Machines: map[string]*clusterv1.Machine{machine.Name: machine}}

	g.Expect(r.triggerInPlaceUpdate(ctx, controlPlane, machine, result)).To(Succeed())

	// The event KubeadmControlPlane emits when the in-place update is triggered.
	event := <-recorder.Events
	g.Expect(event).To(ContainSubstring("Normal"))
	g.Expect(event).To(ContainSubstring("SuccessfulStartInPlaceUpdate"))
	g.Expect(event).To(ContainSubstring("Machine starting in-place update"))

	// One deferral after the UpdateInProgressAnnotation patch and one after the pending hook is
	// written, both for the Machine the trigger wrote.
	g.Expect(controller.deferred).To(HaveLen(2))
	g.Expect(controller.deferred[0].writtenKind).To(Equal("Machine"))
	g.Expect(controller.deferred[1].writtenKind).To(Equal("Machine"))
	g.Expect(controller.deferred[0].reconciledObject).To(Equal(metav1.NamespaceDefault + "/talos-control-plane"))
	g.Expect(controller.deferred[1].resourceVersion).NotTo(BeEmpty())

	// A trigger without a reconciled TalosControlPlane or without a controller handle cannot defer
	// anything and must not fail.
	rWithoutHandles := &TalosControlPlaneReconciler{Client: c}
	g.Expect(rWithoutHandles.triggerInPlaceUpdate(ctx, nil, machine.DeepCopy(), result)).To(Succeed())
}

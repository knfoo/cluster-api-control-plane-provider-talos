// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/envtest"

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

			err = r.triggerInPlaceUpdate(ctx, nil, machine, result)
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

// TestTriggerInPlaceUpdateLeavesLabelsToMetadataManager checks that the in-place trigger owns only the
// spec and the update-in-progress annotation (issue #67). The machine template's labels and annotations
// stay owned by the metadata field manager capi-taloscontrolplane-metadata (machine_apply.go:138-165),
// so a label removed from the machine template and dropped by applyMetadata is gone from the InfraMachine
// and the TalosConfig after an in-place update, instead of being kept by the trigger's field manager.
// envtest is required because the fake client does not track managed fields.
func TestTriggerInPlaceUpdateLeavesLabelsToMetadataManager(t *testing.T) {
	g := NewWithT(t)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			crdDir(t),
			filepath.Join("..", "config", "crd", "bases"),
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		if os.Getenv("KUBEBUILDER_ASSETS") == "" || strings.Contains(err.Error(), "no such file or directory") {
			t.Skipf("envtest binaries are not available: %v", err)
		}

		t.Fatalf("failed to start envtest: %v", err)
	}

	t.Cleanup(func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			t.Logf("failed to stop envtest: %v", stopErr)
		}
	})

	scheme, err := inPlaceUpdateScheme()
	g.Expect(err).ToNot(HaveOccurred())

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).ToNot(HaveOccurred())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machine, infraMachine, talosConfig, result, err := inPlaceUpdateObjects()
	g.Expect(err).ToNot(HaveOccurred())

	// The machine template's labels and annotations, one of each the test removes later.
	templateLabels := map[string]string{
		clusterv1.ClusterNameLabel: "cluster-1",
		"label-to-remove":         "value",
		"label-kept":              "value",
	}
	templateAnnotations := map[string]string{
		"annotation-1":         "annotation-value-1",
		"annotation-to-remove": "value",
	}

	r := &TalosControlPlaneReconciler{Client: c, Scheme: scheme}

	result.DesiredInfraMachine.SetLabels(templateLabels)
	result.DesiredInfraMachine.SetAnnotations(map[string]string{
		clusterv1.TemplateClonedFromNameAnnotation:      "infra-machine-template",
		clusterv1.TemplateClonedFromGroupKindAnnotation: inPlaceInfraTemplateKind + ".infrastructure.cluster.x-k8s.io",
		"annotation-1":                                  "annotation-value-1",
		"annotation-to-remove":                          "value",
	})
	result.DesiredTalosConfig.Labels = templateLabels
	result.DesiredTalosConfig.Annotations = templateAnnotations

	g.Expect(c.Create(ctx, machine.DeepCopy())).To(Succeed())

	// Create the InfraMachine and the TalosConfig the way the controller does: the spec under the
	// TalosControlPlane field manager, the machine template's labels and annotations under the metadata
	// field manager (machine_apply.go:138-165).
	infraForCreate := infraMachine.DeepCopy()
	infraForCreate.SetLabels(nil)
	infraForCreate.SetAnnotations(withoutKeys(infraForCreate.GetAnnotations(), templateAnnotations))
	g.Expect(r.applyOwned(ctx, infraForCreate)).To(Succeed())
	g.Expect(r.applyMetadata(ctx, infraForCreate, templateLabels, templateAnnotations)).To(Succeed())

	talosForCreate := talosConfig.DeepCopy()
	talosForCreate.Labels = nil
	talosForCreate.Annotations = nil
	g.Expect(r.applyOwned(ctx, talosForCreate)).To(Succeed())
	g.Expect(r.applyMetadata(ctx, talosForCreate, templateLabels, templateAnnotations)).To(Succeed())

	// Trigger the in-place update.
	g.Expect(r.triggerInPlaceUpdate(ctx, nil, machine.DeepCopy(), result)).To(Succeed())

	// After the trigger, the TalosControlPlane field manager owns the spec, the update-in-progress
	// annotation and the annotations that record the infrastructure template the object was cloned from,
	// and no labels or other annotations.
	gotInfra := &unstructured.Unstructured{}
	gotInfra.SetGroupVersionKind(infraMachine.GroupVersionKind())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(infraMachine), gotInfra)).To(Succeed())
	g.Expect(gotInfra.Object["spec"]).To(BeComparableTo(result.DesiredInfraMachine.Object["spec"]))
	g.Expect(gotInfra.GetAnnotations()[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	g.Expect(gotInfra.GetAnnotations()[clusterv1.TemplateClonedFromNameAnnotation]).To(Equal("infra-machine-template"))
	owned := fieldsOwnedBy(gotInfra, tcpManagerName)
	g.Expect(owned["f:spec"]).ToNot(BeNil())
	metadata, _ := owned["f:metadata"].(map[string]any)
	g.Expect(metadata).ToNot(BeNil())
	g.Expect(metadata["f:labels"]).To(BeNil())
	annotations, _ := metadata["f:annotations"].(map[string]any)
	g.Expect(annotations["f:"+clusterv1.UpdateInProgressAnnotation]).ToNot(BeNil())
	g.Expect(annotations["f:"+clusterv1.TemplateClonedFromNameAnnotation]).ToNot(BeNil())
	for key := range templateAnnotations {
		g.Expect(annotations["f:"+key]).To(BeNil())
	}

	gotTalosConfig := &cabptv1.TalosConfig{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(talosConfig), gotTalosConfig)).To(Succeed())
	g.Expect(gotTalosConfig.Spec).To(BeComparableTo(result.DesiredTalosConfig.Spec))
	g.Expect(gotTalosConfig.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	owned = fieldsOwnedBy(gotTalosConfig, tcpManagerName)
	g.Expect(owned["f:spec"]).ToNot(BeNil())
	metadata, _ = owned["f:metadata"].(map[string]any)
	g.Expect(metadata).ToNot(BeNil())
	g.Expect(metadata["f:labels"]).To(BeNil())
	annotations, _ = metadata["f:annotations"].(map[string]any)
	g.Expect(annotations["f:"+clusterv1.UpdateInProgressAnnotation]).ToNot(BeNil())
	for key := range templateAnnotations {
		g.Expect(annotations["f:"+key]).To(BeNil())
	}

	// Remove a label and an annotation from the machine template and apply the metadata again: the keys
	// the trigger never owned are removed from both objects.
	reducedLabels := map[string]string{
		clusterv1.ClusterNameLabel: "cluster-1",
		"label-kept":              "value",
	}
	reducedAnnotations := map[string]string{
		"annotation-1": "annotation-value-1",
	}

	g.Expect(r.applyMetadata(ctx, gotInfra, reducedLabels, reducedAnnotations)).To(Succeed())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(infraMachine), gotInfra)).To(Succeed())
	g.Expect(gotInfra.GetLabels()).To(Equal(reducedLabels))
	g.Expect(gotInfra.GetAnnotations()).To(Equal(map[string]string{
		"annotation-1":                                    "annotation-value-1",
		clusterv1.UpdateInProgressAnnotation:              "",
		clusterv1.TemplateClonedFromNameAnnotation:        "infra-machine-template",
		clusterv1.TemplateClonedFromGroupKindAnnotation:   inPlaceInfraTemplateKind + ".infrastructure.cluster.x-k8s.io",
	}))

	g.Expect(r.applyMetadata(ctx, gotTalosConfig, reducedLabels, reducedAnnotations)).To(Succeed())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(talosConfig), gotTalosConfig)).To(Succeed())
	g.Expect(gotTalosConfig.Labels).To(Equal(reducedLabels))
	g.Expect(gotTalosConfig.Annotations).To(Equal(map[string]string{
		"annotation-1":                       "annotation-value-1",
		clusterv1.UpdateInProgressAnnotation: "",
	}))
}

// fieldsOwnedBy returns the fields of an object owned by the given apply field manager.
func fieldsOwnedBy(obj client.Object, manager string) map[string]any {
	merged := map[string]any{}
	for _, managedField := range obj.GetManagedFields() {
		if managedField.Manager != manager ||
			managedField.Operation != metav1.ManagedFieldsOperationApply ||
			managedField.Subresource != "" {
			continue
		}

		var fields map[string]any
		if err := json.Unmarshal(managedField.FieldsV1.GetRawBytes(), &fields); err != nil {
			continue
		}

		for key, value := range fields {
			merged[key] = value
		}
	}

	return merged
}

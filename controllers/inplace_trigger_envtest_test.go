// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

// The CRDs the scenario needs. CACPPT only ships its own CRDs in config/crd/bases, so the CRDs of the
// objects an in-place update writes - the Machine, its InfraMachine, its TalosConfig and the stub
// extension - are defined here with a permissive schema.
const inPlaceUpdateCRDs = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: machines.cluster.x-k8s.io
spec:
  group: cluster.x-k8s.io
  names:
    kind: Machine
    listKind: MachineList
    plural: machines
    singular: machine
  scope: Namespaced
  versions:
    - name: v1beta2
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: talosconfigs.bootstrap.cluster.x-k8s.io
spec:
  group: bootstrap.cluster.x-k8s.io
  names:
    kind: TalosConfig
    listKind: TalosConfigList
    plural: talosconfigs
    singular: talosconfig
  scope: Namespaced
  versions:
    - name: v1alpha3
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: genericinfrastructuremachines.infrastructure.cluster.x-k8s.io
spec:
  group: infrastructure.cluster.x-k8s.io
  names:
    kind: GenericInfrastructureMachine
    listKind: GenericInfrastructureMachineList
    plural: genericinfrastructuremachines
    singular: genericinfrastructuremachine
  scope: Namespaced
  versions:
    - name: v1beta1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: extensionconfigs.runtime.cluster.x-k8s.io
spec:
  group: runtime.cluster.x-k8s.io
  names:
    kind: ExtensionConfig
    listKind: ExtensionConfigList
    plural: extensionconfigs
    singular: extensionconfig
  scope: Namespaced
  versions:
    - name: v1beta2
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
`

// Test_inPlaceUpdateEnvtest is the stage's end-to-end scenario against a real API server: an
// ExtensionConfig stubs the in-place update extension, a TalosConfig-only change annotates the Machine,
// its InfraMachine and its TalosConfig, marks the UpdateMachine hook pending on the Machine, and deletes
// no Machine.
//
// The real API server is what makes this more than the unit test: it enforces server-side apply
// ownership, which the InfraMachine and the TalosConfig need before an in-place update can change them
// (implementation notes 194-227).
func Test_inPlaceUpdateEnvtest(t *testing.T) {
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

	tcp := &controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-control-plane",
			Namespace: metav1.NamespaceDefault,
			UID:       "tcp-uid",
		},
		Spec: controlplanev1.TalosControlPlaneSpec{
			Version:  "v1.30.0",
			Replicas: ptr.To[int32](3),
		},
	}
	g.Expect(c.Create(ctx, tcp)).To(Succeed())

	machine, infraMachine, talosConfig, result, err := inPlaceUpdateObjects()
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(c.Create(ctx, machine.DeepCopy())).To(Succeed())
	g.Expect(c.Create(ctx, infraMachine.DeepCopy())).To(Succeed())
	g.Expect(c.Create(ctx, talosConfig.DeepCopy())).To(Succeed())

	// The stub extension is what the in-place update path would discover: an extension the Runtime SDK
	// has discovered, with a handler for the UpdateMachine hook.
	extensionConfig := &runtimev1beta2.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-updater",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: runtimev1beta2.ExtensionConfigSpec{
			ClientConfig: runtimev1beta2.ClientConfig{URL: "https://talos-updater.cluster.x-k8s.io/api"},
		},
		Status: runtimev1beta2.ExtensionConfigStatus{
			Conditions: []metav1.Condition{
				{
					Type:   runtimev1beta2.ExtensionConfigDiscoveredCondition,
					Status: metav1.ConditionTrue,
					Reason: runtimev1beta2.ExtensionConfigDiscoveredReason,
				},
			},
			Handlers: []runtimev1beta2.ExtensionHandler{
				{
					Name: "update-machine",
					RequestHook: runtimev1beta2.GroupVersionHook{
						APIVersion: runtimehooksv1.GroupVersion.String(),
						Hook:       "UpdateMachine",
					},
				},
			},
		},
	}
	g.Expect(c.Create(ctx, extensionConfig)).To(Succeed())

	r := &TalosControlPlaneReconciler{Client: c, Scheme: scheme}

	// The discovered extension makes the CanUpdateMachine hook answer yes; the hook call itself is the
	// next stage, so the override seam stands in for the extension's reply, as in KubeadmControlPlane's
	// tests (inplace_test.go:40-66).
	overrideCanUpdateMachine = func(_ context.Context, _ *clusterv1.Machine) (bool, error) {
		return true, nil
	}
	defer func() { overrideCanUpdateMachine = nil }()

	fallbackToScaleDown, err := r.tryInPlaceUpdate(ctx, nil, machine, result)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fallbackToScaleDown).To(BeFalse())

	// The stub extension is discoverable through the Runtime SDK client the fork carries.
	handlers, err := NewRuntimeClient(c).GetAllExtensions(ctx, runtimehooksv1.UpdateMachine, machine)
	g.Expect(err).ToNot(HaveOccurred())
	extensionList := &runtimev1beta2.ExtensionConfigList{}
	g.Expect(c.List(ctx, extensionList)).To(Succeed())
	t.Logf("extension configs: %+v", extensionList.Items)
	g.Expect(handlers).To(ConsistOf("update-machine.talos-updater"))

	// No Machine is deleted: it is kept, annotated, and the UpdateMachine hook is pending on it.
	gotMachine := &clusterv1.Machine{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), gotMachine)).To(Succeed())
	g.Expect(gotMachine.DeletionTimestamp).To(BeNil())
	g.Expect(gotMachine.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	g.Expect(gotMachine.Annotations[runtimev1beta2.PendingHooksAnnotation]).To(Equal("UpdateMachine"))

	gotInfraMachine := &unstructured.Unstructured{}
	gotInfraMachine.SetGroupVersionKind(infraMachine.GroupVersionKind())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(infraMachine), gotInfraMachine)).To(Succeed())
	g.Expect(gotInfraMachine.GetAnnotations()[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	g.Expect(gotInfraMachine.Object["spec"]).To(BeComparableTo(result.DesiredInfraMachine.Object["spec"]))
	g.Expect(managersOf(gotInfraMachine)).To(ContainElement(tcpManagerName))

	gotTalosConfig := &cabptv1.TalosConfig{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(talosConfig), gotTalosConfig)).To(Succeed())
	g.Expect(gotTalosConfig.Annotations[clusterv1.UpdateInProgressAnnotation]).To(Equal(""))
	g.Expect(gotTalosConfig.Spec).To(BeComparableTo(result.DesiredTalosConfig.Spec))
	g.Expect(managersOf(gotTalosConfig)).To(ContainElement(tcpManagerName))
}

// crdDir writes the CRDs of the objects the in-place update touches and returns their directory.
func crdDir(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-place-updates.yaml"), []byte(inPlaceUpdateCRDs), 0600); err != nil {
		t.Fatalf("failed to write CRDs: %v", err)
	}

	return dir
}

// managersOf returns the field managers that own fields of the object: the InfraMachine and the
// TalosConfig are created with Create, so an in-place update has to take their ownership with
// ForceOwnership before it can change them.
func managersOf(obj client.Object) []string {
	managers := []string{}
	for _, managedField := range obj.GetManagedFields() {
		managers = append(managers, managedField.Manager)
	}

	return managers
}

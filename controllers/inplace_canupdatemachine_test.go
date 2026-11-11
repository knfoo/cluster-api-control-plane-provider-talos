// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilfeature "k8s.io/component-base/featuregate/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hookRuntimeClient is the fork's counterpart of the fake RuntimeClient of Cluster API's Runtime SDK,
// which KubeadmControlPlane uses in canUpdateMachine tests (inplace_canupdatemachine_test.go:154). It
// answers both GetAllExtensions and CallExtension.
type hookRuntimeClient struct {
	handlers []string
	getErr   error
	callErr  error

	response runtimehooksv1.CanUpdateMachineResponse

	getAllExtensionsCalls int
	callExtensionCalls    int
}

func (f *hookRuntimeClient) GetAllExtensions(_ context.Context, hook runtimecatalog.Hook, _ client.Object) ([]string, error) {
	f.getAllExtensionsCalls++

	if f.getErr != nil {
		return nil, f.getErr
	}

	return f.handlers, nil
}

func (f *hookRuntimeClient) CallExtension(_ context.Context, _ runtimecatalog.Hook, _ client.Object, handler string, _ runtimehooksv1.RequestObject, response runtimehooksv1.ResponseObject) error {
	f.callExtensionCalls++

	if f.callErr != nil {
		return errors.Wrapf(f.callErr, "failed to call extension handler %q", handler)
	}

	resp := response.(*runtimehooksv1.CanUpdateMachineResponse)

	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.MachinePatch = f.response.MachinePatch
	resp.BootstrapConfigPatch = f.response.BootstrapConfigPatch
	resp.InfrastructureMachinePatch = f.response.InfrastructureMachinePatch

	return nil
}

func jsonPatch(patch string) runtimehooksv1.Patch {
	return runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     []byte(patch),
	}
}

func mergePatch(patch string) runtimehooksv1.Patch {
	return runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONMergePatchType,
		Patch:     []byte(patch),
	}
}

func testCurrentMachine() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				Kind:     "TalosInfraMachine",
				Name:     "infra-machine-current",
				APIGroup: "infrastructure.cluster.x-k8s.io",
			},
		},
	}
}

func testUpToDateResult() UpToDateResult {
	machine := testCurrentMachine()

	desiredMachine := machine.DeepCopy()
	desiredMachine.Spec.InfrastructureRef.Name = "infra-machine-desired"

	currentInfra := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "TalosInfraMachine",
		"spec":       map[string]any{"image": "image-current"},
	}}
	currentInfra.SetName("infra-machine-current")
	currentInfra.SetNamespace(metav1.NamespaceDefault)

	desiredInfra := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "TalosInfraMachine",
		"spec":       map[string]any{"image": "image-desired"},
	}}
	desiredInfra.SetName("infra-machine-current")
	desiredInfra.SetNamespace(metav1.NamespaceDefault)

	currentTalosConfig := &cabptv1.TalosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: cabptv1.TalosConfigSpec{
			TalosVersion: "v1.14.0",
			Data:         "current-machine-config",
		},
	}

	desiredTalosConfig := currentTalosConfig.DeepCopy()
	desiredTalosConfig.Spec.Data = "desired-machine-config"

	return UpToDateResult{
		DesiredMachine:      desiredMachine,
		CurrentInfraMachine: currentInfra,
		DesiredInfraMachine: desiredInfra,
		CurrentTalosConfig:  currentTalosConfig,
		DesiredTalosConfig:  desiredTalosConfig,
	}
}

// TestCanUpdateMachineIgnoresFieldsTheMachineControllerWrites mirrors the reference behaviour: the
// Machine spec is compared after CleanupMachineSpecForDiff (inplace_canupdatemachine.go:325-335), so a
// field the Machine controller writes on the current Machine, such as bootstrap.dataSecretName, does not
// force a rollout when the extension only patches the TalosConfig.
func TestCanUpdateMachineIgnoresFieldsTheMachineControllerWrites(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	machine := testCurrentMachine()
	// The Machine controller writes bootstrap.dataSecretName on the Machine as the API server holds it;
	// computeDesiredMachine (controllers/machine_apply.go:58-70) does not set it.
	dataSecretName := "machine-to-in-place-update-bootstrap-secret"
	machine.Spec.Bootstrap.DataSecretName = &dataSecretName

	result := testUpToDateResult()
	// The InfraMachine of the Machine as the API server holds it carries the provider-written fields;
	// the extension patches them so the InfraMachine matches after the in-place update.
	result.CurrentInfraMachine.Object["spec"].(map[string]any)["instanceID"] = "i-1234567890abcdef"

	r := &TalosControlPlaneReconciler{
		RuntimeClient: &hookRuntimeClient{
			handlers: []string{"talos-update.canupdatemachine"},
			response: runtimehooksv1.CanUpdateMachineResponse{
				BootstrapConfigPatch: jsonPatch(`[{"op":"replace","path":"/spec/data","value":"desired-machine-config"}]`),
				InfrastructureMachinePatch: jsonPatch(`[{"op":"replace","path":"/spec/image","value":"image-desired"},{"op":"remove","path":"/spec/instanceID"}]`),
			},
		},
	}

	canUpdateMachine, err := r.canUpdateMachine(context.Background(), machine, result)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(canUpdateMachine).To(BeTrue())
	g.Expect(r.RuntimeClient.(*hookRuntimeClient).callExtensionCalls).To(Equal(1))
}

// TestCanUpdateMachineHookCall mirrors the canUpdateMachine cases of KubeadmControlPlane's
// inplace_canupdatemachine_test.go: no patch is a rollout, patches that make the current objects equal
// the desired ones are an in-place update, patches that leave a difference are a rollout, and a failed
// hook call is an error.
func TestCanUpdateMachineHookCall(t *testing.T) {
	tests := []struct {
		name                 string
		response             runtimehooksv1.CanUpdateMachineResponse
		callErr              error
		wantCanUpdateMachine bool
		wantErrorMessage     string
		wantCalls            int
		alreadyEqual         bool
	}{
		{
			name:                 "Return false when the extension returns no patch",
			wantCanUpdateMachine: false,
			wantCalls:            1,
		},
		{
			name: "Return true when the patches make the current objects equal the desired ones",
			response: runtimehooksv1.CanUpdateMachineResponse{
				BootstrapConfigPatch:       jsonPatch(`[{"op":"replace","path":"/spec/data","value":"desired-machine-config"}]`),
				InfrastructureMachinePatch: mergePatch(`{"spec":{"image":"image-desired"}}`),
			},
			wantCanUpdateMachine: true,
			wantCalls:            1,
		},
		{
			name: "Return false when a patch leaves a difference",
			response: runtimehooksv1.CanUpdateMachineResponse{
				BootstrapConfigPatch: jsonPatch(`[{"op":"replace","path":"/spec/data","value":"another-machine-config"}]`),
			},
			wantCanUpdateMachine: false,
			wantCalls:            1,
		},
		{
			name: "Return true when the objects are already equal and the extension returns no patch",
			response: runtimehooksv1.CanUpdateMachineResponse{},
			wantCanUpdateMachine: true,
			wantCalls:            1,
			alreadyEqual:       true,
		},
		{
			name: "Return false when the extension returns no patch and the objects differ",
			response: runtimehooksv1.CanUpdateMachineResponse{},
			wantCanUpdateMachine: false,
			wantCalls:            1,
		},
		{
			name:                 "Return an error when the extension call fails",
			callErr:              errors.New("extension unreachable"),
			wantCanUpdateMachine: false,
			wantErrorMessage:     `failed to call extension handler "talos-update.canupdatemachine": extension unreachable`,
			wantCalls:            1,
		},
		{
			name: "Return an error when a patch writes outside the spec",
			response: runtimehooksv1.CanUpdateMachineResponse{
				MachinePatch: jsonPatch(`[{"op":"replace","path":"/metadata/name","value":"renamed"}]`),
			},
			wantCanUpdateMachine: false,
			wantErrorMessage:     `failed to apply patches from extension talos-update.canupdatemachine to the CanUpdateMachine request: patch path "/metadata/name" is not allowed: only the spec of an object can be patched`,
			wantCalls:            1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

			r := &TalosControlPlaneReconciler{
				RuntimeClient: &hookRuntimeClient{
					handlers: []string{"talos-update.canupdatemachine"},
					callErr:  tt.callErr,
					response: tt.response,
				},
			}

			result := testUpToDateResult()
			if tt.alreadyEqual {
				// Mirror the reference's nonEmptyMachineUpToDateResult (inplace_canupdatemachine_test.go:59-67):
				// the current and the desired objects are equal, so an extension that returns no patch answers true.
				result.DesiredInfraMachine.Object["spec"] = result.CurrentInfraMachine.Object["spec"]
				result.DesiredTalosConfig.Spec = *result.CurrentTalosConfig.Spec.DeepCopy()
			}

			canUpdateMachine, err := r.canUpdateMachine(context.Background(), testCurrentMachine(), result)

			if tt.wantErrorMessage != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tt.wantErrorMessage))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}

			g.Expect(canUpdateMachine).To(Equal(tt.wantCanUpdateMachine))
			g.Expect(r.RuntimeClient.(*hookRuntimeClient).callExtensionCalls).To(Equal(tt.wantCalls))
		})
	}
}

// TestCanUpdateMachineNilObjects checks the guard that runs before the extension lookup
// (inplace_canupdatemachine.go:58-64): when the update could not compute all the objects, the Machine
// is a rollout and no extension is looked up.
func TestCanUpdateMachineNilObjects(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	result := testUpToDateResult()
	result.DesiredTalosConfig = nil

	r := &TalosControlPlaneReconciler{
		RuntimeClient: &hookRuntimeClient{
			handlers: []string{"talos-update.canupdatemachine"},
		},
	}

	canUpdateMachine, err := r.canUpdateMachine(context.Background(), testCurrentMachine(), result)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(canUpdateMachine).To(BeFalse())
	g.Expect(r.RuntimeClient.(*hookRuntimeClient).getAllExtensionsCalls).To(Equal(0))
	g.Expect(r.RuntimeClient.(*hookRuntimeClient).callExtensionCalls).To(Equal(0))
}

// TestCanUpdateMachineInfraMachineWithoutDryRun covers the fork's deviation from KubeadmControlPlane:
// the InfraMachine is compared without the SSA dry-run KCP uses for defaulting
// (inplace_canupdatemachine.go:160-185). An InfraMachine whose spec carries fields the infrastructure
// provider wrote is still matched when the extension patches them.
func TestCanUpdateMachineInfraMachineWithoutDryRun(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	result := testUpToDateResult()

	// Fields the infrastructure provider wrote on the InfraMachine as the API server holds it;
	// the desired InfraMachine computed by the reconciler does not carry them.
	currentInfra := result.CurrentInfraMachine.Object["spec"].(map[string]any)
	currentInfra["instanceID"] = "i-1234567890abcdef"
	currentInfra["ready"] = true

	r := &TalosControlPlaneReconciler{
		RuntimeClient: &hookRuntimeClient{
			handlers: []string{"talos-update.canupdatemachine"},
			response: runtimehooksv1.CanUpdateMachineResponse{
				BootstrapConfigPatch: jsonPatch(`[{"op":"replace","path":"/spec/data","value":"desired-machine-config"}]`),
				// The extension patches the InfraMachine so the provider-written fields match the
				// desired object, which the fork compares without the SSA dry-run KCP uses.
				InfrastructureMachinePatch: jsonPatch(`[{"op":"replace","path":"/spec/image","value":"image-desired"},{"op":"remove","path":"/spec/instanceID"},{"op":"remove","path":"/spec/ready"}]`),
			},
		},
	}

	canUpdateMachine, err := r.canUpdateMachine(context.Background(), testCurrentMachine(), result)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(canUpdateMachine).To(BeTrue())
	g.Expect(r.RuntimeClient.(*hookRuntimeClient).callExtensionCalls).To(Equal(1))
}

// TestCanUpdateMachineRequestIsCleanedUp checks that the request sent to the extension only carries the
// GVK, metadata and spec of the objects, the cleanup KubeadmControlPlane applies before calling the hook
// (inplace_canupdatemachine.go:136-221).
func TestCanUpdateMachineRequestIsCleanedUp(t *testing.T) {
	g := NewWithT(t)

	result := testUpToDateResult()
	result.DesiredMachine.Labels = map[string]string{"cluster.x-k8s.io/cluster-name": "test-cluster"}
	result.CurrentTalosConfig.Labels = map[string]string{"stale": "label"}
	result.CurrentTalosConfig.Annotations = map[string]string{"stale": "annotation"}
	result.CurrentTalosConfig.Status = cabptv1.TalosConfigStatus{Ready: true}
	result.DesiredTalosConfig.Labels = map[string]string{"cluster.x-k8s.io/cluster-name": "test-cluster"}
	result.DesiredInfraMachine.SetLabels(result.DesiredTalosConfig.Labels)

	req, err := createCanUpdateMachineRequest(result.DesiredMachine, result)
	g.Expect(err).ToNot(HaveOccurred())

	currentTalosConfig := map[string]any{}
	g.Expect(json.Unmarshal(req.Current.BootstrapConfig.Raw, &currentTalosConfig)).To(Succeed())
	g.Expect(currentTalosConfig["apiVersion"]).To(Equal("bootstrap.cluster.x-k8s.io/v1alpha3"))
	g.Expect(currentTalosConfig["kind"]).To(Equal("TalosConfig"))
	g.Expect(currentTalosConfig["status"]).To(BeNil())
	g.Expect(currentTalosConfig["metadata"].(map[string]any)["labels"]).To(Equal(map[string]any{"cluster.x-k8s.io/cluster-name": "test-cluster"}))
}

// TestCallExtensionURLAndTLS checks the fork's copy of the Runtime SDK call: the URL is built from the
// ExtensionConfig clientConfig and the https call validates the extension certificate against its caBundle.
func TestCallExtensionURLAndTLS(t *testing.T) {
	g := NewWithT(t)

	group, version, err := splitAPIVersion("hooks.runtime.cluster.x-k8s.io/v1alpha1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(group).To(Equal("hooks.runtime.cluster.x-k8s.io"))
	g.Expect(version).To(Equal("v1alpha1"))

	extensionURL, err := urlForExtension(runtimev1beta2.ClientConfig{
		URL: "https://talos-update.example.com/api",
	}, runtimev1beta2.GroupVersionHook{
		APIVersion: runtimehooksv1.GroupVersion.String(),
		Hook:       "CanUpdateMachine",
	}, "can-update-machine")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(extensionURL.String()).To(Equal("https://talos-update.example.com/api/hooks.runtime.cluster.x-k8s.io/v1alpha1/canupdatemachine/can-update-machine"))

	_, err = urlForExtension(runtimev1beta2.ClientConfig{URL: "http://talos-update.example.com"}, runtimev1beta2.GroupVersionHook{
		APIVersion: runtimehooksv1.GroupVersion.String(),
		Hook:       "CanUpdateMachine",
	}, "can-update-machine")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("expected https scheme"))

	_, err = extensionHTTPClient([]byte("not a certificate"), "talos-update.example.com")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("caBundle"))
}

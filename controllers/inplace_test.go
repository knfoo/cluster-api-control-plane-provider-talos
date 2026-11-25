// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	utilfeature "k8s.io/component-base/featuregate/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeRuntimeClient stands in for the fake RuntimeClient of Cluster API's Runtime SDK
// (NewRuntimeClientBuilder().WithGetAllExtensionResponses(...).Build(), used at
// inplace_canupdatemachine_test.go:154 in the KCP reference). Cluster API keeps that fake in an
// internal/ package a provider cannot import, so the fork carries its own.
type fakeRuntimeClient struct {
	getAllExtensions map[string][]string
	err              error

	getAllExtensionsCalls int
}

func (f *fakeRuntimeClient) GetAllExtensions(_ context.Context, hook runtimecatalog.Hook, _ client.Object) ([]string, error) {
	f.getAllExtensionsCalls++

	if f.err != nil {
		return nil, f.err
	}

	return f.getAllExtensions[runtimecatalog.HookName(hook)], nil
}

// CallExtension is the fake counterpart of the Runtime SDK fake that Cluster API keeps in an internal/
// package (inplace_canupdatemachine_test.go:154). The CanUpdateMachine hook call is exercised by the
// fake in inplace_canupdatemachine_test.go.
func (f *fakeRuntimeClient) CallExtension(_ context.Context, _ runtimecatalog.Hook, _ client.Object, _ string, _ runtimehooksv1.RequestObject, response runtimehooksv1.ResponseObject) error {
	response.SetStatus(runtimehooksv1.ResponseStatusSuccess)

	return nil
}

// TestCanUpdateMachine mirrors the KCP canUpdateMachine test cases (inplace_canupdatemachine_test.go:86-145):
// the feature gate, zero extensions and more than one extension.
func TestCanUpdateMachine(t *testing.T) {
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
	}

	tests := []struct {
		name                  string
		enableInPlaceUpdates  bool
		getAllExtensions      map[string][]string
		getAllExtensionsErr   error
		machineUpToDateResult UpToDateResult
		wantCanUpdateMachine  bool
		wantError             bool
		wantErrorMessage      string
		wantGetAllExtensions  int
	}{
		{
			name:                 "Return false if the feature gate is not enabled",
			enableInPlaceUpdates: false,
			wantCanUpdateMachine: false,
			wantGetAllExtensions: 0,
		},
		{
			name:                  "Return false if no CanUpdateMachine extension is registered",
			enableInPlaceUpdates:  true,
			getAllExtensions:      map[string][]string{},
			machineUpToDateResult: fullUpToDateResult(),
			wantCanUpdateMachine:  false,
			wantGetAllExtensions:  1,
		},
		{
			name:                 "Return an error if more than one CanUpdateMachine extension is registered",
			enableInPlaceUpdates: true,
			getAllExtensions: map[string][]string{
				"CanUpdateMachine": {"test-update-extension-1.canupdate", "test-update-extension-2.canupdate"},
			},
			machineUpToDateResult: fullUpToDateResult(),
			wantCanUpdateMachine:  false,
			wantError:             true,
			wantErrorMessage:      "found multiple CanUpdateMachine hooks (test-update-extension-1.canupdate,test-update-extension-2.canupdate): only one hook is supported",
			wantGetAllExtensions:  1,
		},
		{
			name:                  "Return an error if the extension lookup fails",
			enableInPlaceUpdates:  true,
			getAllExtensionsErr:   errors.New("extension lookup failed"),
			machineUpToDateResult: fullUpToDateResult(),
			wantCanUpdateMachine:  false,
			wantError:             true,
			wantErrorMessage:      "failed to get CanUpdateMachine extensions for Machine machine-to-in-place-update: extension lookup failed",
			wantGetAllExtensions:  1,
		},
		{
			name:                 "Return false with exactly one extension but no computed objects: nothing to compare",
			enableInPlaceUpdates: true,
			getAllExtensions: map[string][]string{
				"CanUpdateMachine": {"talos-update.canupdatemachine"},
			},
			wantCanUpdateMachine: false,
			wantGetAllExtensions: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, tt.enableInPlaceUpdates)

			r := &TalosControlPlaneReconciler{
				RuntimeClient: &fakeRuntimeClient{
					getAllExtensions: tt.getAllExtensions,
					err:              tt.getAllExtensionsErr,
				},
			}

			canUpdateMachine, err := r.canUpdateMachine(context.Background(), machine, tt.machineUpToDateResult)
			if tt.wantError {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tt.wantErrorMessage))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}

			g.Expect(canUpdateMachine).To(Equal(tt.wantCanUpdateMachine))
			g.Expect(r.RuntimeClient.(*fakeRuntimeClient).getAllExtensionsCalls).To(Equal(tt.wantGetAllExtensions))
		})
	}
}

// TestRuntimeClientGetAllExtensions checks the extension lookup the fork mirrors from Cluster API,
// reading the ExtensionConfig objects the Runtime SDK registers.
func TestRuntimeClientGetAllExtensions(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.RuntimeSDK, true)

	canUpdateMachineHook := runtimev1beta2.GroupVersionHook{
		APIVersion: runtimehooksv1.GroupVersion.String(),
		Hook:       "CanUpdateMachine",
	}
	updateMachineHook := runtimev1beta2.GroupVersionHook{
		APIVersion: runtimehooksv1.GroupVersion.String(),
		Hook:       "UpdateMachine",
	}

	discovered := metav1.Condition{
		Type:   runtimev1beta2.ExtensionConfigDiscoveredCondition,
		Status: metav1.ConditionTrue,
		Reason: runtimev1beta2.ExtensionConfigDiscoveredReason,
	}

	talosUpdater := &runtimev1beta2.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-updater"},
		Spec: runtimev1beta2.ExtensionConfigSpec{
			ClientConfig: runtimev1beta2.ClientConfig{URL: "https://talos-updater.cluster.x-k8s.io/api"},
		},
		Status: runtimev1beta2.ExtensionConfigStatus{
			Conditions: []metav1.Condition{discovered},
			Handlers: []runtimev1beta2.ExtensionHandler{
				{Name: "can-update-machine.talos-updater", RequestHook: canUpdateMachineHook},
				{Name: "update-machine.talos-updater", RequestHook: updateMachineHook},
			},
		},
	}

	// Its handlers are not known until the extension is discovered.
	undiscovered := &runtimev1beta2.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "undiscovered-extension"},
		Spec: runtimev1beta2.ExtensionConfigSpec{
			ClientConfig: runtimev1beta2.ClientConfig{URL: "https://undiscovered.cluster.x-k8s.io/api"},
		},
		Status: runtimev1beta2.ExtensionConfigStatus{
			Handlers: []runtimev1beta2.ExtensionHandler{
				{Name: "can-update-machine", RequestHook: canUpdateMachineHook},
			},
		},
	}

	// Restricted to namespaces carrying the label; the Machine's namespace does not.
	labeled := &runtimev1beta2.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "labeled-extension"},
		Spec: runtimev1beta2.ExtensionConfigSpec{
			ClientConfig:      runtimev1beta2.ClientConfig{URL: "https://labeled.cluster.x-k8s.io/api"},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"in-place-updates": "enabled"}},
		},
		Status: runtimev1beta2.ExtensionConfigStatus{
			Conditions: []metav1.Condition{discovered},
			Handlers: []runtimev1beta2.ExtensionHandler{
				{Name: "can-update-machine", RequestHook: canUpdateMachineHook},
			},
		},
	}

	machineNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceDefault},
	}

	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(runtimev1beta2.AddToScheme(scheme)).To(Succeed())

	runtimeClient := NewRuntimeClient(fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(talosUpdater, undiscovered, labeled, machineNamespace).
		Build())

	handlers, err := runtimeClient.GetAllExtensions(context.Background(), runtimehooksv1.CanUpdateMachine, &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(handlers).To(ConsistOf("can-update-machine.talos-updater"))
}

// TestRuntimeClientCallExtensionCanonicalHandlerName drives the whole CanUpdateMachine call against a
// stub extension, with an ExtensionConfig shaped the way Runtime SDK discovery writes it: the handler
// names in Status.Handlers already carry the extension name. It reproduces the failure of the in-place
// update e2e, where the handler was looked up with the extension name appended a second time and
// rejected as not being in the `HANDLER_NAME.EXTENSION_NAME` format.
func TestRuntimeClientCallExtensionCanonicalHandlerName(t *testing.T) {
	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.RuntimeSDK, true)

	canUpdateMachineHook := runtimev1beta2.GroupVersionHook{
		APIVersion: runtimehooksv1.GroupVersion.String(),
		Hook:       "CanUpdateMachine",
	}

	hookPath := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimehooksv1.CanUpdateMachineResponse{
			CommonResponse: runtimehooksv1.CommonResponse{
				Status: runtimehooksv1.ResponseStatusSuccess,
			},
		})
	}))
	defer server.Close()

	extension := &runtimev1beta2.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: runtimev1beta2.ExtensionConfigSpec{
			ClientConfig: runtimev1beta2.ClientConfig{
				URL:      server.URL + "/api",
				CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": metav1.NamespaceDefault}},
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
				{Name: "can-update-machine.talos-in-place-update", RequestHook: canUpdateMachineHook, FailurePolicy: runtimev1beta2.FailurePolicyFail},
			},
		},
	}

	machineNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   metav1.NamespaceDefault,
			Labels: map[string]string{"kubernetes.io/metadata.name": metav1.NamespaceDefault},
		},
	}

	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(runtimev1beta2.AddToScheme(scheme)).To(Succeed())

	runtimeClient := NewRuntimeClient(fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(extension, machineNamespace).
		Build())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-to-in-place-update",
			Namespace: metav1.NamespaceDefault,
		},
	}

	ctx := context.Background()

	handlers, err := runtimeClient.GetAllExtensions(ctx, runtimehooksv1.CanUpdateMachine, machine)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(handlers).To(ConsistOf("can-update-machine.talos-in-place-update"))

	response := &runtimehooksv1.CanUpdateMachineResponse{}
	err = runtimeClient.CallExtension(ctx, runtimehooksv1.CanUpdateMachine, machine, handlers[0], &runtimehooksv1.CanUpdateMachineRequest{}, response)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(response.GetStatus()).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(hookPath).To(Equal("/api/hooks.runtime.cluster.x-k8s.io/v1alpha1/canupdatemachine/can-update-machine"))
}

// fullUpToDateResult is an UpToDateResult whose objects are all present, so tryInPlaceUpdate gets to
// ask canUpdateMachine.
func fullUpToDateResult() UpToDateResult {
	return UpToDateResult{
		DesiredMachine:      &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-to-in-place-update"}},
		CurrentInfraMachine: &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}},
		DesiredInfraMachine: &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}},
		CurrentTalosConfig:  &cabptv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-to-in-place-update"}},
		DesiredTalosConfig:  &cabptv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-to-in-place-update"}},
	}
}

// Test_tryInPlaceUpdate mirrors the tryInPlaceUpdate test of KubeadmControlPlane (inplace_test.go:40-66):
// the decision and the trigger are overridden, and the test checks that a Machine that cannot be
// updated in place falls back to the scale down that rolls it out, while one that can is triggered.
func Test_tryInPlaceUpdate(t *testing.T) {
	machineToInPlaceUpdate := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-to-in-place-update",
		},
	}

	tests := []struct {
		name                           string
		canUpdateMachineFunc           func(ctx context.Context, machine *clusterv1.Machine) (bool, error)
		machineUpToDateResult          UpToDateResult
		wantCanUpdateMachineCalled     bool
		wantTriggerInPlaceUpdateCalled bool
		wantFallbackToScaleDown        bool
		wantError                      bool
		wantErrorMessage               string
	}{
		{
			name: "Return error if canUpdateMachine returns an error",
			canUpdateMachineFunc: func(_ context.Context, _ *clusterv1.Machine) (bool, error) {
				return false, errors.New("canUpdateMachine error")
			},
			machineUpToDateResult:      fullUpToDateResult(),
			wantCanUpdateMachineCalled: true,
			wantError:                  true,
			wantErrorMessage:           "failed to determine if Machine machine-to-in-place-update can be updated in-place: canUpdateMachine error",
		},
		{
			name: "Fallback to scale down if canUpdateMachine returns false",
			canUpdateMachineFunc: func(_ context.Context, _ *clusterv1.Machine) (bool, error) {
				return false, nil
			},
			machineUpToDateResult:      fullUpToDateResult(),
			wantCanUpdateMachineCalled: true,
			wantFallbackToScaleDown:    true,
		},
		{
			name: "Trigger in-place update if canUpdateMachine returns true",
			canUpdateMachineFunc: func(_ context.Context, _ *clusterv1.Machine) (bool, error) {
				return true, nil
			},
			machineUpToDateResult:          fullUpToDateResult(),
			wantCanUpdateMachineCalled:     true,
			wantTriggerInPlaceUpdateCalled: true,
			wantFallbackToScaleDown:        false,
		},
		{
			name: "Fallback to scale down if the desired InfraMachine is missing",
			machineUpToDateResult: func() UpToDateResult {
				result := fullUpToDateResult()
				result.DesiredInfraMachine = nil
				return result
			}(),
			wantFallbackToScaleDown: true,
		},
		{
			name: "Fallback to scale down if the desired TalosConfig is missing",
			machineUpToDateResult: func() UpToDateResult {
				result := fullUpToDateResult()
				result.DesiredTalosConfig = nil
				return result
			}(),
			wantFallbackToScaleDown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			var canUpdateMachineCalled bool
			var triggerInPlaceUpdateCalled bool

			r := &TalosControlPlaneReconciler{
				overrideCanUpdateMachine: func(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
					canUpdateMachineCalled = true
					if tt.canUpdateMachineFunc == nil {
						return false, nil
					}

					return tt.canUpdateMachineFunc(ctx, machine)
				},
				overrideTriggerInPlaceUpdate: func(_ context.Context, _ *clusterv1.Machine, _ UpToDateResult) error {
					triggerInPlaceUpdateCalled = true

					return nil
				},
			}

			fallbackToScaleDown, err := r.tryInPlaceUpdate(context.Background(), nil, machineToInPlaceUpdate, tt.machineUpToDateResult)
			if tt.wantError {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tt.wantErrorMessage))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}

			g.Expect(fallbackToScaleDown).To(Equal(tt.wantFallbackToScaleDown))
			g.Expect(canUpdateMachineCalled).To(Equal(tt.wantCanUpdateMachineCalled), "canUpdateMachineCalled: actual: %t expected: %t", canUpdateMachineCalled, tt.wantCanUpdateMachineCalled)
			g.Expect(triggerInPlaceUpdateCalled).To(Equal(tt.wantTriggerInPlaceUpdateCalled), "triggerInPlaceUpdateCalled: actual: %t expected: %t", triggerInPlaceUpdateCalled, tt.wantTriggerInPlaceUpdateCalled)
		})
	}
}

// rollingUpdateControlPlane returns a three-Machine control plane whose TalosConfig is outdated on every
// Machine, the state the e2e run e2e-849a0df-0928bcc reached when the in-place update step failed, along
// with the MachineList reconcileMachines expects.
func rollingUpdateControlPlane(machines []*clusterv1.Machine) (*clusterv1.Cluster, *controlplanev1.TalosControlPlane, *ControlPlane, *clusterv1.MachineList) {
	tcp := &controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: metav1.NamespaceDefault},
		Spec: controlplanev1.TalosControlPlaneSpec{
			Version:  currentVersion,
			Replicas: ptr.To[int32](int32(len(machines))),
			RolloutStrategy: &controlplanev1.RolloutStrategy{
				Type:          controlplanev1.RollingUpdateStrategyType,
				RollingUpdate: &controlplanev1.RollingUpdate{MaxSurge: ptr.To(intstr.FromInt(0))},
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: metav1.NamespaceDefault},
		Status:     clusterv1.ClusterStatus{FailureDomains: controlPlaneDomains("a", "b", "c")},
	}

	machineList := clusterv1.MachineList{Items: make([]clusterv1.Machine, 0, len(machines))}
	for _, machine := range machines {
		machineList.Items = append(machineList.Items, *machine)
	}

	return cluster, tcp, &ControlPlane{
		TCP:      tcp,
		Cluster:  cluster,
		Machines: collections.FromMachines(machines...),
	}, &machineList
}

// TestReconcileMachinesWaitsForInProgressInPlaceUpdate covers the failure of e2e run
// e2e-849a0df-0928bcc: the in-place update step failed because the reconciler started an in-place update
// for every Machine that is not up to date, one per reconcile, so all control plane Machines were marked
// in-progress at the same time. KubeadmControlPlane waits for the update in progress to finish before
// rolling out the next Machine (kubeadmcontrolplane_controller.go:517-523 of the Cluster API v1.14.4
// reference); without that wait a second Machine is triggered while the first is still being updated.
func TestReconcileMachinesWaitsForInProgressInPlaceUpdate(t *testing.T) {

	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	machines := []*clusterv1.Machine{
		newControlPlaneMachine("cp-1", inFailureDomain("a"), outdated(), createdAt(0)),
		newControlPlaneMachine("cp-2", inFailureDomain("b"), outdated(), createdAt(time.Minute)),
		newControlPlaneMachine("cp-3", inFailureDomain("c"), outdated(), createdAt(2*time.Minute)),
	}

	// cp-1 is already being updated in place: the trigger wrote the UpdateInProgressAnnotation and the
	// UpdateMachine hook is pending on it.
	machines[0].Annotations = map[string]string{clusterv1.UpdateInProgressAnnotation: ""}
	markObjectAsPending(machines[0], runtimehooksv1.UpdateMachine)

	cluster, tcp, controlPlane, machineList := rollingUpdateControlPlane(machines)

	var triggered []string

	r := &TalosControlPlaneReconciler{
		Client: fake.NewClientBuilder().Build(),
		Log:    logr.Discard(),
		overrideTryInPlaceUpdate: func(_ context.Context, _ *ControlPlane, machine *clusterv1.Machine, _ UpToDateResult) (bool, error) {
			triggered = append(triggered, machine.Name)

			return false, nil
		},
	}

	// The Machine controller has not finished cp-1 yet, so three consecutive reconciles may not start
	// the in-place update of another Machine.
	for range 3 {
		res, err := r.reconcileMachines(context.Background(), cluster, tcp, machineList, controlPlane)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
	}

	g.Expect(triggered).To(BeEmpty(), "no Machine may be updated in place while another update is in progress")
	g.Expect(controlPlane.MachinesToCompleteInPlaceUpdate().Names()).To(Equal([]string{"cp-1"}))
}

// TestReconcileMachinesCompletesInterruptedInPlaceUpdateTrigger covers the reentrancy step of
// KubeadmControlPlane (kubeadmcontrolplane_controller.go:501-511 of the Cluster API v1.14.4 reference):
// if triggerInPlaceUpdate failed after writing the UpdateInProgressAnnotation and before marking the
// UpdateMachine hook pending, the next reconcile completes the trigger instead of waiting forever for an
// update that never started.
func TestReconcileMachinesCompletesInterruptedInPlaceUpdateTrigger(t *testing.T) {

	g := NewWithT(t)

	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.InPlaceUpdates, true)

	machines := []*clusterv1.Machine{
		newControlPlaneMachine("cp-1", inFailureDomain("a"), outdated(), createdAt(0)),
		newControlPlaneMachine("cp-2", inFailureDomain("b"), outdated(), createdAt(time.Minute)),
		newControlPlaneMachine("cp-3", inFailureDomain("c"), outdated(), createdAt(2*time.Minute)),
	}

	// The interrupted trigger: annotated, but the hook was never marked pending.
	machines[0].Annotations = map[string]string{clusterv1.UpdateInProgressAnnotation: ""}

	cluster, tcp, controlPlane, machineList := rollingUpdateControlPlane(machines)

	var completed []string

	r := &TalosControlPlaneReconciler{
		Client: fake.NewClientBuilder().Build(),
		Log:    logr.Discard(),
		overrideTriggerInPlaceUpdate: func(_ context.Context, machine *clusterv1.Machine, _ UpToDateResult) error {
			completed = append(completed, machine.Name)
			markObjectAsPending(machine, runtimehooksv1.UpdateMachine)

			return nil
		},
	}

	res, err := r.reconcileMachines(context.Background(), cluster, tcp, machineList, controlPlane)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	g.Expect(completed).To(Equal([]string{"cp-1"}), "the interrupted trigger is completed for the annotated Machine only")
	g.Expect(controlPlane.MachinesToCompleteTriggerInPlaceUpdate().Len()).To(BeZero())
}

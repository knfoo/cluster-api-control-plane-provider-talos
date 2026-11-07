// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	utilfeature "k8s.io/component-base/featuregate/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
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
		name                   string
		enableInPlaceUpdates   bool
		getAllExtensions       map[string][]string
		getAllExtensionsErr    error
		wantCanUpdateMachine   bool
		wantError              bool
		wantErrorMessage       string
		wantGetAllExtensions   int
	}{
		{
			name:                 "Return false if the feature gate is not enabled",
			enableInPlaceUpdates: false,
			wantCanUpdateMachine: false,
			wantGetAllExtensions: 0,
		},
		{
			name:                 "Return false if no CanUpdateMachine extension is registered",
			enableInPlaceUpdates: true,
			getAllExtensions:     map[string][]string{},
			wantCanUpdateMachine: false,
			wantGetAllExtensions: 1,
		},
		{
			name:                 "Return an error if more than one CanUpdateMachine extension is registered",
			enableInPlaceUpdates: true,
			getAllExtensions: map[string][]string{
				"CanUpdateMachine": {"test-update-extension-1.canupdate", "test-update-extension-2.canupdate"},
			},
			wantCanUpdateMachine: false,
			wantError:            true,
			wantErrorMessage:     "found multiple CanUpdateMachine hooks (test-update-extension-1.canupdate,test-update-extension-2.canupdate): only one hook is supported",
			wantGetAllExtensions: 1,
		},
		{
			name:                 "Return an error if the extension lookup fails",
			enableInPlaceUpdates: true,
			getAllExtensionsErr:  errors.New("extension lookup failed"),
			wantCanUpdateMachine: false,
			wantError:            true,
			wantErrorMessage:     "failed to get CanUpdateMachine extensions for Machine machine-to-in-place-update: extension lookup failed",
			wantGetAllExtensions: 1,
		},
		{
			name:                 "Return false with exactly one extension: calling the hook is the next stage",
			enableInPlaceUpdates: true,
			getAllExtensions: map[string][]string{
				"CanUpdateMachine": {"talos-update.canupdatemachine"},
			},
			wantCanUpdateMachine: false,
			wantGetAllExtensions: 1,
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

			canUpdateMachine, err := r.canUpdateMachine(context.Background(), machine)
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
				{Name: "can-update-machine", RequestHook: canUpdateMachineHook},
				{Name: "update-machine", RequestHook: updateMachineHook},
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

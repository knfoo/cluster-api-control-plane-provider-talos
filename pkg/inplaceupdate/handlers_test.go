// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// fakeApplier stands in for a Talos node. It reports a reboot requirement when the
// configuration touches the machine install image, which Talos cannot apply with mode
// NO_REBOOT.
type fakeApplier struct {
	applied      map[string]string
	alwaysReboot bool
	dryRunRuns   int
	applyRuns    int
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{applied: map[string]string{}}
}

func (f *fakeApplier) status(endpoint string, config []byte) ApplyStatus {
	if f.applied[endpoint] == string(config) {
		return ApplyStatus{NoChanges: true, Details: "the node already runs this configuration"}
	}

	if f.alwaysReboot || strings.Contains(string(config), "install:") {
		return ApplyStatus{RebootRequired: true, Details: "the machine configuration requires a reboot"}
	}

	return ApplyStatus{Details: "the node accepts the configuration with mode NO_REBOOT"}
}

func (f *fakeApplier) DryRun(_ context.Context, endpoint string, config []byte) (ApplyStatus, error) {
	f.dryRunRuns++

	return f.status(endpoint, config), nil
}

func (f *fakeApplier) Apply(_ context.Context, endpoint string, config []byte) error {
	f.applyRuns++
	f.applied[endpoint] = string(config)

	return nil
}

const baseConfig = `version: v1alpha1
machine:
  network:
    hostname: control-plane-1
  files:
    - path: /var/inplace/first
      content: original
cluster:
  token: bootstrap-token
`

const changedConfig = `version: v1alpha1
machine:
  network:
    hostname: control-plane-1
  files:
    - path: /var/inplace/first
      content: updated
    - path: /var/inplace/second
      content: added
cluster:
  token: bootstrap-token
`

// rebootConfig changes the machine install image, which Talos cannot apply without a
// reboot.
const rebootConfig = `version: v1alpha1
machine:
  install:
    image: ghcr.io/siderolabs/talos:v1.14.6
  network:
    hostname: control-plane-1
  files:
    - path: /var/inplace/first
      content: updated
cluster:
  token: bootstrap-token
`

func talosConfigRaw(name string, spec map[string]any) runtime.RawExtension {
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "bootstrap.cluster.x-k8s.io/v1alpha3",
		"kind":       "TalosConfig",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	})
	if err != nil {
		panic(err)
	}

	return runtime.RawExtension{Raw: raw}
}

func talosConfig(name, data string) runtime.RawExtension {
	return talosConfigRaw(name, map[string]any{
		"talosVersion": "v1.14.0",
		"data":         data,
	})
}

func machine(name, version string) clusterv1.Machine {
	return clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: clusterv1.MachineSpec{
			Version:    version,
			ProviderID: "talos://10.0.0.1",
		},
	}
}

func infraMachine(image string) runtime.RawExtension {
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "TalosInfraMachine",
		"metadata":   map[string]any{"name": "infra-machine"},
		"spec":       map[string]any{"image": image},
	})
	if err != nil {
		panic(err)
	}

	return runtime.RawExtension{Raw: raw}
}

func canUpdateRequest(current, desired runtime.RawExtension, infraImage string, currentVersion, desiredVersion string) *runtimehooksv1.CanUpdateMachineRequest {
	return &runtimehooksv1.CanUpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Current: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", currentVersion),
			InfrastructureMachine: infraMachine(infraImage),
			BootstrapConfig:       current,
		},
		Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", desiredVersion),
			InfrastructureMachine: infraMachine(infraImage),
			BootstrapConfig:       desired,
		},
	}
}

func expectNoInPlaceUpdate(g *GomegaWithT, resp *runtimehooksv1.CanUpdateMachineResponse, applier *fakeApplier) {
	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).To(BeEmpty())
	g.Expect(resp.MachinePatch.Patch).To(BeEmpty())
	g.Expect(resp.InfrastructureMachinePatch.Patch).To(BeEmpty())
	g.Expect(applier.dryRunRuns).To(BeZero())
}

// TestCanUpdateMachinePatch checks the patch computed from a machine configuration
// diff.
func TestCanUpdateMachinePatch(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.33.1"),
		resp,
	)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())

	ops := []jsonPatchOp{}
	g.Expect(json.Unmarshal(resp.BootstrapConfigPatch.Patch, &ops)).To(Succeed())
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "replace", Path: "/spec/data", Value: changedConfig}))
	g.Expect(applier.dryRunRuns).To(Equal(1))
	g.Expect(applier.applyRuns).To(BeZero())
}

// TestCanUpdateMachineConfigPatches checks that the RFC 6902 patches of a TalosConfig
// are applied before the machine configuration is diffed.
func TestCanUpdateMachineConfigPatches(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":  "v1.14.0",
		"data":          baseConfig,
		"configPatches": []map[string]any{{"op": "replace", "path": "/machine/files/0/content", "value": "patched"}},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(talosConfig("talosconfig-1", baseConfig), desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
}

// TestCanUpdateMachineNoPatchForRebootRequired checks that a change Talos cannot apply
// without a reboot gets no patch, so Cluster API rolls the machine out.
func TestCanUpdateMachineNoPatchForRebootRequired(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", rebootConfig), "talos-image", "v1.33.1", "v1.33.1"),
		resp,
	)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestCanUpdateMachineNoPatchWhenDryRunNeedsReboot checks the second half of the
// owner's condition: the node decides, through the DryRun.
func TestCanUpdateMachineNoPatchWhenDryRunNeedsReboot(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	applier.alwaysReboot = true
	handler := NewHandler(applier, logr.Discard(), "", 0)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.33.1"),
		resp,
	)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).To(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
}

// TestCanUpdateMachineNoPatchForRolloutChanges checks the changes the owner keeps as
// rollouts: Talos version, Kubernetes version and infrastructure changes.
func TestCanUpdateMachineNoPatchForRolloutChanges(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name    string
		request *runtimehooksv1.CanUpdateMachineRequest
	}{
		{
			name: "Talos version change",
			request: canUpdateRequest(
				talosConfig("talosconfig-1", baseConfig),
				talosConfigRaw("talosconfig-1", map[string]any{"talosVersion": "v1.15.0", "data": changedConfig}),
				"talos-image",
				"v1.33.1",
				"v1.33.1",
			),
		},
		{
			name: "Kubernetes version change",
			request: canUpdateRequest(
				talosConfig("talosconfig-1", baseConfig),
				talosConfig("talosconfig-1", changedConfig),
				"talos-image",
				"v1.33.1",
				"v1.34.0",
			),
		},
		{
			name: "infrastructure change",
			request: &runtimehooksv1.CanUpdateMachineRequest{
				CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
				Current: runtimehooksv1.CanUpdateMachineRequestObjects{
					Machine:               machine("control-plane-1", "v1.33.1"),
					InfrastructureMachine: infraMachine("talos-image"),
					BootstrapConfig:       talosConfig("talosconfig-1", baseConfig),
				},
				Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
					Machine:               machine("control-plane-1", "v1.33.1"),
					InfrastructureMachine: infraMachine("different-image"),
					BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
				},
			},
		},
		{
			name: "identical machine configuration",
			request: canUpdateRequest(
				talosConfig("talosconfig-1", baseConfig),
				talosConfig("talosconfig-1", baseConfig),
				"talos-image",
				"v1.33.1",
				"v1.33.1",
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := newFakeApplier()
			handler := NewHandler(applier, logr.Discard(), "", 0)

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(test.request, resp)

			expectNoInPlaceUpdate(g, resp, applier)
		})
	}
}

// TestUpdateMachineIdempotent checks that a second call with the same desired
// configuration reports the update as finished.
func TestUpdateMachineIdempotent(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, first)

	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	g.Expect(applier.applyRuns).To(Equal(1))

	second := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, second)

	g.Expect(second.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(second.RetryAfterSeconds).To(BeZero())
	g.Expect(applier.applyRuns).To(Equal(1))
}

// TestUpdateMachineFailsWhenRebootRequired checks the extension never applies a
// configuration the node would need a reboot for.
func TestUpdateMachineFailsWhenRebootRequired(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", rebootConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(applier.applyRuns).To(BeZero())
}

// TestApplyConfigPatches checks the RFC 6902 patches of a TalosConfig.
func TestApplyConfigPatches(t *testing.T) {
	g := NewWithT(t)

	patched, err := applyConfigPatches([]byte(baseConfig), []jsonPatchOp{
		{Op: "replace", Path: "/machine/files/0/content", Value: "patched"},
		{Op: "add", Path: "/machine/files/1", Value: map[string]any{"path": "/var/inplace/second", "content": "added"}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(patched)).To(ContainSubstring("patched"))
	g.Expect(string(patched)).To(ContainSubstring("/var/inplace/second"))
}

// TestExtensionRBAC checks that the rules the extension needs are in the generated
// role.
func TestExtensionRBAC(t *testing.T) {
	g := NewWithT(t)

	role, err := os.ReadFile("../../config/rbac/role.yaml")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(string(role)).To(ContainSubstring("runtime.cluster.x-k8s.io"))
	g.Expect(string(role)).To(ContainSubstring("extensionconfigs"))
	g.Expect(string(role)).To(ContainSubstring("- namespaces"))
}

// TestDiscovery checks that the extension announces only the two hooks of this stage: the
// MachineDeployment hook CanUpdateMachineSet belongs to stage L5.
func TestDiscovery(t *testing.T) {
	g := NewWithT(t)
	handler := NewHandler(newFakeApplier(), logr.Discard(), "", 0)

	resp := &runtimehooksv1.DiscoveryResponse{}
	handler.Discovery(&runtimehooksv1.DiscoveryRequest{}, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))

	hooks := []string{}
	for _, extensionHandler := range resp.Handlers {
		g.Expect(extensionHandler.RequestHook.APIVersion).To(Equal(runtimehooksv1.GroupVersion.String()))
		hooks = append(hooks, extensionHandler.RequestHook.Hook)
	}

	g.Expect(hooks).To(ConsistOf("CanUpdateMachine", "UpdateMachine"))
}

// TestCanUpdateMachineNoPatchForStrategicPatches checks that a TalosConfig using strategic patches is
// not updated in place: the extension cannot compute the machine configuration those patches produce,
// so the Machine rolls out.
func TestCanUpdateMachineNoPatchForStrategicPatches(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"data":             baseConfig,
		"strategicPatches": []string{"{\"machine\":{\"network\":{\"hostname\":\"new-host\"}}}"},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(talosConfig("talosconfig-1", baseConfig), desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestUpdateMachineNeedsAnEndpoint checks that the extension fails loudly instead of guessing which
// node to update.
func TestUpdateMachineNeedsAnEndpoint(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "control-plane-1"}},
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(resp.Message).To(ContainSubstring("cannot determine the Talos address"))
	g.Expect(applier.applyRuns).To(BeZero())
}

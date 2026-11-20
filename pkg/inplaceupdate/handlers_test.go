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
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// fakeApplier stands in for a Talos node. It reports a reboot requirement when the
// configuration touches the machine install image, and it reads the Kubernetes component
// images out of the configuration it was given, the way a node reports them through its
// COSI resources.
type fakeApplier struct {
	applied      map[string]string
	endpoints    []string
	alwaysReboot bool
	dryRunRuns   int
	applyRuns    int
	node         NodeStatus
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{applied: map[string]string{}}
}

// controlPlaneNode runs the control-plane components of Kubernetes 1.33.1, with the
// kubelet image suffix the bootstrap provider gives it.
func controlPlaneNode() NodeStatus {
	return NodeStatus{
		APIServer:         "registry.k8s.io/kube-apiserver:v1.33.1",
		ControllerManager: "registry.k8s.io/kube-controller-manager:v1.33.1",
		Scheduler:         "registry.k8s.io/kube-scheduler:v1.33.1",
		Proxy:             "registry.k8s.io/kube-proxy:v1.33.1",
		Kubelet:           "ghcr.io/siderolabs/kubelet:v1.33.1-fat",
	}
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

func (f *fakeApplier) NodeStatus(_ context.Context, endpoint string) (NodeStatus, error) {
	f.endpoints = append(f.endpoints, endpoint)

	return f.node, nil
}

func (f *fakeApplier) DryRun(_ context.Context, endpoint string, config []byte) (ApplyStatus, error) {
	f.dryRunRuns++
	f.endpoints = append(f.endpoints, endpoint)

	return f.status(endpoint, config), nil
}

func (f *fakeApplier) Apply(_ context.Context, endpoint string, config []byte) error {
	f.applyRuns++
	f.endpoints = append(f.endpoints, endpoint)
	f.applied[endpoint] = string(config)
	f.node = nodeStatusFromConfig(config)

	return nil
}

// nodeStatusFromConfig reads the Kubernetes component images of a machine configuration,
// both of the v1alpha1 form (cluster/apiServer/image, machine/kubelet/image) and of the
// documents a generated configuration carries (KubeAPIServerConfig and friends).
func nodeStatusFromConfig(data []byte) NodeStatus {
	status := NodeStatus{}

	docs, err := documents(data)
	if err != nil {
		return status
	}

	for _, doc := range docs {
		for _, document := range []struct {
			kind  string
			image *string
		}{
			{kind: "KubeAPIServerConfig", image: &status.APIServer},
			{kind: "KubeControllerManagerConfig", image: &status.ControllerManager},
			{kind: "KubeSchedulerConfig", image: &status.Scheduler},
			{kind: "KubeProxyConfig", image: &status.Proxy},
			{kind: "KubeletConfig", image: &status.Kubelet},
		} {
			if doc["kind"] == document.kind {
				if image, ok := doc["image"].(string); ok && image != "" {
					*document.image = image
				}
			}
		}

		if cluster, ok := doc["cluster"].(map[string]any); ok {
			setComponentImage(&status.APIServer, componentImageOf(cluster, "apiServer"))
			setComponentImage(&status.ControllerManager, componentImageOf(cluster, "controllerManager"))
			setComponentImage(&status.Scheduler, componentImageOf(cluster, "scheduler"))
			setComponentImage(&status.Proxy, componentImageOf(cluster, "proxy"))
		}

		if machine, ok := doc["machine"].(map[string]any); ok {
			setComponentImage(&status.Kubelet, componentImageOf(machine, "kubelet"))
		}
	}

	return status
}

func componentImageOf(document map[string]any, key string) string {
	value, ok := document[key].(map[string]any)
	if !ok {
		return ""
	}

	image, ok := value["image"].(string)
	if !ok {
		return ""
	}

	return image
}

func setComponentImage(target *string, image string) {
	if image != "" {
		*target = image
	}
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

// generateTypeConfig is a TalosConfig created with generateType, which carries no
// machine configuration data.
func generateTypeConfig(name string) runtime.RawExtension {
	return talosConfigRaw(name, map[string]any{
		"talosVersion": "v1.14.0",
		"generateType": "controlplane",
	})
}

func machine(name, version string) clusterv1.Machine {
	return clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: clusterv1.MachineSpec{
			ClusterName:       "test-cluster",
			Version:           version,
			ProviderID:        "talos://10.0.0.1",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "TalosInfraMachine", Name: "infra-machine"},
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
// rollouts: Talos version and infrastructure changes. A Kubernetes version change is not in
// this list since the owner's amendment of 2026-11-15: an adjacent Kubernetes version
// change of a control-plane Machine is updated in place (see
// TestCanUpdateMachineInPlaceKubernetesVersion), a non-adjacent one still rolls out (see
// TestCanUpdateMachineRolloutForNonAdjacentKubernetesVersion).
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
		{
			name: "generateType TalosConfig on both sides",
			request: canUpdateRequest(
				generateTypeConfig("talosconfig-1"),
				generateTypeConfig("talosconfig-1"),
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

// TestCanUpdateMachineInPlaceKubernetesVersion checks the owner's amendment of 2026-11-15:
// an adjacent Kubernetes version change of a control-plane Machine is handled in place. The
// response carries the patch for Machine spec.version, together with the TalosConfig patch
// the machine configuration diff produces, and the node is DryRun once.
func TestCanUpdateMachineInPlaceKubernetesVersion(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name             string
		current          runtime.RawExtension
		desired          runtime.RawExtension
		wantsConfigPatch bool
	}{
		{
			name:             "with TalosConfig changes",
			current:          talosConfig("talosconfig-1", baseConfig),
			desired:          talosConfig("talosconfig-1", changedConfig),
			wantsConfigPatch: true,
		},
		{
			name:             "the Kubernetes version alone",
			current:          talosConfig("talosconfig-1", baseConfig),
			desired:          talosConfig("talosconfig-1", baseConfig),
			wantsConfigPatch: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := newFakeApplier()
			applier.node = controlPlaneNode()
			handler := NewHandler(applier, logr.Discard(), "", 0)

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(canUpdateRequest(test.current, test.desired, "talos-image", "v1.33.1", "v1.34.0"), resp)

			g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
			g.Expect(resp.MachinePatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))

			ops := []jsonPatchOp{}
			g.Expect(json.Unmarshal(resp.MachinePatch.Patch, &ops)).To(Succeed())
			g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "replace", Path: "/spec/version", Value: "v1.34.0"}))

			if test.wantsConfigPatch {
				g.Expect(resp.BootstrapConfigPatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))
				g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
			} else {
				g.Expect(resp.BootstrapConfigPatch.Patch).To(BeEmpty())
			}

			g.Expect(resp.InfrastructureMachinePatch.Patch).To(BeEmpty())
			g.Expect(applier.dryRunRuns).To(Equal(1))
			g.Expect(applier.applyRuns).To(BeZero())
		})
	}
}

// TestCanUpdateMachineRolloutForNonAdjacentKubernetesVersion checks the fix of the attempt-3
// review: Talos supports the Kubernetes versions around the one it ships, so a jump of more
// than one minor version, a patch version and a downgrade are not upgrades in place.
func TestCanUpdateMachineRolloutForNonAdjacentKubernetesVersion(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name    string
		current string
		desired string
	}{
		{name: "two minor versions", current: "v1.33.1", desired: "v1.35.0"},
		{name: "a patch version", current: "v1.33.1", desired: "v1.33.2"},
		{name: "a downgrade", current: "v1.34.0", desired: "v1.33.1"},
		{name: "an unparsable version", current: "v1.33.1", desired: "not-a-version"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := newFakeApplier()
			applier.node = controlPlaneNode()
			handler := NewHandler(applier, logr.Discard(), "", 0)

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(
				canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", test.current, test.desired),
				resp,
			)

			expectNoInPlaceUpdate(g, resp, applier)
		})
	}
}

// TestUpdateMachineKubernetesVersionStages checks that UpdateMachine moves the control-plane
// components in the order talosctl upgrade-k8s uses - kube-apiserver, kube-controller-manager,
// kube-scheduler, kube-proxy, kubelet - and reports progress until the node runs every
// component at the target version.
func TestUpdateMachineKubernetesVersionStages(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = controlPlaneNode()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for index, component := range kubernetesComponents {
		resp := &runtimehooksv1.UpdateMachineResponse{}
		handler.UpdateMachine(request, resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
		g.Expect(resp.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))

		g.Expect(componentAtTarget(component.image(applier.node), "v1.34.0")).To(BeTrue(), "machine configuration for %q did not advance %s", component.name, component.name)

		if index+1 < len(kubernetesComponents) {
			next := kubernetesComponents[index+1]

			g.Expect(componentAtTarget(next.image(applier.node), "v1.34.0")).To(BeFalse(), "%s was upgraded before %s", next.name, component.name)
		}
	}

	g.Expect(applier.applyRuns).To(Equal(len(kubernetesComponents)))

	done := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(done.RetryAfterSeconds).To(BeZero())
	g.Expect(applier.applyRuns).To(Equal(len(kubernetesComponents)))
	g.Expect(componentsAtTarget(applier.node, "v1.34.0")).To(BeTrue())

	applied := applier.applied["10.0.0.1"]
	g.Expect(applied).To(ContainSubstring("kube-apiserver:v1.34.0"))
	g.Expect(applied).To(ContainSubstring("kube-controller-manager:v1.34.0"))
	g.Expect(applied).To(ContainSubstring("kube-scheduler:v1.34.0"))
	g.Expect(applied).To(ContainSubstring("kube-proxy:v1.34.0"))
	g.Expect(applied).To(ContainSubstring("kubelet:v1.34.0"))
}

// TestCanUpdateMachineIgnoresBootstrapMetadataWrittenByTheMachineController covers the
// e2e case: the current Machine carries spec.bootstrap.dataSecretName written by the
// Machine controller, the desired Machine carries only the ConfigRef, and the TalosConfig
// changed. That is the normal in-place case, so the handler must not answer "the bootstrap
// configuration reference changes" but return the machine configuration patch.
func TestCanUpdateMachineIgnoresBootstrapMetadataWrittenByTheMachineController(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	dataSecretName := "talosconfig-1-bootstrap-data"
	configRef := clusterv1.ContractVersionedObjectReference{
		APIGroup: "bootstrap.cluster.x-k8s.io",
		Kind:     "TalosConfig",
		Name:     "talosconfig-1",
	}

	current := machine("control-plane-1", "v1.33.1")
	current.Spec.Bootstrap = clusterv1.Bootstrap{ConfigRef: configRef, DataSecretName: &dataSecretName}

	desired := machine("control-plane-1", "v1.33.1")
	desired.Spec.Bootstrap = clusterv1.Bootstrap{ConfigRef: configRef}

	request := &runtimehooksv1.CanUpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Current: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               current,
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", baseConfig),
		},
		Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               desired,
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
	g.Expect(resp.MachinePatch.Patch).To(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
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

// TestDiscovery checks that the extension announces the three in-place update hooks of the
// extension: the Machine hooks CanUpdateMachine and UpdateMachine and the MachineDeployment
// hook CanUpdateMachineSet of stage L5.
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

	g.Expect(hooks).To(ConsistOf("CanUpdateMachine", "CanUpdateMachineSet", "UpdateMachine"))
}

// TestDiscoveryHandlerNamesAreUnique reproduces the e2e failure of run 4bacb43-03356e7:
// the Runtime SDK client rejects a discovery response that repeats a handler name and the
// ExtensionConfig never gets the Discovered condition, so the extension is never called
// (sigs.k8s.io/cluster-api/internal/runtime/client/client.go:774-786).
func TestDiscoveryHandlerNamesAreUnique(t *testing.T) {
	g := NewWithT(t)
	handler := NewHandler(newFakeApplier(), logr.Discard(), "", 0)

	resp := &runtimehooksv1.DiscoveryResponse{}
	handler.Discovery(&runtimehooksv1.DiscoveryRequest{}, resp)

	seen := map[string]int{}

	for _, extensionHandler := range resp.Handlers {
		g.Expect(validation.IsDNS1123Label(extensionHandler.Name)).To(BeEmpty(), "handler name %q must be a DNS1123 label", extensionHandler.Name)
		seen[extensionHandler.Name]++
	}

	for name, count := range seen {
		g.Expect(count).To(Equal(1), "handler name %q is announced more than once", name)
	}
}

// TestCanUpdateMachineRendersStrategicPatches checks the owner's condition (issue #56 item 2):
// the strategicPatches of a generated TalosConfig are rendered through CABPT's pkg/templatepatch
// engine and patched in place, instead of being refused.
func TestCanUpdateMachineRendersStrategicPatches(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})
	desired := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-2"})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
	g.Expect(applier.applyRuns).To(BeZero())
}

// TestCanUpdateMachineRolloutForRebootRequiredRender checks that a rendered configuration
// whose change Talos can only apply after a reboot - here the machine install image, set
// through a strategic patch on a generated TalosConfig - gets no patch, so the Machine rolls out.
func TestCanUpdateMachineRolloutForRebootRequiredRender(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})
	desired := generatedConfig(
		[]string{hostnamePatch, `{"machine":{"install":{"image":"ghcr.io/siderolabs/installer:v1.14.5"}}}`},
		map[string]string{"hostname": "control-plane-1"},
	)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

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

// TestUpdateMachineWorkerMachineCompletes checks that a Machine whose node reports only a
// kubelet still finishes the staged apply instead of retrying forever.
func TestUpdateMachineWorkerMachineCompletes(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := NewHandler(applier, logr.Discard(), "", 0)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("worker-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, first)
	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))

	second := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, second)
	g.Expect(second.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(second.RetryAfterSeconds).To(BeZero(), "worker node never reports the update complete; applyRuns=%d", applier.applyRuns)
}

// TestCanUpdateMachineRolloutForWorkerMachineKubernetesVersion checks that a Kubernetes version change of a Machine
// whose node runs no control-plane components is not handled in place.
func TestCanUpdateMachineRolloutForWorkerMachineKubernetesVersion(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := NewHandler(applier, logr.Discard(), "", 0)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

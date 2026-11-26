// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeHandler builds a handler around a fake node. The metadata reader stands in for the
// management cluster objects the manager's hooks need: Machine status addresses, the cluster
// Talos secrets bundle and the cluster talosconfig secret. The manifest syncer is left nil so
// fake handlers do not try to reach a workload cluster unless a test installs its own.
func fakeHandler(applier Applier, metadata MetadataReader) *Handler {
	handler := newHandler(metadata, logr.Discard(), 0, func(_ string, _ *talosconfig.Config) Applier {
		return applier
	})
	handler.manifestSyncer = nil

	return handler
}

// fakeTalosConfig is a syntactically valid talosconfig the hooks decode from the Secret the
// bootstrap provider would have written for a cluster.
func fakeTalosConfig(clusterName string, endpoints ...string) []byte {
	config := &talosconfig.Config{
		Context: clusterName,
		Contexts: map[string]*talosconfig.Context{
			clusterName: {
				Endpoints: endpoints,
			},
		},
	}

	data, err := config.Bytes()
	if err != nil {
		panic(err)
	}

	return data
}

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
	// refuses stands in for an older Talos release that rejects a configuration in
	// immediate mode instead of reporting a reboot requirement, e.g. machine/env on
	// Talos v1.12.
	refuses string
	// nodes lets a test give a node of its own to each Talos endpoint, so the control
	// plane peers a manifest sync has to wait for can lag behind the machine being
	// updated.
	nodes map[string]NodeStatus
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

	if node, ok := f.nodes[endpoint]; ok {
		return node, nil
	}

	return f.node, nil
}

// BootstrapManifests stands in for GetBootstrapManifests on a real node: the kube-proxy
// DaemonSet Talos renders from the node's BootstrapManifestsConfig, carrying the image the
// node reports.
func (f *fakeApplier) BootstrapManifests(_ context.Context, endpoint string) ([]*unstructured.Unstructured, error) {
	node := f.node

	if status, ok := f.nodes[endpoint]; ok {
		node = status
	}

	if node.Proxy == "" {
		return nil, nil
	}

	return []*unstructured.Unstructured{
		{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "DaemonSet",
				"metadata": map[string]any{
					"name":      "kube-proxy",
					"namespace": "kube-system",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{"name": "kube-proxy", "image": node.Proxy},
							},
						},
					},
				},
			},
		},
	}, nil
}

func (f *fakeApplier) DryRun(_ context.Context, endpoint string, config []byte) (ApplyStatus, error) {
	f.dryRunRuns++
	f.endpoints = append(f.endpoints, endpoint)

	if f.refuses != "" {
		return ApplyStatus{}, errors.New(f.refuses)
	}

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
	handler := fakeHandler(applier, clusterMetadata())

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(),
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
	handler := fakeHandler(applier, clusterMetadata())

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":  "v1.14.0",
		"data":          baseConfig,
		"configPatches": []map[string]any{{"op": "replace", "path": "/machine/files/0/content", "value": "patched"}},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(), canUpdateRequest(talosConfig("talosconfig-1", baseConfig), desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
}

// TestCanUpdateMachineNoPatchForRebootRequired checks that a change Talos cannot apply
// without a reboot gets no patch, so Cluster API rolls the machine out.
func TestCanUpdateMachineNoPatchForRebootRequired(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(),
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
	handler := fakeHandler(applier, clusterMetadata())

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(),
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
			handler := fakeHandler(applier, clusterMetadata())

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(context.Background(), test.request, resp)

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
			handler := fakeHandler(applier, clusterMetadata())

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(context.Background(), canUpdateRequest(test.current, test.desired, "talos-image", "v1.33.1", "v1.34.0"), resp)

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

// TestCanUpdateMachineRolloutForNonAdjacentKubernetesVersion checks that Talos, which
// supports the Kubernetes versions around the one it ships, rolls out a jump of more than one
// minor version, a patch version and a downgrade instead of updating them in place.
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
			handler := fakeHandler(applier, clusterMetadata())

			resp := &runtimehooksv1.CanUpdateMachineResponse{}
			handler.CanUpdateMachine(context.Background(),
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
	handler := fakeHandler(applier, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for index, component := range kubernetesComponents {
		resp := &runtimehooksv1.UpdateMachineResponse{}
		handler.UpdateMachine(context.Background(), request, resp)

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
	handler.UpdateMachine(context.Background(), request, done)

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
	handler := fakeHandler(applier, clusterMetadata())

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
	handler.CanUpdateMachine(context.Background(), request, resp)

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
	handler := fakeHandler(applier, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, first)

	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	g.Expect(applier.applyRuns).To(Equal(1))

	second := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, second)

	g.Expect(second.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(second.RetryAfterSeconds).To(BeZero())
	g.Expect(applier.applyRuns).To(Equal(1))
}

// TestUpdateMachineFailsWhenRebootRequired checks the extension never applies a
// configuration the node would need a reboot for.
func TestUpdateMachineFailsWhenRebootRequired(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", rebootConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(applier.applyRuns).To(BeZero())
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

// TestCanUpdateMachineRendersStrategicPatches checks that the strategicPatches of a
// generated TalosConfig are rendered with the template data the bootstrap provider builds
// and patched in place, instead of being refused.
func TestCanUpdateMachineRendersStrategicPatches(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})
	desired := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-2"})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(), canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

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
	handler := fakeHandler(applier, clusterMetadata())

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})
	desired := generatedConfig(
		[]string{hostnamePatch, `{"machine":{"install":{"image":"ghcr.io/siderolabs/installer:v1.14.5"}}}`},
		map[string]string{"hostname": "control-plane-1"},
	)

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(), canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestUpdateMachineNeedsAnEndpoint checks that UpdateMachine fails when the Machine status has
// no address, instead of guessing which node to update.
func TestUpdateMachineNeedsAnEndpoint(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, &fakeMetadata{})

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "control-plane-1"}},
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(resp.Message).To(ContainSubstring("has no address in its status"))
	g.Expect(applier.applyRuns).To(BeZero())
	g.Expect(applier.endpoints).To(BeEmpty())
}

// TestManagerHooksReadTheTalosConfigAndAddressOfTheirCluster checks that a handler built with the
// manager client resolves the Talos API endpoint from the Machine status addresses and the
// talosconfig from the Secret of the cluster named in the request, rather than from one global
// extension setting.
func TestManagerHooksReadTheTalosConfigAndAddressOfTheirCluster(t *testing.T) {
	g := NewWithT(t)

	appliers := map[string]*fakeApplier{
		"cluster-a": newFakeApplier(),
		"cluster-b": newFakeApplier(),
	}

	configs := map[string]*talosconfig.Config{}

	client := fake.NewClientBuilder().WithObjects(
		statusMachine("control-plane-a", "cluster-a", "10.0.0.5"),
		statusMachine("control-plane-b", "cluster-b", "10.0.0.6"),
		talosConfigSecret("cluster-a", "10.0.0.5"),
		talosConfigSecret("cluster-b", "10.0.0.6"),
	).Build()

	handler := newHandler(NewKubeMetadataReader(client), logr.Discard(), 0, func(clusterName string, config *talosconfig.Config) Applier {
		configs[clusterName] = config

		return appliers[clusterName]
	})
	handler.manifestSyncer = nil

	cases := []struct {
		clusterName string
		machineName string
		address     string
	}{
		{clusterName: "cluster-a", machineName: "control-plane-a", address: "10.0.0.5"},
		{clusterName: "cluster-b", machineName: "control-plane-b", address: "10.0.0.6"},
	}

	for _, test := range cases {
		resp := &runtimehooksv1.CanUpdateMachineResponse{}
		handler.CanUpdateMachine(context.Background(), dataCanUpdateRequest(test.clusterName, test.machineName), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
		g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
		g.Expect(appliers[test.clusterName].dryRunRuns).To(Equal(1))
		g.Expect(appliers[test.clusterName].endpoints).To(ContainElement(test.address))
		g.Expect(configs[test.clusterName].Contexts[test.clusterName].Endpoints).To(ContainElement(test.address))
	}

	g.Expect(appliers["cluster-a"].endpoints).To(ContainElement("10.0.0.5"))
	g.Expect(appliers["cluster-b"].endpoints).To(ContainElement("10.0.0.6"))
}

func statusMachine(name, clusterName, address string) *unstructured.Unstructured {
	machine := &unstructured.Unstructured{}

	machine.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("Machine"))
	machine.SetName(name)
	machine.SetNamespace("default")
	machine.Object["spec"] = map[string]any{"clusterName": clusterName}
	machine.Object["status"] = map[string]any{
		"addresses": []any{
			map[string]any{"type": "InternalIP", "address": address},
		},
	}

	return machine
}

func talosConfigSecret(clusterName, endpoint string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + talosConfigSecretName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			talosConfigSecretKey: fakeTalosConfig(clusterName, endpoint),
		},
	}
}

func managerMachine(clusterName, name string) clusterv1.Machine {
	return clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Version:           "v1.33.1",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "TalosInfraMachine", Name: "infra-machine"},
		},
	}
}

func dataCanUpdateRequest(clusterName, machineName string) *runtimehooksv1.CanUpdateMachineRequest {
	machine := managerMachine(clusterName, machineName)

	return &runtimehooksv1.CanUpdateMachineRequest{
		Current: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               machine,
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", baseConfig),
		},
		Desired: runtimehooksv1.CanUpdateMachineRequestObjects{
			Machine:               machine,
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}
}

// TestUpdateMachineWorkerMachineCompletes checks that a Machine whose node reports only a
// kubelet still finishes the staged apply instead of retrying forever.
func TestUpdateMachineWorkerMachineCompletes(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := fakeHandler(applier, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("worker-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, first)
	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))

	second := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, second)
	g.Expect(second.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(second.RetryAfterSeconds).To(BeZero(), "worker node never reports the update complete; applyRuns=%d", applier.applyRuns)
}

// TestCanUpdateMachineRolloutForWorkerMachineKubernetesVersion checks that a Kubernetes version change of a Machine
// whose node runs no control-plane components is not handled in place.
func TestCanUpdateMachineRolloutForWorkerMachineKubernetesVersion(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := fakeHandler(applier, clusterMetadata())

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(context.Background(), canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestRebootRequiredReasonIsNotTalosVersionAware checks the cheap guard in front of the
// DryRun: only the paths Talos can never apply after booting the node again are listed, and
// a change under machine/env is left to the node, because whether Talos applies it live
// depends on the version the node runs.
func TestRebootRequiredReasonIsNotTalosVersionAware(t *testing.T) {
	g := NewWithT(t)

	envChange := []jsonPatchOp{{Op: "replace", Path: "/machine/env/E2E_REBOOT_REQUIRED", Value: "1"}}

	g.Expect(rebootRequiredReason(envChange)).To(BeEmpty(), "machine/env is decided by the DryRun of the node, not by its Talos version")

	g.Expect(rebootRequiredReason([]jsonPatchOp{{Op: "replace", Path: "/machine/install/image", Value: "ghcr.io/siderolabs/talos:v1.14.6"}})).NotTo(BeEmpty())
}

// TestUpdateMachineWaitsForMetadataBeforeApplyingConfiguration covers the two-phase bootstrap
// of a Metal3/CAPM3 machine: metadataFrom is set, but the metadata secret the infrastructure
// provider writes after the bootstrap data does not exist yet, so the rendered configuration
// is the phase-1 configuration, without the strategic patches that still contain template
// expressions. Applying it would give the node a machine configuration that is missing the
// patches its metadata renders, so the hook reports the update as in progress and Cluster API
// calls the hook again instead of updating the Machine with a partial configuration.
func TestUpdateMachineWaitsForMetadataBeforeApplyingConfiguration(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	metadata := clusterMetadata()
	handler := fakeHandler(applier, metadata)

	config := talosConfigRaw("talosconfig-2", map[string]any{
		"talosVersion": "v1.14.0",
		"data":         baseConfig,
		"metadataFrom": map[string]any{"secretRef": map[string]any{"name": "workers-metadata"}},
		"strategicPatches": []string{
			"machine:\n  sysctls:\n    net.ipv4.tcp_keepalive_time: \"{{ .keepalive }}\"\n",
		},
	})

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("worker-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       config,
		},
	}

	pending := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, pending)

	g.Expect(pending.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(pending.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)), "the machine stays in progress while its metadata is missing")
	g.Expect(applier.applyRuns).To(BeZero(), "the phase-1 configuration is not applied to the node")
	g.Expect(applier.applied["10.0.0.2"]).To(BeEmpty())

	// The infrastructure provider has created the metadata claim, so the retry renders the
	// whole configuration and applies it.
	metadata.withMetadata("workers-metadata", "keepalive: \"1000\"\n")

	done := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(applier.applyRuns).To(Equal(1))
	g.Expect(applier.applied["10.0.0.2"]).To(ContainSubstring("tcp_keepalive_time"))
	g.Expect(applier.applied["10.0.0.2"]).To(ContainSubstring("1000"))
}

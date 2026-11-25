// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The CanUpdateMachineSet hook of the MachineDeployment controller, the in-place update of
// worker Machines, and the Talos address Cluster API copies from the infrastructure machine
// into the Machine status.
package inplaceupdate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// workerMachineSet builds the MachineSet of a MachineDeployment rollout. The MachineSet of
// a rollout always points at an infrastructure machine template and a bootstrap config
// template, as createRequest of the MachineDeployment controller reads them
// (machinedeployment_canupdatemachineset.go: getTemplateObjects).
func workerMachineSet(name, version, templateName string) clusterv1.MachineSet {
	replicas := int32(2)

	return clusterv1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: clusterv1.MachineSetSpec{
			ClusterName: "test-cluster",
			Replicas:    &replicas,
			Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"component": "worker"}},
			Template: clusterv1.MachineTemplateSpec{
				ObjectMeta: clusterv1.ObjectMeta{Labels: map[string]string{"component": "worker"}},
				Spec: clusterv1.MachineSpec{
					ClusterName:       "test-cluster",
					Version:           version,
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "TalosInfraMachineTemplate", Name: "infra-template"},
					Bootstrap: clusterv1.Bootstrap{
						ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "TalosConfigTemplate", Name: templateName},
					},
				},
			},
		},
	}
}

// talosConfigTemplateRaw builds a TalosConfigTemplate object as the Runtime SDK passes it:
// cleanupUnstructured of the MachineDeployment controller keeps apiVersion, kind, metadata
// and spec, and the machine configuration lives in spec.template.spec.
func talosConfigTemplateRaw(name string, spec map[string]any) runtime.RawExtension {
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "bootstrap.cluster.x-k8s.io/v1alpha3",
		"kind":       "TalosConfigTemplate",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"template": map[string]any{"spec": spec}},
	})
	if err != nil {
		panic(err)
	}

	return runtime.RawExtension{Raw: raw}
}

func talosConfigTemplate(name, data string) runtime.RawExtension {
	return talosConfigTemplateRaw(name, map[string]any{
		"talosVersion": "v1.14.0",
		"data":         data,
	})
}

func infraMachineTemplate(name, image string) runtime.RawExtension {
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "TalosInfraMachineTemplate",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"template": map[string]any{"spec": map[string]any{"image": image}}},
	})
	if err != nil {
		panic(err)
	}

	return runtime.RawExtension{Raw: raw}
}

func canUpdateMachineSetRequest(currentSet, desiredSet clusterv1.MachineSet, currentTemplate, desiredTemplate, infraTemplate runtime.RawExtension) *runtimehooksv1.CanUpdateMachineSetRequest {
	return &runtimehooksv1.CanUpdateMachineSetRequest{
		Current: runtimehooksv1.CanUpdateMachineSetRequestObjects{
			MachineSet:                    currentSet,
			InfrastructureMachineTemplate: infraTemplate,
			BootstrapConfigTemplate:       currentTemplate,
		},
		Desired: runtimehooksv1.CanUpdateMachineSetRequestObjects{
			MachineSet:                    desiredSet,
			InfrastructureMachineTemplate: infraTemplate,
			BootstrapConfigTemplate:       desiredTemplate,
		},
	}
}

func unmarshalPatch(patch []byte) []jsonPatchOp {
	ops := []jsonPatchOp{}

	if err := json.Unmarshal(patch, &ops); err != nil {
		panic(err)
	}

	return ops
}

// TestCanUpdateMachineSetInPlaceForTemplateChange checks the end-to-end case: a new
// TalosConfigTemplate whose machine configuration differs in a way Talos applies without a
// reboot is answered with the patch of spec.template.spec, so the MachineSet is not rolled
// out and the same Machines are kept.
func TestCanUpdateMachineSetInPlaceForTemplateChange(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	request := canUpdateMachineSetRequest(
		workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
		workerMachineSet("workers", "v1.33.1", "talosconfig-2"),
		talosConfigTemplate("talosconfig-1", baseConfig),
		talosConfigTemplate("talosconfig-2", changedConfig),
		infraMachineTemplate("infra-template", "talos-image"),
	)

	resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
	handler.CanUpdateMachineSet(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigTemplatePatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))
	g.Expect(resp.BootstrapConfigTemplatePatch.Patch).NotTo(BeEmpty())

	ops := unmarshalPatch(resp.BootstrapConfigTemplatePatch.Patch)
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "replace", Path: "/spec/template/spec/data", Value: changedConfig}))
	g.Expect(resp.MachineSetPatch.Patch).To(BeEmpty())
	g.Expect(resp.InfrastructureMachineTemplatePatch.Patch).To(BeEmpty())
}

// TestCanUpdateMachineSetInPlaceForKubernetesVersion checks that a Kubernetes version change
// of the MachineSet template is patched on the MachineSet itself: the bootstrap template is
// unchanged, so only spec.template.spec.version is patched.
func TestCanUpdateMachineSetInPlaceForKubernetesVersion(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	request := canUpdateMachineSetRequest(
		workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
		workerMachineSet("workers", "v1.34.0", "talosconfig-2"),
		talosConfigTemplate("talosconfig-1", baseConfig),
		talosConfigTemplate("talosconfig-2", baseConfig),
		infraMachineTemplate("infra-template", "talos-image"),
	)

	resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
	handler.CanUpdateMachineSet(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.MachineSetPatch.PatchType).To(Equal(runtimehooksv1.JSONPatchType))

	ops := unmarshalPatch(resp.MachineSetPatch.Patch)
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "replace", Path: "/spec/template/spec/version", Value: "v1.34.0"}))
	g.Expect(resp.BootstrapConfigTemplatePatch.Patch).To(BeEmpty())
}

// TestCanUpdateMachineSetInPlaceForTemplateAndKubernetesVersion checks both patches together,
// the combination the e2e scenario inplace_workers drives.
func TestCanUpdateMachineSetInPlaceForTemplateAndKubernetesVersion(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	request := canUpdateMachineSetRequest(
		workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
		workerMachineSet("workers", "v1.34.0", "talosconfig-2"),
		talosConfigTemplate("talosconfig-1", baseConfig),
		talosConfigTemplate("talosconfig-2", changedConfig),
		infraMachineTemplate("infra-template", "talos-image"),
	)

	resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
	handler.CanUpdateMachineSet(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigTemplatePatch.Patch).NotTo(BeEmpty())
	g.Expect(resp.MachineSetPatch.Patch).NotTo(BeEmpty())
}

// TestCanUpdateMachineSetRendersAGeneratedTemplate checks that the machine configuration of a
// TalosConfigTemplate created with generateType is rendered the way the bootstrap provider
// renders it, so a strategic patch that only changes machine configuration is handled in
// place.
func TestCanUpdateMachineSetRendersAGeneratedTemplate(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, clusterMetadata())

	current := talosConfigTemplateRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"generateType":     "worker",
		"strategicPatches": []string{"machine:\n  files:\n    - path: /var/inplace/keepalive\n      content: \"600\"\n"},
	})
	desired := talosConfigTemplateRaw("talosconfig-2", map[string]any{
		"talosVersion":     "v1.14.0",
		"generateType":     "worker",
		"strategicPatches": []string{"machine:\n  files:\n    - path: /var/inplace/keepalive\n      content: \"900\"\n"},
	})

	resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
	handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
		workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
		workerMachineSet("workers", "v1.33.1", "talosconfig-2"),
		current,
		desired,
		infraMachineTemplate("infra-template", "talos-image"),
	), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigTemplatePatch.Patch).NotTo(BeEmpty())
	g.Expect(unmarshalPatch(resp.BootstrapConfigTemplatePatch.Patch)).To(ContainElement(jsonPatchOp{
		Op:    "replace",
		Path:  "/spec/template/spec/strategicPatches",
		Value: []any{"machine:\n  files:\n    - path: /var/inplace/keepalive\n      content: \"900\"\n"},
	}))
}

// TestCanUpdateMachineSetNoPatchForRolloutChanges checks the changes the extension never
// patches: a machine configuration Talos can only apply with a reboot, a Talos version
// change, an infrastructure machine template change, a Kubernetes version jump of more than
// one minor version, and a template that does not change at all. Every one of them leaves
// the MachineSet different from its desired state, which makes Cluster API roll it out.
func TestCanUpdateMachineSetNoPatchForRolloutChanges(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		name            string
		currentTemplate runtime.RawExtension
		desiredTemplate runtime.RawExtension
		infraTemplate   runtime.RawExtension
		version         string
		desiredVersion  string
	}{
		{name: "a reboot required machine configuration change", currentTemplate: talosConfigTemplate("talosconfig-1", baseConfig), desiredTemplate: talosConfigTemplate("talosconfig-2", rebootConfig), infraTemplate: infraMachineTemplate("infra-template", "talos-image"), version: "v1.33.1", desiredVersion: "v1.33.1"},
		{name: "a Talos version change", currentTemplate: talosConfigTemplate("talosconfig-1", baseConfig), desiredTemplate: talosConfigTemplateRaw("talosconfig-2", map[string]any{"talosVersion": "v1.15.0", "data": baseConfig}), infraTemplate: infraMachineTemplate("infra-template", "talos-image"), version: "v1.33.1", desiredVersion: "v1.33.1"},
		{name: "an infrastructure machine template change", currentTemplate: talosConfigTemplate("talosconfig-1", baseConfig), desiredTemplate: talosConfigTemplate("talosconfig-2", baseConfig), infraTemplate: infraMachineTemplate("infra-template", "talos-image-2"), version: "v1.33.1", desiredVersion: "v1.33.1"},
		{name: "a Kubernetes version jump of two minor versions", currentTemplate: talosConfigTemplate("talosconfig-1", baseConfig), desiredTemplate: talosConfigTemplate("talosconfig-2", baseConfig), infraTemplate: infraMachineTemplate("infra-template", "talos-image"), version: "v1.33.1", desiredVersion: "v1.35.0"},
		{name: "no change at all", currentTemplate: talosConfigTemplate("talosconfig-1", baseConfig), desiredTemplate: talosConfigTemplate("talosconfig-2", baseConfig), infraTemplate: infraMachineTemplate("infra-template", "talos-image"), version: "v1.33.1", desiredVersion: "v1.33.1"},
	} {
		applier := newFakeApplier()
		handler := fakeHandler(applier, clusterMetadata())

		resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
		handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
			workerMachineSet("workers", test.version, "talosconfig-1"),
			workerMachineSet("workers", test.desiredVersion, "talosconfig-2"),
			test.currentTemplate,
			test.desiredTemplate,
			test.infraTemplate,
		), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess), test.name)
		g.Expect(resp.BootstrapConfigTemplatePatch.Patch).To(BeEmpty(), "%s: the bootstrap config template must not be patched", test.name)
		g.Expect(resp.MachineSetPatch.Patch).To(BeEmpty(), "%s: the machine set must not be patched", test.name)
		g.Expect(resp.InfrastructureMachineTemplatePatch.Patch).To(BeEmpty(), "%s: the infrastructure machine template is never patched", test.name)
	}
}

// TestCanUpdateMachineSetNoPatchWithoutATalosConfigTemplate checks the MachineSets the
// extension cannot patch because their bootstrap configuration is not a TalosConfigTemplate:
// bootstrap data instead of a template, and a template of another bootstrap provider. Neither
// is an error, so the hook answers Success with no patch, which leaves the MachineSet
// different from its desired state and makes Cluster API roll it out.
func TestCanUpdateMachineSetNoPatchWithoutATalosConfigTemplate(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		name            string
		currentSet      clusterv1.MachineSet
		desiredSet      clusterv1.MachineSet
		currentTemplate runtime.RawExtension
		desiredTemplate runtime.RawExtension
	}{
		{
			name:            "bootstrap data instead of a bootstrap config template",
			currentSet:      machineSetWithBootstrapData("workers", "v1.33.1", "talosconfig-data-1"),
			desiredSet:      machineSetWithBootstrapData("workers", "v1.33.1", "talosconfig-data-2"),
			currentTemplate: runtime.RawExtension{},
			desiredTemplate: runtime.RawExtension{},
		},
		{
			name:            "a bootstrap config template of another provider",
			currentSet:      workerMachineSet("workers", "v1.33.1", "cloudinit-1"),
			desiredSet:      workerMachineSet("workers", "v1.33.1", "cloudinit-2"),
			currentTemplate: rawConfigTemplate("cloudinit-1", "CloudInitConfigTemplate"),
			desiredTemplate: rawConfigTemplate("cloudinit-2", "CloudInitConfigTemplate"),
		},
	} {
		applier := newFakeApplier()
		handler := fakeHandler(applier, clusterMetadata())

		resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
		handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
			test.currentSet,
			test.desiredSet,
			test.currentTemplate,
			test.desiredTemplate,
			infraMachineTemplate("infra-template", "talos-image"),
		), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess), test.name)
		g.Expect(resp.Message).To(BeEmpty(), "%s: a machine set the extension cannot patch is not a failure", test.name)
		g.Expect(resp.BootstrapConfigTemplatePatch.Patch).To(BeEmpty(), test.name)
		g.Expect(resp.MachineSetPatch.Patch).To(BeEmpty(), test.name)
	}
}

// machineSetWithBootstrapData builds a MachineSet whose machines carry bootstrap data
// instead of a reference to a bootstrap config template.
func machineSetWithBootstrapData(name, version, secretName string) clusterv1.MachineSet {
	machineSet := workerMachineSet(name, version, "")
	machineSet.Spec.Template.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{}
	machineSet.Spec.Template.Spec.Bootstrap.DataSecretName = &secretName

	return machineSet
}

// rawConfigTemplate builds a bootstrap config template object of any provider.
func rawConfigTemplate(name, kind string) runtime.RawExtension {
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "bootstrap.cluster.x-k8s.io/v1beta2",
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"template": map[string]any{"spec": map[string]any{}}},
	})
	if err != nil {
		panic(err)
	}

	return runtime.RawExtension{Raw: raw}
}

// TestCanUpdateMachineWorkerMachineConfigChange checks that a worker Machine of a
// MachineSet whose node reports only a kubelet is handled by the machine configuration patch
// and the DryRun of the node.
func TestCanUpdateMachineWorkerMachineConfigChange(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := fakeHandler(applier, clusterMetadata())

	current := machine("worker-1", "v1.33.1")
	current.Spec.ProviderID = "talos://10.0.0.2"

	desired := machine("worker-1", "v1.33.1")
	desired.Spec.ProviderID = "talos://10.0.0.2"

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
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
	g.Expect(applier.endpoints).To(ContainElement("10.0.0.2"))
}

// TestUpdateMachineWorkerKubernetesVersionUpgradesKubeletOnly checks the worker half of the
// owner's condition (ADR-0003 as amended on 2026-11-15): the Kubernetes version of a worker
// Machine is changed in place, and the configuration applied to the node patches the kubelet
// only - the components of a control plane are not touched, as talosctl upgrade-k8s does for
// a node without control-plane components
// (talosctl upgrade-k8s: upgradeKubeletOnNode).
func TestUpdateMachineWorkerKubernetesVersionUpgradesKubeletOnly(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}
	handler := fakeHandler(applier, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("worker-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", baseConfig),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, first)

	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	g.Expect(applier.applyRuns).To(Equal(1))

	applied := applier.applied["10.0.0.2"]
	g.Expect(applied).To(ContainSubstring("kubelet:v1.34.0"))
	g.Expect(applied).NotTo(ContainSubstring("kube-apiserver"), "a worker Machine has no kube-apiserver to upgrade")
	g.Expect(applier.node.Kubelet).To(Equal("ghcr.io/siderolabs/kubelet:v1.34.0-fat"))
	g.Expect(componentsAtTarget(applier.node, "v1.34.0")).To(BeTrue())

	done := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(done.RetryAfterSeconds).To(BeZero())
	g.Expect(applier.applyRuns).To(Equal(1))
}

// TestUpdateMachineUsesTheAddressOfTheMachine checks that the Talos API endpoint is the
// address the infrastructure provider reported for the machine, which Cluster API copies into
// the Machine status (clusterv1.MachineStatus.Addresses: "This field is copied from the
// infrastructure provider reference"), before the ExtensionConfig settings and the ProviderID.
func TestUpdateMachineUsesTheAddressOfTheMachine(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	applier.node = NodeStatus{Kubelet: "ghcr.io/siderolabs/kubelet:v1.33.1-fat"}

	metadata := &fakeMetadataWithAddresses{fakeMetadata: clusterMetadata(), addresses: []clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
	}}

	handler := fakeHandler(applier, metadata)

	worker := machine("worker-1", "v1.33.1")
	worker.Spec.ProviderID = "talos://10.0.0.2"

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               worker,
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(applier.endpoints).NotTo(BeEmpty())

	for _, endpoint := range applier.endpoints {
		g.Expect(endpoint).To(Equal("10.0.0.5"), "the node was addressed at %q instead of the address of the Machine", endpoint)
	}
}

// fakeMetadataWithAddresses is the fake management cluster of render_test.go plus the
// Machine status addresses the hooks read to reach a node.
type fakeMetadataWithAddresses struct {
	*fakeMetadata

	addresses []clusterv1.MachineAddress
	err       error
}

func (f *fakeMetadataWithAddresses) MachineAddresses(_ context.Context, _, _ string) ([]clusterv1.MachineAddress, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.addresses, nil
}

// TestUpdateMachineFailsWhenTheMachineHasNoAddress checks that a Machine status without
// addresses is not replaced by an ExtensionConfig setting or a ProviderID fallback.
func TestUpdateMachineFailsWhenTheMachineHasNoAddress(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := fakeHandler(applier, &fakeMetadataWithAddresses{fakeMetadata: clusterMetadata()})

	request := &runtimehooksv1.UpdateMachineRequest{
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("worker-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(context.Background(), request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(resp.Message).To(ContainSubstring("has no address in its status"))
	g.Expect(applier.endpoints).To(BeEmpty())
	g.Expect(applier.applyRuns).To(BeZero())
}

// machineEnvConfig is the machine configuration of the reboot-required step of the in-place
// e2e test: a variable under machine/env.
const machineEnvConfig = `version: v1alpha1
machine:
  env:
    E2E_REBOOT_REQUIRED: "1"
  network:
    hostname: worker-1
  files:
    - path: /var/inplace/first
      content: original
cluster:
  token: bootstrap-token
`

// machineSetNodes is the fake management cluster of render_test.go plus the Machines of a
// machine set and the addresses their infrastructure providers reported, which is what
// CanUpdateMachineSet needs to DryRun the machine configuration of a rollout against a node.
type machineSetNodes struct {
	*fakeMetadata

	machines []machineSetNode
	err      error
}

type machineSetNode struct {
	name      string
	addresses []clusterv1.MachineAddress
}

func (f *machineSetNodes) MachineSetMachines(_ context.Context, _, _ string) ([]clusterv1.Machine, error) {
	if f.err != nil {
		return nil, f.err
	}

	machines := make([]clusterv1.Machine, 0, len(f.machines))

	for _, node := range f.machines {
		machines = append(machines, clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: node.name, Namespace: "default"},
			Spec:       clusterv1.MachineSpec{ClusterName: "test-cluster"},
		})
	}

	return machines, nil
}

func (f *machineSetNodes) MachineAddresses(_ context.Context, _, name string) ([]clusterv1.MachineAddress, error) {
	for _, node := range f.machines {
		if node.name == name {
			return node.addresses, nil
		}
	}

	return nil, nil
}

// TestCanUpdateMachineSetDryRunsTheMachineConfigurationAgainstItsNodes checks the owner's
// condition of docs/adr/ADR-0003 for a machine set: the hook asks a live Machine of the
// machine set whether the rendered desired configuration is accepted with mode
// ApplyConfigurationRequest_NO_REBOOT, and a node that needs a reboot or that refuses the
// configuration in immediate mode - machine/env on Talos v1.12, "this config change can't be
// applied in immediate mode" - gets no patch, so Cluster API rolls the MachineSet out instead
// of starting an in-place update the node can never finish. The Talos version of the template
// is not consulted: the node is the authority.
func TestCanUpdateMachineSetDryRunsTheMachineConfigurationAgainstItsNodes(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		name      string
		refuses   string
		reboot    bool
		wantPatch bool
	}{
		{name: "the node applies machine/env with mode NO_REBOOT", wantPatch: true},
		{name: "the node needs a reboot for machine/env", reboot: true, wantPatch: false},
		{name: "the node refuses machine/env in immediate mode", refuses: "rpc error: code = InvalidArgument desc = this config change can't be applied in immediate mode", wantPatch: false},
	} {
		applier := newFakeApplier()
		applier.refuses = test.refuses
		applier.alwaysReboot = test.reboot

		nodes := &machineSetNodes{
			fakeMetadata: clusterMetadata(),
			machines: []machineSetNode{
				{name: "worker-1", addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"}}},
			},
		}

		handler := fakeHandler(applier, nodes)

		resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
		handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
			workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
			workerMachineSet("workers", "v1.33.1", "talosconfig-2"),
			talosConfigTemplateRaw("talosconfig-1", map[string]any{"talosVersion": "v1.12.12", "data": baseConfig}),
			talosConfigTemplateRaw("talosconfig-2", map[string]any{"talosVersion": "v1.12.12", "data": machineEnvConfig}),
			infraMachineTemplate("infra-template", "talos-image"),
		), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess), test.name)

		if test.wantPatch {
			g.Expect(resp.BootstrapConfigTemplatePatch.Patch).NotTo(BeEmpty(), test.name)
		} else {
			g.Expect(resp.BootstrapConfigTemplatePatch.Patch).To(BeEmpty(), test.name)
		}

		g.Expect(resp.MachineSetPatch.Patch).To(BeEmpty(), test.name)
		g.Expect(resp.InfrastructureMachineTemplatePatch.Patch).To(BeEmpty(), test.name)
		g.Expect(applier.dryRunRuns).To(Equal(1), test.name)
		g.Expect(applier.endpoints).To(ContainElement("10.0.0.5"), test.name)
	}
}

// TestCanUpdateMachineSetPatchesWithoutALiveMachine checks the two cases in which no node can
// be asked and the hook still patches the template: a machine set with no Machines has no
// node to refuse the change - the patch only changes the template the next Machines are built
// from - and an extension started without --kubeconfig has no reader for the Machines of the
// machine set, exactly like the other optional capabilities of the metadata reader.
func TestCanUpdateMachineSetPatchesWithoutALiveMachine(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		name     string
		metadata MetadataReader
	}{
		{name: "the machine set has no machines", metadata: &machineSetNodes{fakeMetadata: clusterMetadata()}},
		{name: "the extension has no reader for the machines of the machine set", metadata: clusterMetadata()},
	} {
		applier := newFakeApplier()
		handler := fakeHandler(applier, test.metadata)

		resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
		handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
			workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
			workerMachineSet("workers", "v1.33.1", "talosconfig-2"),
			talosConfigTemplateRaw("talosconfig-1", map[string]any{"talosVersion": "v1.12.12", "data": baseConfig}),
			talosConfigTemplateRaw("talosconfig-2", map[string]any{"talosVersion": "v1.12.12", "data": machineEnvConfig}),
			infraMachineTemplate("infra-template", "talos-image"),
		), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess), test.name)
		g.Expect(resp.BootstrapConfigTemplatePatch.Patch).NotTo(BeEmpty(), test.name)
		g.Expect(applier.dryRunRuns).To(BeZero(), test.name)
	}
}

// TestCanUpdateMachineSetRollsOutWhenNoNodeIsDryRun checks that an in-place update nobody
// DryRunned is not approved: when the Machines of the machine set cannot be read, or when
// none of them can be reached, the hook returns no patch and Cluster API rolls the MachineSet
// out, instead of patching the template for machines whose node was never asked whether it
// accepts the machine configuration with mode ApplyConfigurationRequest_NO_REBOOT.
func TestCanUpdateMachineSetRollsOutWhenNoNodeIsDryRun(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		name     string
		metadata *machineSetNodes
	}{
		{
			name:     "the machines of the machine set cannot be read",
			metadata: &machineSetNodes{fakeMetadata: clusterMetadata(), err: errors.New("the management cluster did not answer")},
		},
		{
			name: "no machine of the machine set has an address",
			metadata: &machineSetNodes{
				fakeMetadata: clusterMetadata(),
				machines: []machineSetNode{
					{name: "worker-1"},
					{name: "worker-2"},
				},
			},
		},
	} {
		applier := newFakeApplier()
		handler := fakeHandler(applier, test.metadata)

		resp := &runtimehooksv1.CanUpdateMachineSetResponse{}
		handler.CanUpdateMachineSet(context.Background(), canUpdateMachineSetRequest(
			workerMachineSet("workers", "v1.33.1", "talosconfig-1"),
			workerMachineSet("workers", "v1.33.1", "talosconfig-2"),
			talosConfigTemplateRaw("talosconfig-1", map[string]any{"talosVersion": "v1.12.12", "data": baseConfig}),
			talosConfigTemplateRaw("talosconfig-2", map[string]any{"talosVersion": "v1.12.12", "data": machineEnvConfig}),
			infraMachineTemplate("infra-template", "talos-image"),
		), resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess), test.name)
		g.Expect(resp.BootstrapConfigTemplatePatch.Patch).To(BeEmpty(), test.name)
		g.Expect(resp.MachineSetPatch.Patch).To(BeEmpty(), test.name)
		g.Expect(applier.dryRunRuns).To(BeZero(), test.name)
	}
}

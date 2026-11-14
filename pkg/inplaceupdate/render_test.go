// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Rendering of the machine configuration a TalosConfig describes: a TalosConfig created
// with generateType is generated the way the bootstrap provider generates it, and its
// strategicPatches are rendered through the bootstrap provider's pkg/templatepatch engine
// before the configuration is diffed.
package inplaceupdate

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// fakeMetadata stands in for the management cluster objects the extension reads to render
// a machine configuration: the Cluster a machine belongs to, the Talos secrets bundle the
// bootstrap provider generated for that cluster, and the metadata secrets a metadataFrom
// TalosConfig points at.
type fakeMetadata struct {
	cluster    *clusterv1.Cluster
	secrets    map[string]map[string][]byte
	clusterErr error
}

func (f *fakeMetadata) Cluster(_ context.Context, _, name string) (*clusterv1.Cluster, error) {
	if f.clusterErr != nil {
		return nil, f.clusterErr
	}

	if f.cluster == nil || f.cluster.Name != name {
		return nil, errors.Errorf("cluster %q does not exist", name)
	}

	return f.cluster, nil
}

func (f *fakeMetadata) Secret(_ context.Context, _, name string) (map[string][]byte, error) {
	return f.secrets[name], nil
}

// clusterMetadata builds the cluster and the Talos secrets bundle a generated machine
// configuration needs. The bundle is written once, so rendering the current and the
// desired configuration of the same TalosConfig produces the same configuration.
func clusterMetadata() *fakeMetadata {
	bundle, err := secrets.NewBundle(secrets.NewFixedClock(time.Now()), config.TalosVersionCurrent)
	if err != nil {
		panic(err)
	}

	bundleYAML, err := yaml.Marshal(bundle)
	if err != nil {
		panic(err)
	}

	return &fakeMetadata{
		cluster: &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "10.0.0.1", Port: 6443},
				ClusterNetwork: clusterv1.ClusterNetwork{
					ServiceDomain: "cluster.local",
					Pods:          clusterv1.NetworkRanges{CIDRBlocks: []string{"10.244.0.0/16"}},
					Services:      clusterv1.NetworkRanges{CIDRBlocks: []string{"10.96.0.0/12"}},
				},
			},
		},
		secrets: map[string]map[string][]byte{
			"test-cluster-talos": {"bundle": bundleYAML},
		},
	}
}

// withMetadata adds a metadata secret to the fake cluster objects.
func (f *fakeMetadata) withMetadata(name string, data string) *fakeMetadata {
	f.secrets[name] = map[string][]byte{"metaData": []byte(data)}

	return f
}

// generatedConfig is a TalosConfig created with generateType.
func generatedConfig(patches []string, variables map[string]string) runtime.RawExtension {
	return talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":      "v1.14.0",
		"generateType":      "controlplane",
		"strategicPatches":  patches,
		"templateVariables": variables,
	})
}

// hostnamePatch renders the hostname of the machine from the template context.
const hostnamePatch = "machine:\n  network:\n    hostname: {{ get \"hostname\" }}\n"

// TestCanUpdateMachineRendersGeneratedConfig checks that two TalosConfigs generated with
// generateType render to the same machine configuration, so the extension does not see a
// change where there is none.
func TestCanUpdateMachineRendersGeneratedConfig(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	config := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(config, config, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestCanUpdateMachineRolloutWithoutClusterAccess checks that a generated TalosConfig is
// answered with Success and no patch when the extension was started without a kubeconfig:
// it cannot generate the machine configuration, so Cluster API rolls the Machine out
// instead of the hook call turning into an error.
func TestCanUpdateMachineRolloutWithoutClusterAccess(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	config := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(config, config, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestCanUpdateMachineRendersMetadata checks that a metadataFrom TalosConfig renders its
// strategicPatches with the metadata secret the infrastructure provider (Metal3/CAPM3)
// created, plus the machine and cluster context the bootstrap provider adds.
func TestCanUpdateMachineRendersMetadata(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	metadata := clusterMetadata().withMetadata("infra-machine-metadata", "hostname: control-plane-1\nrole: controlplane\n")
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, metadata)

	patch := "machine:\n  network:\n    hostname: {{ get \"hostname\" }}-{{ get \"role\" }}\n"

	current := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"generateType":     "controlplane",
		"metadataFrom":     map[string]any{"infrastructureMachine": map[string]any{}},
		"strategicPatches": []string{patch},
	})

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"generateType":     "controlplane",
		"metadataFrom":     map[string]any{"infrastructureMachine": map[string]any{}},
		"strategicPatches": []string{"machine:\n  network:\n    hostname: {{ get \"cluster.name\" }}-{{ get \"role\" }}\n"},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
}

// TestCanUpdateMachineRolloutForMetadataNotYetAvailable checks the two-phase bootstrap of
// Metal3/CAPM3: the metadata claim is created after the bootstrap data, so while the
// metadata secret is missing the extension renders the same template-free configuration
// the bootstrap provider writes in phase 1 and the Machine rolls out.
func TestCanUpdateMachineRolloutForMetadataNotYetAvailable(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	config := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"generateType":     "controlplane",
		"metadataFrom":     map[string]any{"infrastructureMachine": map[string]any{}},
		"strategicPatches": []string{hostnamePatch},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(config, config, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestCanUpdateMachineRendersStrategicPatchesOnData checks that strategic patches on a
// TalosConfig that supplies its own machine configuration are applied through the
// bootstrap provider's configpatcher before the configuration is diffed, so changing a
// patch is an in-place update rather than a rollout.
func TestCanUpdateMachineRendersStrategicPatchesOnData(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandler(applier, logr.Discard(), "", 0)

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":     "v1.14.0",
		"data":             baseConfig,
		"strategicPatches": []string{"machine:\n  network:\n    hostname: control-plane-2\n"},
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(talosConfig("talosconfig-1", baseConfig), desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.BootstrapConfigPatch.Patch).NotTo(BeEmpty())
	g.Expect(applier.dryRunRuns).To(Equal(1))
}

// TestCanUpdateMachineRolloutForTemplateError checks that a strategic patch that cannot be
// rendered gets no patch: the extension does not guess a machine configuration, so Cluster
// API rolls the Machine out instead of the hook call failing.
func TestCanUpdateMachineRolloutForTemplateError(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})
	desired := generatedConfig([]string{`machine:
  network:
    hostname: {{ get "hostname" `}, map[string]string{"hostname": "control-plane-2"})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestCanUpdateMachineGeneratedConfigWinsOverData checks that a TalosConfig with a generateType
// renders from the generated machine configuration even when it also carries a data field:
// the bootstrap provider's reconcileGenerate switch ignores spec.data for a machine type,
// so the current generated TalosConfig and the same TalosConfig with a different data field
// render to the same machine configuration and the hook answers Success with no patch.
func TestCanUpdateMachineGeneratedConfigWinsOverData(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	metadata := clusterMetadata()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, metadata)

	current := generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"})

	desired := talosConfigRaw("talosconfig-1", map[string]any{
		"talosVersion":      "v1.14.0",
		"generateType":      "controlplane",
		"strategicPatches":  []string{hostnamePatch},
		"templateVariables": map[string]string{"hostname": "control-plane-1"},
		"data":              changedConfig,
	})

	resp := &runtimehooksv1.CanUpdateMachineResponse{}
	handler.CanUpdateMachine(canUpdateRequest(current, desired, "talos-image", "v1.33.1", "v1.33.1"), resp)

	expectNoInPlaceUpdate(g, resp, applier)
}

// TestUpdateMachineIdempotentGeneratedConfig checks UpdateMachine with a generated machine
// configuration: the rendered configuration is applied once, and a second call with the
// same TalosConfig reports the update as finished.
func TestUpdateMachineIdempotentGeneratedConfig(t *testing.T) {
	g := NewWithT(t)
	applier := newFakeApplier()
	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, clusterMetadata())

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.33.1"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       generatedConfig([]string{hostnamePatch}, map[string]string{"hostname": "control-plane-1"}),
		},
	}

	first := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, first)

	g.Expect(first.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(first.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	g.Expect(applier.applyRuns).To(Equal(1))
	g.Expect(applier.applied["10.0.0.1"]).To(ContainSubstring("control-plane-1"))

	second := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, second)

	g.Expect(second.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(second.RetryAfterSeconds).To(BeZero())
	g.Expect(applier.applyRuns).To(Equal(1))
}

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// recordingSyncer stands in for the ManifestSyncer of pkg/inplaceupdate/manifestsync.go:
// it records every sync call instead of talking to a workload cluster.
type recordingSyncer struct {
	calls        int
	kubeconfig   []byte
	manifests    []*unstructured.Unstructured
	talosVersion string
	err          error
}

func (s *recordingSyncer) SyncManifests(_ context.Context, kubeconfig []byte, objects []*unstructured.Unstructured, talosVersion string) error {
	s.calls++
	s.kubeconfig = kubeconfig
	s.manifests = objects
	s.talosVersion = talosVersion

	return s.err
}

// syncReader is a MetadataReader with the optional MachineAddressReader and
// ControlPlaneMachineReader capabilities, so a test can hand the handler the control
// plane of a cluster and the kubeconfig secret Cluster API stores for it.
type syncReader struct {
	machines   []clusterv1.Machine
	addresses  map[string][]clusterv1.MachineAddress
	kubeconfig []byte
}

func (r *syncReader) Cluster(_ context.Context, _, _ string) (*clusterv1.Cluster, error) {
	return nil, nil
}

func (r *syncReader) Secret(_ context.Context, _, name string) (map[string][]byte, error) {
	if name != "test-cluster-kubeconfig" {
		return nil, nil
	}

	return map[string][]byte{"value": r.kubeconfig}, nil
}

func (r *syncReader) MachineAddresses(_ context.Context, _, name string) ([]clusterv1.MachineAddress, error) {
	return r.addresses[name], nil
}

func (r *syncReader) ControlPlaneMachines(_ context.Context, _, _ string) ([]clusterv1.Machine, error) {
	return r.machines, nil
}

// TestUpdateMachineSyncsBootstrapManifestsOnceControlPlaneIsAtTarget covers the stale
// kube-proxy DaemonSet of a workload cluster after an in-place Kubernetes upgrade: the
// Machine being updated reaches the target Kubernetes version, but the bootstrap manifests
// the node rendered for that version are never synced to the cluster. The sync runs once,
// on the control-plane Machine whose peers are already at the target version, and a
// Machine whose peers are not there yet completes without syncing, so the rollout of the
// control plane is never held open by a Machine waiting for a peer that has not been
// triggered yet.
func TestUpdateMachineSyncsBootstrapManifestsOnceControlPlaneIsAtTarget(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = controlPlaneNode()
	// The peer of the machine being updated still runs the old Kubernetes version.
	applier.nodes = map[string]NodeStatus{"10.0.0.2": controlPlaneNode()}

	syncer := &recordingSyncer{}
	reader := &syncReader{
		machines: []clusterv1.Machine{
			machine("control-plane-1", "v1.34.0"),
			machine("control-plane-2", "v1.34.0"),
		},
		addresses: map[string][]clusterv1.MachineAddress{
			"control-plane-2": {{Type: clusterv1.MachineAddressType("InternalIP"), Address: "10.0.0.2"}},
		},
		kubeconfig: []byte("kubeconfig-of-test-cluster"),
	}

	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	handler.manifestSyncer = syncer

	// Cluster API runs CanUpdateMachine before the first UpdateMachine request.
	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"),
		&runtimehooksv1.CanUpdateMachineResponse{},
	)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		resp := &runtimehooksv1.UpdateMachineResponse{}
		handler.UpdateMachine(request, resp)

		g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
		g.Expect(resp.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	}

	g.Expect(componentsAtTarget(applier.node, "v1.34.0")).To(BeTrue())

	// The machine being updated is at the target version, but its control-plane peer
	// still runs the old one: this Machine's update is complete and the sync waits for
	// the peer, which the trigger has not rolled out yet.
	lagging := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, lagging)

	g.Expect(lagging.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(lagging.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(BeZero())

	// The peer reaches the target version, so the kube-proxy manifest of the new version
	// is synced to the workload cluster with the kubeconfig of that cluster.
	peer := newFakeApplier()
	peer.node = controlPlaneNode()

	peerHandler := NewHandlerWithMetadata(peer, logr.Discard(), "", 0, reader)
	peerRequest := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.2"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-2", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		peerHandler.UpdateMachine(peerRequest, &runtimehooksv1.UpdateMachineResponse{})
	}

	applier.nodes["10.0.0.2"] = peer.node

	done := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(done.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(Equal(1))
	g.Expect(syncer.kubeconfig).To(Equal([]byte("kubeconfig-of-test-cluster")))
	g.Expect(syncer.talosVersion).To(Equal("v1.14.0"))

	containers, found, err := unstructured.NestedSlice(syncer.manifests[0].Object, "spec", "template", "spec", "containers")
	g.Expect(err).To(Succeed())
	g.Expect(found).To(BeTrue())
	g.Expect(containers[0]).To(Equal(map[string]any{"name": "kube-proxy", "image": "registry.k8s.io/kube-proxy:v1.34.0"}))

	// The sync already ran for this cluster and version: a further hook call completes
	// without syncing again.
	again := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, again)

	g.Expect(again.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(again.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(Equal(1))
}

// TestUpdateMachineKeepsTheUpdateInProgressWhileManifestsReconcile covers a sync whose
// objects are applied but not reconciled in the workload cluster yet. The hook reports the
// update as in progress instead of completing it with a kube-proxy that is still the old
// one, and the next call syncs again: the apply is idempotent, so the retry only waits.
func TestUpdateMachineKeepsTheUpdateInProgressWhileManifestsReconcile(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = controlPlaneNode()

	syncer := &recordingSyncer{
		err: fmt.Errorf("%w: kube-proxy status is still InProgress", errManifestsNotReconciled),
	}
	reader := &syncReader{
		machines:   []clusterv1.Machine{machine("control-plane-1", "v1.34.0")},
		kubeconfig: []byte("kubeconfig-of-test-cluster"),
	}

	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	handler.manifestSyncer = syncer

	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"),
		&runtimehooksv1.CanUpdateMachineResponse{},
	)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		handler.UpdateMachine(request, &runtimehooksv1.UpdateMachineResponse{})
	}

	inProgress := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, inProgress)

	g.Expect(inProgress.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(inProgress.RetryAfterSeconds).To(Equal(int32(DefaultRetryAfterSeconds)))
	g.Expect(syncer.calls).To(Equal(1))

	// The workload cluster has reconciled the manifests, so the retry completes the update.
	syncer.err = nil

	done := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(done.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(Equal(2))
}

// TestUpdateMachineSyncsBootstrapManifestsAfterARestart checks that the sync is decided from
// the state of the control plane rather than from anything an earlier hook call remembered.
// A handler created after the upgrade is already underway - Cluster API restarting the
// extension, for example - still syncs the manifests once the last control-plane Machine is
// at the target Kubernetes version.
func TestUpdateMachineSyncsBootstrapManifestsAfterARestart(t *testing.T) {
	g := NewWithT(t)

	syncer := &recordingSyncer{}
	reader := &syncReader{
		machines: []clusterv1.Machine{
			machine("control-plane-1", "v1.34.0"),
			machine("control-plane-2", "v1.34.0"),
		},
		addresses: map[string][]clusterv1.MachineAddress{
			"control-plane-2": {{Type: clusterv1.MachineAddressType("InternalIP"), Address: "10.0.0.2"}},
		},
		kubeconfig: []byte("kubeconfig-of-test-cluster"),
	}

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	// The first control-plane Machine is staged and applied component by component.
	applier := newFakeApplier()
	applier.node = controlPlaneNode()
	applier.nodes = map[string]NodeStatus{"10.0.0.2": controlPlaneNode()}

	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	handler.manifestSyncer = syncer

	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"),
		&runtimehooksv1.CanUpdateMachineResponse{},
	)

	for range kubernetesComponents {
		handler.UpdateMachine(request, &runtimehooksv1.UpdateMachineResponse{})
	}

	g.Expect(componentsAtTarget(applier.node, "v1.34.0")).To(BeTrue())

	// The extension restarts: this handler knows neither the CanUpdateMachine call nor any
	// earlier UpdateMachine call.
	restarted := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	restarted.manifestSyncer = syncer

	// The peer still runs the old Kubernetes version, so this Machine completes without
	// syncing: the sync belongs to the Machine whose peers are already at the target.
	lagging := &runtimehooksv1.UpdateMachineResponse{}
	restarted.UpdateMachine(request, lagging)

	g.Expect(lagging.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(lagging.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(BeZero())

	peer := newFakeApplier()
	peer.node = controlPlaneNode()
	peerHandler := NewHandlerWithMetadata(peer, logr.Discard(), "", 0, reader)

	peerRequest := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.2"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-2", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		peerHandler.UpdateMachine(peerRequest, &runtimehooksv1.UpdateMachineResponse{})
	}

	applier.nodes["10.0.0.2"] = peer.node

	// Every control-plane Machine is at the target version, so the restarted handler syncs.
	done := &runtimehooksv1.UpdateMachineResponse{}
	restarted.UpdateMachine(request, done)

	g.Expect(done.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(done.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(Equal(1))
}

// TestUpdateMachineReportsManifestSyncFailure makes a failing sync visible to Cluster
// API instead of completing the update with a stale kube-proxy.
func TestUpdateMachineReportsManifestSyncFailure(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = controlPlaneNode()

	syncer := &recordingSyncer{err: errors.New("the workload cluster is unreachable")}
	reader := &syncReader{
		machines:   []clusterv1.Machine{machine("control-plane-1", "v1.34.0")},
		kubeconfig: []byte("kubeconfig-of-test-cluster"),
	}

	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	handler.manifestSyncer = syncer

	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"),
		&runtimehooksv1.CanUpdateMachineResponse{},
	)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		handler.UpdateMachine(request, &runtimehooksv1.UpdateMachineResponse{})
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusFailure))
	g.Expect(resp.Message).To(ContainSubstring("the workload cluster is unreachable"))
}

// TestUpdateMachineCompletesWhileTheRestOfTheControlPlaneIsStillAtTheOldVersion covers the
// rollout rule of the trigger: it rolls in-place updates out one Machine at a time and
// waits for the Machine that is in progress before triggering the next one. A control-plane
// Machine whose own components are at the target version but whose peer is still at the old
// one must complete, not stay in progress, because holding it open would stop the peer from
// ever being triggered and the control plane would never reach the version the manifests are
// synced for.
func TestUpdateMachineCompletesWhileTheRestOfTheControlPlaneIsStillAtTheOldVersion(t *testing.T) {
	g := NewWithT(t)

	applier := newFakeApplier()
	applier.node = controlPlaneNode()
	applier.nodes = map[string]NodeStatus{"10.0.0.2": controlPlaneNode()}

	syncer := &recordingSyncer{}
	reader := &syncReader{
		machines: []clusterv1.Machine{
			machine("control-plane-1", "v1.34.0"),
			machine("control-plane-2", "v1.34.0"),
		},
		addresses: map[string][]clusterv1.MachineAddress{
			"control-plane-2": {{Type: clusterv1.MachineAddressType("InternalIP"), Address: "10.0.0.2"}},
		},
		kubeconfig: []byte("kubeconfig-of-test-cluster"),
	}

	handler := NewHandlerWithMetadata(applier, logr.Discard(), "", 0, reader)
	handler.manifestSyncer = syncer

	handler.CanUpdateMachine(
		canUpdateRequest(talosConfig("talosconfig-1", baseConfig), talosConfig("talosconfig-1", changedConfig), "talos-image", "v1.33.1", "v1.34.0"),
		&runtimehooksv1.CanUpdateMachineResponse{},
	)

	request := &runtimehooksv1.UpdateMachineRequest{
		CommonRequest: runtimehooksv1.CommonRequest{Settings: map[string]string{SettingAddress: "10.0.0.1"}},
		Desired: runtimehooksv1.UpdateMachineRequestObjects{
			Machine:               machine("control-plane-1", "v1.34.0"),
			InfrastructureMachine: infraMachine("talos-image"),
			BootstrapConfig:       talosConfig("talosconfig-1", changedConfig),
		},
	}

	for range kubernetesComponents {
		handler.UpdateMachine(request, &runtimehooksv1.UpdateMachineResponse{})
	}

	resp := &runtimehooksv1.UpdateMachineResponse{}
	handler.UpdateMachine(request, resp)

	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.RetryAfterSeconds).To(BeZero())
	g.Expect(syncer.calls).To(BeZero())
}

// TestSupportsSSAManifestSync mirrors the version gate of talosctl upgrade-k8s: Talos
// v1.13 and newer sync manifests with a server-side-apply inventory, Talos v1.12 uses the
// legacy sync.
func TestSupportsSSAManifestSync(t *testing.T) {
	g := NewWithT(t)

	g.Expect(supportsSSAManifestSync("")).To(BeTrue(), "an empty TalosVersion means the latest release")
	g.Expect(supportsSSAManifestSync("v1.14.0")).To(BeTrue())
	g.Expect(supportsSSAManifestSync("v1.13.0")).To(BeTrue())
	g.Expect(supportsSSAManifestSync("v1.12.1")).To(BeFalse())
	g.Expect(supportsSSAManifestSync("v1.11.0")).To(BeFalse())
}

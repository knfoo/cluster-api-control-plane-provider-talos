// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The addresses the manager hooks read from the Machine status to reach a node's Talos API.
package inplaceupdate

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	cabptv1beta1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1beta1"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// metadataClient is the fake client of these tests. Like the manager's client it has the
// Cluster API and core Kubernetes types registered, which the reader's typed reads need.
func metadataClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// TestMachineAddressesReadsTheAddressObjectsOfAMachine checks the shape Cluster API writes:
// Machine status.addresses is a list of MachineAddress objects with a type and an address
// (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineAddresses), copied from the infrastructure
// provider, not a list of strings.
func TestMachineAddressesReadsTheAddressObjectsOfAMachine(t *testing.T) {
	g := NewWithT(t)

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "default"},
		Status: clusterv1.MachineStatus{
			Addresses: clusterv1.MachineAddresses{
				{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
				{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
			},
		},
	}

	addresses, err := NewKubeMetadataReader(metadataClient(machine)).MachineAddresses(context.Background(), "default", "worker-1")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(Equal([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
	}))
}

// TestMachineAddressesOfAMachineWithoutAddresses checks that a Machine that does not exist
// yet, or has no addresses in its status, is answered with no addresses and no error, so the
// hook can report the missing address as a machine-specific failure.
func TestMachineAddressesOfAMachineWithoutAddresses(t *testing.T) {
	g := NewWithT(t)

	machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-2", Namespace: "default"}}

	reader := NewKubeMetadataReader(metadataClient(machine))

	addresses, err := reader.MachineAddresses(context.Background(), "default", "worker-2")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(BeEmpty())

	addresses, err = reader.MachineAddresses(context.Background(), "default", "worker-that-does-not-exist")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(BeEmpty())
}

// TestPickMachineAddressPrefersTheInternalAddress checks which of the reported addresses a
// hook addresses the Talos API at: Talos serves its API on the node's own network, so the
// internal address comes before an external one, and any address is used when the provider
// reports no internal one.
func TestPickMachineAddressPrefersTheInternalAddress(t *testing.T) {
	g := NewWithT(t)

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
	})).To(Equal("10.0.0.5"))

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalDNS, Address: "worker.example.com"},
		{Type: clusterv1.MachineInternalDNS, Address: "worker.internal"},
	})).To(Equal("worker.internal"))

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
	})).To(Equal("203.0.113.10"))

	g.Expect(pickMachineAddress(nil)).To(BeEmpty())
}

// TestPickMachineAddressSkipsLinkLocalAddresses checks the rule the provider applies to every
// Talos API endpoint (pkg/inplaceupdate TalosEndpoint, used by controllers/configs.go too): a
// link-local address is only reachable on the node's own link and an IPv6 link-local address
// carries a zone the Talos client's gRPC target cannot take, so a hook never uses one, even
// when Metal3 reports it as the only internal address of the Machine.
func TestPickMachineAddressSkipsLinkLocalAddresses(t *testing.T) {
	g := NewWithT(t)

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: "fe80::1%eth0"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
	})).To(Equal("10.0.0.5"))

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: "169.254.1.1"},
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
	})).To(Equal("203.0.113.10"))

	g.Expect(pickMachineAddress([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: "169.254.1.1"},
	})).To(BeEmpty())
}

// TestMetadataSecretResolvesThroughTheBootstrapProvider checks the hooks resolve the metadata secret
// with the bootstrap provider's exported pkg/machineconfig.MetadataSecret: the Metal3/CAPM3 defaults
// (suffix "-metadata", key "metaData") apply, and metadataFrom naming both sources is refused.
func TestMetadataSecretResolvesThroughTheBootstrapProvider(t *testing.T) {
	g := NewWithT(t)
	machine := clusterv1.Machine{Spec: clusterv1.MachineSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{Name: "worker-abc123"}}}

	name, key, err := metadataSecret(cabptv1beta1.TalosConfigSpec{MetadataFrom: &cabptv1beta1.MetadataFrom{InfrastructureMachine: &cabptv1beta1.InfrastructureMachineMetadata{}}}, machine)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(Equal("worker-abc123" + machineconfig.MetadataSecretSuffix))
	g.Expect(key).To(Equal(machineconfig.MetadataDataKey))

	_, _, err = metadataSecret(cabptv1beta1.TalosConfigSpec{MetadataFrom: &cabptv1beta1.MetadataFrom{
		SecretRef:             &cabptv1beta1.MetadataSecretRef{Name: "metal3-data-0"},
		InfrastructureMachine: &cabptv1beta1.InfrastructureMachineMetadata{},
	}}, machine)
	g.Expect(err).To(HaveOccurred())
}

// TestMachineSetMachinesReadsTheMachinesOfAMachineSet checks the capability CanUpdateMachineSet
// uses to find a node it can DryRun the machine configuration of a rollout against: the
// MachineSet controller labels every Machine it creates with the name of the machine set
// (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineSetNameLabel), and Machines of another
// machine set are not returned.
func TestMachineSetMachinesReadsTheMachinesOfAMachineSet(t *testing.T) {
	g := NewWithT(t)

	worker := func(name, machineSet string) *clusterv1.Machine {
		return &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:    "test-cluster",
					clusterv1.MachineSetNameLabel: machineSet,
				},
			},
		}
	}

	reader := NewKubeMetadataReader(metadataClient(
		worker("worker-1", "workers-abc123"),
		worker("worker-2", "workers-abc123"),
		worker("worker-3", "workers-def456"),
	))

	machines, err := reader.(MachineSetMachineReader).MachineSetMachines(context.Background(), "default", "workers-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(machines).To(HaveLen(2))
	g.Expect(machines[0].Name).To(Equal("worker-1"))
	g.Expect(machines[1].Name).To(Equal("worker-2"))

	machines, err = reader.(MachineSetMachineReader).MachineSetMachines(context.Background(), "default", "workers-that-do-not-exist")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(machines).To(BeEmpty())
}

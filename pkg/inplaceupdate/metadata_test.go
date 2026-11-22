// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The addresses the extension reads from the Machine status to reach a node's Talos API.
package inplaceupdate

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestMachineAddressesReadsTheAddressObjectsOfAMachine checks the shape Cluster API writes:
// Machine status.addresses is a list of MachineAddress objects with a type and an address
// (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineAddresses), copied from the infrastructure
// provider, not a list of strings.
func TestMachineAddressesReadsTheAddressObjectsOfAMachine(t *testing.T) {
	g := NewWithT(t)

	machine := &unstructured.Unstructured{}

	machine.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("Machine"))
	machine.SetName("worker-1")
	machine.SetNamespace("default")
	machine.Object["status"] = map[string]any{
		"addresses": []any{
			map[string]any{"type": "ExternalIP", "address": "203.0.113.10"},
			map[string]any{"type": "InternalIP", "address": "10.0.0.5"},
		},
	}

	addresses, err := NewKubeMetadataReader(fake.NewClientBuilder().WithObjects(machine).Build()).(MachineAddressReader).MachineAddresses(context.Background(), "default", "worker-1")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(Equal([]clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
	}))
}

// TestMachineAddressesOfAMachineWithoutAddresses checks that a Machine that does not exist
// yet, or has no addresses in its status, is answered with no addresses and no error, so the
// extension falls back to the settings instead of failing the hook.
func TestMachineAddressesOfAMachineWithoutAddresses(t *testing.T) {
	g := NewWithT(t)

	machine := &unstructured.Unstructured{}

	machine.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("Machine"))
	machine.SetName("worker-2")
	machine.SetNamespace("default")

	reader := NewKubeMetadataReader(fake.NewClientBuilder().WithObjects(machine).Build())

	addresses, err := reader.(MachineAddressReader).MachineAddresses(context.Background(), "default", "worker-2")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(BeEmpty())

	addresses, err = reader.(MachineAddressReader).MachineAddresses(context.Background(), "default", "worker-that-does-not-exist")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(addresses).To(BeEmpty())
}

// TestPickMachineAddressPrefersTheInternalAddress checks which of the reported addresses the
// extension addresses the Talos API at: Talos serves its API on the node's own network, so
// the internal address comes before an external one, and any address is used when the
// provider reports no internal one.
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

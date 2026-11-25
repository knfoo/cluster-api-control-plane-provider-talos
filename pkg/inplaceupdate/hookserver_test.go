// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimeserver "sigs.k8s.io/cluster-api/exp/runtime/server"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestManagerServesTheInPlaceUpdateHooks registers the three in-place update hooks with
// Cluster API's Runtime SDK server exactly the way main.go does, so a handler whose
// signature or name the server rejects fails here instead of at cluster creation time.
// The server answers the Discovery hook itself from the handlers registered with it
// (sigs.k8s.io/cluster-api/exp/runtime/server Server.Start), so the hooks of the manager
// are discoverable without an extension binary.
func TestManagerServesTheInPlaceUpdateHooks(t *testing.T) {
	g := NewWithT(t)

	catalog := runtimecatalog.New()
	g.Expect(runtimehooksv1.AddToCatalog(catalog)).To(Succeed())

	server, err := runtimeserver.New(runtimeserver.Options{
		Port:    9444,
		Catalog: catalog,
	})
	g.Expect(err).NotTo(HaveOccurred())

	handler := NewHandler(fake.NewClientBuilder().Build(), logr.Discard())

	for _, extensionHandler := range []runtimeserver.ExtensionHandler{
		{Hook: runtimehooksv1.CanUpdateMachine, Name: "talos-can-update-machine", HandlerFunc: handler.CanUpdateMachine},
		{Hook: runtimehooksv1.CanUpdateMachineSet, Name: "talos-can-update-machine-set", HandlerFunc: handler.CanUpdateMachineSet},
		{Hook: runtimehooksv1.UpdateMachine, Name: "talos-update-machine", HandlerFunc: handler.UpdateMachine},
	} {
		g.Expect(server.AddExtensionHandler(extensionHandler)).To(Succeed())
	}
}

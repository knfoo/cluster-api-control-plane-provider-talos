// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimeserver "sigs.k8s.io/cluster-api/exp/runtime/server"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// hookPortNumber is the port the manager serves the in-place update hooks on, matching the
// default of the --runtime-extension-port flag in main.go.
const hookPortNumber = 9444

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

// TestHookServiceManifestPortNames checks the manifests that expose the in-place update
// hooks. The apiserver validates every port name as an IANA service name of at most 15
// characters (k8s.io/apimachinery/pkg/util/validation.IsValidPortName, which also applies
// to a named targetPort and to a container port name), so clusterctl refuses a Service or
// a manager pod whose hook port is called runtime-extension, and the provider is never
// installed.
func TestHookServiceManifestPortNames(t *testing.T) {
	g := NewWithT(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", "service.yaml"))
	g.Expect(err).NotTo(HaveOccurred())

	var service corev1.Service
	g.Expect(yaml.Unmarshal(raw, &service)).To(Succeed())

	hookPort := int32(0)
	for _, port := range service.Spec.Ports {
		g.Expect(validation.IsValidPortName(port.Name)).To(BeEmpty(), "Service port name %q", port.Name)
		if port.TargetPort.Type == intstr.String {
			g.Expect(validation.IsValidPortName(port.TargetPort.StrVal)).To(BeEmpty(), "Service targetPort %q", port.TargetPort.StrVal)
		}
		if port.Port == hookPortNumber {
			hookPort = port.Port
		}
	}
	// The ExtensionConfig and the in-place e2e reach the hooks on this port of the Service.
	g.Expect(int(hookPort)).To(Equal(hookPortNumber), "the webhook Service exposes the hooks on port %d", hookPortNumber)

	raw, err = os.ReadFile(filepath.Join("..", "..", "config", "default", "manager_webhook_patch.yaml"))
	g.Expect(err).NotTo(HaveOccurred())

	var patch appsv1.Deployment
	g.Expect(yaml.Unmarshal(raw, &patch)).To(Succeed())

	served := false
	for _, container := range patch.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			g.Expect(validation.IsValidPortName(port.Name)).To(BeEmpty(), "container port name %q", port.Name)
			if port.ContainerPort == hookPortNumber {
				served = true
			}
		}
	}
	g.Expect(served).To(BeTrue(), "the manager serves the hooks on port %d", hookPortNumber)
}

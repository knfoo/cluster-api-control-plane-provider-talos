// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"context"
	"fmt"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	. "github.com/onsi/gomega"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	"google.golang.org/grpc/metadata"
)

// nodeState stands in for the COSI state of one Talos node. It answers the reads of
// nodeStatusOf with the resources a real Talos v1.14 node reports, records the metadata
// of every read, and can be made to fail.
type nodeState struct {
	resources map[string]resource.Resource
	fail      error
	reads     []nodeRead
}

type nodeRead struct {
	pointer resource.Pointer
	node    []string
	nodes   []string
}

func newNodeState(resources ...resource.Resource) *nodeState {
	state := &nodeState{resources: map[string]resource.Resource{}}

	for _, res := range resources {
		state.resources[resourceKey(res.Metadata())] = res
	}

	return state
}

func resourceKey(pointer resource.Pointer) string {
	return pointer.Namespace() + "/" + pointer.Type() + "/" + pointer.ID()
}

func (s *nodeState) Get(ctx context.Context, pointer resource.Pointer, options ...state.GetOption) (resource.Resource, error) {
	read := nodeRead{pointer: pointer}

	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		read.node = md.Get("node")
		read.nodes = md.Get("nodes")
	}

	s.reads = append(s.reads, read)

	if s.fail != nil && (pointer.Type() == k8s.APIServerConfigType || pointer.Type() == k8s.ControllerManagerConfigType || pointer.Type() == k8s.SchedulerConfigType) {
		return nil, s.fail
	}

	res, ok := s.resources[resourceKey(pointer)]
	if !ok {
		return nil, notFoundError{pointer: pointer}
	}

	return res, nil
}

func (s *nodeState) List(ctx context.Context, kind resource.Kind, options ...state.ListOption) (resource.List, error) {
	return resource.List{}, nil
}

// notFoundError implements state.ErrNotFound, the way the COSI runtime reports a resource
// that does not exist.
type notFoundError struct {
	pointer resource.Pointer
}

func (e notFoundError) Error() string {
	return fmt.Sprintf("resource %s/%s/%s not found", e.pointer.Namespace(), e.pointer.Type(), e.pointer.ID())
}

func (e notFoundError) NotFoundError() {}

// controlPlaneResources are the resources a Talos v1.14 control-plane node reports for its
// Kubernetes components: the final- configurations of the static pods, the bootstrap
// manifests and the kubelet specification.
func controlPlaneResources(image string) []resource.Resource {
	apiserver := k8s.NewAPIServerConfig(k8s.FinalAPIServerConfigID)
	apiserver.TypedSpec().Image = "registry.k8s.io/kube-apiserver:" + image
	apiserver.TypedSpec().EtcdServers = []string{"https://10.67.244.22:2379"}

	controllerManager := k8s.NewControllerManagerConfig(k8s.FinalControllerManagerConfigID)
	controllerManager.TypedSpec().Image = "registry.k8s.io/kube-controller-manager:" + image

	scheduler := k8s.NewSchedulerConfig(k8s.FinalSchedulerConfigID)
	scheduler.TypedSpec().Image = "registry.k8s.io/kube-scheduler:" + image

	manifests := k8s.NewBootstrapManifestsConfig()
	manifests.TypedSpec().ProxyImage = "registry.k8s.io/kube-proxy:" + image

	kubelet := k8s.NewKubeletSpec(k8s.NamespaceName, k8s.KubeletID)
	kubelet.TypedSpec().Image = "ghcr.io/siderolabs/kubelet:" + image + "-fat"

	return []resource.Resource{apiserver, controllerManager, scheduler, manifests, kubelet}
}

func TestNodeStatusReportsTheControlPlaneOfATalosNode(t *testing.T) {
	g := NewWithT(t)

	node := newNodeState(controlPlaneResources("v1.36.6")...)
	reader := state.CoreStateReader(node)

	status, err := nodeStatusOf(context.Background(), reader, "10.67.244.22")
	g.Expect(err).To(BeNil())
	g.Expect(status.APIServer).To(Equal("registry.k8s.io/kube-apiserver:v1.36.6"))
	g.Expect(status.ControllerManager).To(Equal("registry.k8s.io/kube-controller-manager:v1.36.6"))
	g.Expect(status.Scheduler).To(Equal("registry.k8s.io/kube-scheduler:v1.36.6"))
	g.Expect(status.Proxy).To(Equal("registry.k8s.io/kube-proxy:v1.36.6"))
	g.Expect(status.Kubelet).To(Equal("ghcr.io/siderolabs/kubelet:v1.36.6-fat"))
	g.Expect(status.Etcd).To(ContainElement("https://10.67.244.22:2379"))
	g.Expect(status.hasControlPlane()).To(BeTrue())
}

// Every read has to be addressed to the one node of the machine. WithNodes is deprecated
// and lets apid aggregate the answers of all endpoints, so a read through it is not a read
// of that node's resources.
func TestNodeStatusReadsAreAddressedToTheNode(t *testing.T) {
	g := NewWithT(t)

	node := newNodeState(controlPlaneResources("v1.36.6")...)

	_, err := nodeStatusOf(context.Background(), state.CoreStateReader(node), "10.67.244.22")
	g.Expect(err).To(BeNil())
	g.Expect(node.reads).NotTo(BeEmpty())

	for _, read := range node.reads {
		g.Expect(read.node).To(ContainElement("10.67.244.22"))
		g.Expect(read.nodes).To(BeEmpty())
	}
}

// A node whose component configuration is not rendered yet (the plain resource exists, the
// final- one does not) is still reported.
func TestNodeStatusFallsBackToThePlainComponentConfig(t *testing.T) {
	g := NewWithT(t)

	apiserver := k8s.NewAPIServerConfig(k8s.APIServerConfigID)
	apiserver.TypedSpec().Image = "registry.k8s.io/kube-apiserver:v1.36.6"
	apiserver.TypedSpec().EtcdServers = []string{"https://10.67.244.22:2379"}

	node := newNodeState(apiserver)

	reader := state.CoreStateReader(node)
	status, err := nodeStatusOf(context.Background(), reader, "10.67.244.22")
	g.Expect(err).To(BeNil())
	g.Expect(status.APIServer).To(Equal("registry.k8s.io/kube-apiserver:v1.36.6"))
	g.Expect(status.hasControlPlane()).To(BeTrue())
}

// A worker Machine reports no Kubernetes component resources at all, which is not an error:
// the extension leaves it to a rollout.
func TestNodeStatusOfANodeWithoutKubernetesComponents(t *testing.T) {
	g := NewWithT(t)

	node := newNodeState()

	reader := state.CoreStateReader(node)
	status, err := nodeStatusOf(context.Background(), reader, "10.67.244.22")
	g.Expect(err).To(BeNil())
	g.Expect(status.hasKubernetes()).To(BeFalse())
	g.Expect(status.hasControlPlane()).To(BeFalse())
}

// A read that fails for a reason other than "the resource does not exist" must not be
// swallowed into an empty NodeStatus: it makes a control-plane node look like a worker, and
// then the staged Kubernetes update never starts.
func TestNodeStatusDoesNotSwallowReadErrors(t *testing.T) {
	g := NewWithT(t)

	node := newNodeState(controlPlaneResources("v1.36.6")...)
	node.fail = fmt.Errorf("connection reset while reading the node state")

	reader := state.CoreStateReader(node)
	status, err := nodeStatusOf(context.Background(), reader, "10.67.244.22")
	g.Expect(err).NotTo(BeNil())
	g.Expect(err.Error()).To(ContainSubstring("10.67.244.22"))
	g.Expect(status).To(Equal(NodeStatus{}))
}

func TestNodeStatusRequiresAnEndpoint(t *testing.T) {
	g := NewWithT(t)

	_, err := NewTalosApplier(nil).NodeStatus(context.Background(), "")
	g.Expect(err).NotTo(BeNil())
}

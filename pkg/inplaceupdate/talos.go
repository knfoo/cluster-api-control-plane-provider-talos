// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Talking to the Talos API of one machine. An applier is built per hook request from the
// talosconfig of the machine's cluster, so no applier outlives a request.
package inplaceupdate

import (
	"context"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/pkg/errors"
	"github.com/siderolabs/go-kubernetes/kubernetes/manifests"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NodeStatus reports the Kubernetes components a node runs, read from the COSI resources
// of the node. An empty image means the node does not report that component, which is not
// an error: a node that reports no Kubernetes component at all is not upgraded through the
// staged Kubernetes path. Etcd is the etcd the node's kube-apiserver configuration points
// at, which a control-plane node always reports and a worker Machine never has.
type NodeStatus struct {
	APIServer         string
	ControllerManager string
	Scheduler         string
	Proxy             string
	Kubelet           string
	Etcd              []string
}

// hasKubernetes reports whether the node runs any Kubernetes component.
func (s NodeStatus) hasKubernetes() bool {
	return s.APIServer != "" || s.ControllerManager != "" || s.Scheduler != "" || s.Proxy != "" || s.Kubelet != ""
}

// hasControlPlane reports whether the node runs control-plane components. A node with
// only a kubelet (a worker Machine) has no control-plane component configuration
// resources and no etcd, so the staged Kubernetes path can never report it complete.
func (s NodeStatus) hasControlPlane() bool {
	return s.APIServer != "" || s.ControllerManager != "" || s.Scheduler != "" || len(s.Etcd) > 0
}

// applyConfigurationRequest builds the Talos API request for a machine
// configuration. The owner's condition for in-place updates (ADR-0003) is that
// Talos must accept the change with mode ApplyConfigurationRequest_NO_REBOOT after
// a DryRun, so every request the hooks send is a NO_REBOOT request and the
// DryRun flag distinguishes the check from the real apply.
func applyConfigurationRequest(data []byte, dryRun bool) *machineapi.ApplyConfigurationRequest {
	return &machineapi.ApplyConfigurationRequest{
		Data:   data,
		Mode:   machineapi.ApplyConfigurationRequest_NO_REBOOT,
		DryRun: dryRun,
	}
}

// ApplyStatus is what a node reports about a configuration change.
type ApplyStatus struct {
	// NoChanges is true when the node already runs the configuration.
	NoChanges bool
	// RebootRequired is true when the node cannot apply the configuration with
	// mode NO_REBOOT.
	RebootRequired bool
	// Details is Talos' human-readable explanation of the mode it chose.
	Details string
}

// Applier talks to the Talos API of a single machine.
type Applier interface {
	// DryRun asks the node, without applying anything, whether it accepts the
	// configuration with mode NO_REBOOT.
	DryRun(ctx context.Context, endpoint string, config []byte) (ApplyStatus, error)
	// Apply sends the configuration to the node with mode NO_REBOOT.
	Apply(ctx context.Context, endpoint string, config []byte) error
	// NodeStatus reports the Kubernetes components the node runs, so the hooks can
	// tell whether a machine has to be upgraded component by component and how far the
	// upgrade has come.
	NodeStatus(ctx context.Context, endpoint string) (NodeStatus, error)
	// BootstrapManifests returns the bootstrap manifests (kube-proxy, CoreDNS) the node
	// has for its Kubernetes cluster, as Talos stores them in its COSI state. Syncing them
	// to the workload cluster is the last step of the Kubernetes version update, the one
	// talosctl upgrade-k8s performs with PerformManifestsSync.
	BootstrapManifests(ctx context.Context, endpoint string) ([]*unstructured.Unstructured, error)
}

// TalosApplier talks to real Talos nodes. The node itself reports whether it already runs
// the configuration, so the applier keeps no state between hook requests: a repeated
// UpdateMachine call for the same desired configuration is reported as finished by the
// DryRun of the node.
type TalosApplier struct {
	config *talosconfig.Config
}

// NewTalosApplier creates an applier for the talosconfig of one cluster.
func NewTalosApplier(config *talosconfig.Config) *TalosApplier {
	return &TalosApplier{
		config: config,
	}
}

func (a *TalosApplier) DryRun(ctx context.Context, endpoint string, config []byte) (ApplyStatus, error) {
	return a.request(ctx, endpoint, config, true)
}

func (a *TalosApplier) Apply(ctx context.Context, endpoint string, config []byte) error {
	status, err := a.request(ctx, endpoint, config, false)
	if err != nil {
		return err
	}

	if status.RebootRequired {
		return errors.Errorf("node %q needs a reboot to apply the configuration: %s", endpoint, status.Details)
	}

	return nil
}

// NodeStatus reads the images of the control-plane components from the COSI state of the
// node. The final- resources are the configurations Talos renders for the running pods, so
// they are preferred and the plain resources are the fallback, mirroring the watch of the
// control-plane component configurations in talosctl upgrade-k8s
// (watchControlPlaneConfigResource).
// A component the node does not report stays empty.
func (a *TalosApplier) NodeStatus(ctx context.Context, endpoint string) (NodeStatus, error) {
	if endpoint == "" {
		return NodeStatus{}, errors.New("no Talos endpoint for the machine")
	}

	client, err := talosclient.New(ctx,
		talosclient.WithDefaultGRPCDialOptions(),
		talosclient.WithConfig(a.config),
		talosclient.WithEndpoints(endpoint),
	)
	if err != nil {
		return NodeStatus{}, errors.Wrapf(err, "connecting to Talos node %q", endpoint)
	}

	defer client.Close()

	return nodeStatusOf(ctx, client.COSI, endpoint)
}

// BootstrapManifests reads the bootstrap manifests Talos keeps for the Kubernetes cluster,
// the kube-proxy DaemonSet and the CoreDNS deployment, with the images the staged machine
// configuration has given them. This is the manifest list talosctl upgrade-k8s feeds into
// PerformManifestsSync, read from the node's COSI state with manifests.GetBootstrapManifests.
func (a *TalosApplier) BootstrapManifests(ctx context.Context, endpoint string) ([]*unstructured.Unstructured, error) {
	if endpoint == "" {
		return nil, errors.New("no Talos endpoint for the machine")
	}

	client, err := talosclient.New(ctx,
		talosclient.WithDefaultGRPCDialOptions(),
		talosclient.WithConfig(a.config),
		talosclient.WithEndpoints(endpoint),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "connecting to Talos node %q", endpoint)
	}

	defer client.Close()

	// GetBootstrapManifests is an alias of unstructured objects, so the return type is the
	// slice the ManifestSyncer takes.
	return manifests.GetBootstrapManifests(talosclient.WithNode(ctx, endpoint), client.COSI, nil)
}

// nodeStatusOf reads one node's Kubernetes components from its COSI state. The reads are
// addressed to that single node with client.WithNode: client.WithNodes is deprecated
// ("Use WithNode and client-side multiplexing instead") and aggregates the answers of all
// nodes into one response, which is not a read of one node's resources.
func nodeStatusOf(ctx context.Context, reader state.CoreStateReader, endpoint string) (NodeStatus, error) {
	nodeCtx := talosclient.WithNode(ctx, endpoint)
	status := NodeStatus{}

	apiserver, err := componentConfig[*k8s.APIServerConfig](nodeCtx, reader, k8s.FinalAPIServerConfigID, k8s.APIServerConfigID)
	if err != nil {
		return NodeStatus{}, errors.Wrapf(err, "reading the kube-apiserver configuration of Talos node %q", endpoint)
	}

	if apiserver != nil {
		status.APIServer = apiserver.TypedSpec().Image
		status.Etcd = apiserver.TypedSpec().EtcdServers
	}

	controllerManager, err := componentConfig[*k8s.ControllerManagerConfig](nodeCtx, reader, k8s.FinalControllerManagerConfigID, k8s.ControllerManagerConfigID)
	if err != nil {
		return NodeStatus{}, errors.Wrapf(err, "reading the kube-controller-manager configuration of Talos node %q", endpoint)
	}

	if controllerManager != nil {
		status.ControllerManager = controllerManager.TypedSpec().Image
	}

	scheduler, err := componentConfig[*k8s.SchedulerConfig](nodeCtx, reader, k8s.FinalSchedulerConfigID, k8s.SchedulerConfigID)
	if err != nil {
		return NodeStatus{}, errors.Wrapf(err, "reading the kube-scheduler configuration of Talos node %q", endpoint)
	}

	if scheduler != nil {
		status.Scheduler = scheduler.TypedSpec().Image
	}

	manifests, err := safe.StateGetByID[*k8s.BootstrapManifestsConfig](nodeCtx, reader, k8s.BootstrapManifestsConfigID)
	if err != nil && !state.IsNotFoundError(err) {
		return NodeStatus{}, errors.Wrapf(err, "reading the bootstrap manifests of Talos node %q", endpoint)
	}

	if manifests != nil {
		status.Proxy = manifests.TypedSpec().ProxyImage
	}

	kubelet, err := safe.StateGetByID[*k8s.KubeletSpec](nodeCtx, reader, k8s.KubeletID)
	if err != nil && !state.IsNotFoundError(err) {
		return NodeStatus{}, errors.Wrapf(err, "reading the kubelet specification of Talos node %q", endpoint)
	}

	if kubelet != nil {
		status.Kubelet = kubelet.TypedSpec().Image
	}

	return status, nil
}

// componentConfig reads the COSI resource holding the configuration of one control-plane
// component of a node: the final- resource Talos renders for the running pod, falling back
// to the plain resource. No resource at all is not an error, it is a node that does not run
// that component. Every other read error is returned: a failed read must never be read as
// "this node runs no control plane", because that answers CanUpdateMachine with "the
// machine runs no control-plane components" and UpdateMachine with a plain configuration
// apply, so the staged Kubernetes update never starts.
func componentConfig[T meta.ResourceWithRD](ctx context.Context, reader state.CoreStateReader, finalID, plainID resource.ID) (T, error) {
	config, err := safe.StateGetByID[T](ctx, reader, finalID)
	if err == nil {
		return config, nil
	}

	if state.IsNotFoundError(err) {
		config, err = safe.StateGetByID[T](ctx, reader, plainID)
		if err != nil && !state.IsNotFoundError(err) {
			return config, err
		}

		return config, nil
	}

	return config, err
}

func (a *TalosApplier) request(ctx context.Context, endpoint string, config []byte, dryRun bool) (ApplyStatus, error) {
	if endpoint == "" {
		return ApplyStatus{}, errors.New("no Talos endpoint for the machine")
	}

	client, err := talosclient.New(ctx,
		talosclient.WithDefaultGRPCDialOptions(),
		talosclient.WithConfig(a.config),
		talosclient.WithEndpoints(endpoint),
	)
	if err != nil {
		return ApplyStatus{}, errors.Wrapf(err, "connecting to Talos node %q", endpoint)
	}

	defer client.Close()

	response, err := client.ApplyConfiguration(talosclient.WithNodes(ctx, endpoint), applyConfigurationRequest(config, dryRun))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "reboot") {
			return ApplyStatus{RebootRequired: true, Details: err.Error()}, nil
		}

		return ApplyStatus{}, errors.Wrapf(err, "applying configuration to Talos node %q", endpoint)
	}

	status := ApplyStatus{}

	for _, message := range response.GetMessages() {
		status.Details = message.ModeDetails

		if message.Mode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
			status.RebootRequired = true
		}

		if strings.Contains(strings.ToLower(message.ModeDetails), "no changes") {
			status.NoChanges = true
		}

		for _, warning := range message.Warnings {
			if strings.Contains(strings.ToLower(warning), "reboot") {
				status.RebootRequired = true
			}
		}
	}

	return status, nil
}

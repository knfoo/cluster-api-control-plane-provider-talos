// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Talking to the Talos API of one machine.
package inplaceupdate

import (
	"context"
	"strings"
	"sync"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/pkg/errors"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
)

// NodeStatus reports the Kubernetes components a node runs, read from the COSI resources
// of the node. An empty image means the node does not report that component, which is not
// an error: a node that reports no Kubernetes component at all is not upgraded through the
// staged Kubernetes path.
type NodeStatus struct {
	APIServer         string
	ControllerManager string
	Scheduler         string
	Proxy             string
	Kubelet           string
}

// hasKubernetes reports whether the node runs any Kubernetes component.
func (s NodeStatus) hasKubernetes() bool {
	return s.APIServer != "" || s.ControllerManager != "" || s.Scheduler != "" || s.Proxy != "" || s.Kubelet != ""
}

// hasControlPlane reports whether the node runs control-plane components. A node with
// only a kubelet (a worker Machine) has no control-plane component configuration
// resources, so the staged Kubernetes path can never report it complete.
func (s NodeStatus) hasControlPlane() bool {
	return s.APIServer != "" || s.ControllerManager != "" || s.Scheduler != ""
}

// applyConfigurationRequest builds the Talos API request for a machine
// configuration. The owner's condition for in-place updates (ADR-0003) is that
// Talos must accept the change with mode ApplyConfigurationRequest_NO_REBOOT after
// a DryRun, so every request this extension sends is a NO_REBOOT request and the
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
	// NodeStatus reports the Kubernetes components the node runs, so the extension can
	// tell whether a machine has to be upgraded component by component and how far the
	// upgrade has come.
	NodeStatus(ctx context.Context, endpoint string) (NodeStatus, error)
}

// TalosApplier talks to real Talos nodes.
type TalosApplier struct {
	config *talosconfig.Config

	// applied holds the exact configuration last successfully applied to an
	// endpoint, so a repeated UpdateMachine call for the same desired
	// configuration is reported as finished instead of applied twice.
	mu      sync.Mutex
	applied map[string]string
}

// NewTalosApplier creates an applier for the given talosconfig.
func NewTalosApplier(config *talosconfig.Config) *TalosApplier {
	return &TalosApplier{
		config:  config,
		applied: map[string]string{},
	}
}

func (a *TalosApplier) DryRun(ctx context.Context, endpoint string, config []byte) (ApplyStatus, error) {
	status, err := a.request(ctx, endpoint, config, true)
	if err != nil {
		return ApplyStatus{}, err
	}

	a.mu.Lock()
	if a.applied[endpoint] == string(config) {
		status.NoChanges = true
	}
	a.mu.Unlock()

	return status, nil
}

func (a *TalosApplier) Apply(ctx context.Context, endpoint string, config []byte) error {
	status, err := a.request(ctx, endpoint, config, false)
	if err != nil {
		return err
	}

	if status.RebootRequired {
		return errors.Errorf("node %q needs a reboot to apply the configuration: %s", endpoint, status.Details)
	}

	a.mu.Lock()
	a.applied[endpoint] = string(config)
	a.mu.Unlock()

	return nil
}

// NodeStatus reads the images of the control-plane components from the COSI state of the
// node. The final- resources are the configurations Talos renders for the running pods, so
// they are preferred and the plain resources are the fallback, mirroring the watch of the
// control-plane component configurations in .lap/sources/reference/files/talos_managed.go
// (watchControlPlaneConfigResource). A component the node does not report stays empty.
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

	nodeCtx := talosclient.WithNodes(ctx, endpoint)
	status := NodeStatus{}

	if apiserver, err := safe.StateGetByID[*k8s.APIServerConfig](nodeCtx, client.COSI, k8s.FinalAPIServerConfigID); err == nil {
		status.APIServer = apiserver.TypedSpec().Image
	} else if apiserver, err := safe.StateGetByID[*k8s.APIServerConfig](nodeCtx, client.COSI, k8s.APIServerConfigID); err == nil {
		status.APIServer = apiserver.TypedSpec().Image
	}

	if controllerManager, err := safe.StateGetByID[*k8s.ControllerManagerConfig](nodeCtx, client.COSI, k8s.FinalControllerManagerConfigID); err == nil {
		status.ControllerManager = controllerManager.TypedSpec().Image
	} else if controllerManager, err := safe.StateGetByID[*k8s.ControllerManagerConfig](nodeCtx, client.COSI, k8s.ControllerManagerConfigID); err == nil {
		status.ControllerManager = controllerManager.TypedSpec().Image
	}

	if scheduler, err := safe.StateGetByID[*k8s.SchedulerConfig](nodeCtx, client.COSI, k8s.FinalSchedulerConfigID); err == nil {
		status.Scheduler = scheduler.TypedSpec().Image
	} else if scheduler, err := safe.StateGetByID[*k8s.SchedulerConfig](nodeCtx, client.COSI, k8s.SchedulerConfigID); err == nil {
		status.Scheduler = scheduler.TypedSpec().Image
	}

	if manifests, err := safe.StateGetByID[*k8s.BootstrapManifestsConfig](nodeCtx, client.COSI, k8s.BootstrapManifestsConfigID); err == nil {
		status.Proxy = manifests.TypedSpec().ProxyImage
	}

	if kubelet, err := safe.StateGetByID[*k8s.KubeletSpec](nodeCtx, client.COSI, k8s.KubeletID); err == nil {
		status.Kubelet = kubelet.TypedSpec().Image
	}

	return status, nil
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

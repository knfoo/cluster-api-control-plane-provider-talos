// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Talking to the Talos API of one machine.
package inplaceupdate

import (
	"context"
	"strings"
	"sync"

	"github.com/pkg/errors"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

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

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The Cluster API Runtime SDK extension that updates Talos machines in place.
//
// It answers CanUpdateMachine and UpdateMachine for a single Machine (proposal
// docs/proposals/in-place-updates.md, section 6 rows L4 and L5) and CanUpdateMachineSet for
// the worker Machines of a MachineDeployment (pkg/inplaceupdate/machineset.go). The owner's
// condition (docs/adr/ADR-0003, amended 2026-11-15) is that a TalosConfig change is handled
// in place only when it is a machine configuration change that Talos accepts with
// ApplyConfiguration mode ApplyConfigurationRequest_NO_REBOOT after a DryRun. A Kubernetes
// version change is handled in place as well, one component at a time the way talosctl
// upgrade-k8s does it (pkg/inplaceupdate/kubernetes.go): the control-plane components of a
// control-plane Machine, the kubelet of a worker Machine. Everything else - a Talos version
// change, a Kubernetes version change that is not the next minor version, an infrastructure
// change, or a change Talos says needs a reboot - gets no patch, so Cluster API rolls the
// machine out.
package inplaceupdate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

const (
	// DefaultRetryAfterSeconds is how long UpdateMachine asks Cluster API to wait
	// before it looks at the machine again after applying a configuration.
	DefaultRetryAfterSeconds = 10

	// DefaultRequestTimeout bounds a single call to the Talos API.
	DefaultRequestTimeout = 30 * time.Second

	// SettingAddress is the ExtensionConfig setting that names the Talos API address
	// of the machine being updated.
	SettingAddress = "address"
)

// rebootRequiredConfigPaths are machine configuration keys Talos can only change by
// booting the node again. They are a cheap guard in front of the DryRun: the DryRun
// is the authoritative answer, but these changes never need to reach a node.
var rebootRequiredConfigPaths = []string{
	"machine/install",
	"machine/boot",
	"machine/swap",
	"cluster/id",
	"cluster/secret",
	"cluster/token",
}

// Handler answers the in-place update hooks for Talos machines.
type Handler struct {
	applier         Applier
	log             logr.Logger
	timeout         time.Duration
	defaultEndpoint string
	metadata        MetadataReader
}

// NewHandler creates the extension handlers. endpoint is the fallback Talos address
// used when the request carries none. Without a metadata reader the extension renders
// only the TalosConfigs that carry their machine configuration in spec.data.
func NewHandler(applier Applier, log logr.Logger, endpoint string, timeout time.Duration) *Handler {
	return NewHandlerWithMetadata(applier, log, endpoint, timeout, nil)
}

// NewHandlerWithMetadata creates the extension handlers with read access to the
// management cluster objects needed to render a generated machine configuration and a
// metadata-driven template: the Cluster of the machine, the cluster's Talos secrets
// bundle and the metadata secrets a TalosConfig's metadataFrom points at.
func NewHandlerWithMetadata(applier Applier, log logr.Logger, endpoint string, timeout time.Duration, metadata MetadataReader) *Handler {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	return &Handler{
		applier:         applier,
		log:             log,
		timeout:         timeout,
		defaultEndpoint: endpoint,
		metadata:        metadata,
	}
}

// CanUpdateMachine is the CanUpdateMachine hook. It returns a patch for the
// TalosConfig object only when the whole change is a machine configuration change
// that Talos applies with mode NO_REBOOT after a DryRun.
func (h *Handler) CanUpdateMachine(req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	machine := req.Current.Machine.Name

	if reason := rolloutReason(req); reason != "" {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", reason)

		return
	}

	currentSpec, err := talosConfigSpec(req.Current.BootstrapConfig)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	desiredSpec, err := talosConfigSpec(req.Desired.BootstrapConfig)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if currentSpec.TalosVersion != desiredSpec.TalosVersion {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", "the Talos version changes")

		return
	}

	// The machine configuration of a TalosConfig is not the spec: it is the generated
	// or supplied base configuration with the configuration patches and the (template
	// rendered) strategic patches applied, exactly as the bootstrap provider builds it.
	// A configuration the extension cannot render - a generated configuration without
	// access to the cluster secrets, a template whose metadata is not available yet -
	// is answered with Success and no patch, which leaves current and desired different
	// and makes Cluster API roll the Machine out; answering with Failure would turn the
	// call into an error.
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	currentConfig, err := h.renderMachineConfig(ctx, currentSpec, req.Current.Machine)
	if err != nil {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", err.Error())

		return
	}

	desiredConfig, err := h.renderMachineConfig(ctx, desiredSpec, req.Desired.Machine)
	if err != nil {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", err.Error())

		return
	}

	ops, err := configDiff(currentConfig, desiredConfig)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if reason := rebootRequiredReason(ops); reason != "" {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", reason)

		return
	}

	endpoint, err := h.resolveEndpoint(req.Settings, req.Desired.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	// The owner's condition (docs/adr/ADR-0003 as amended on 2026-11-15): a Kubernetes
	// version change of a control-plane Machine is handled in place too, as the
	// proposal's kcp-version-update example does. The Machine patch carries spec.version,
	// and the machine configuration the node has to accept is the rendered desired
	// configuration with the control-plane component images patched step by step, so the
	// DryRun below covers both changes.
	versionUpgrade := req.Current.Machine.Spec.Version != req.Desired.Machine.Spec.Version

	config := desiredConfig

	if versionUpgrade {
		nodeStatus, err := h.applier.NodeStatus(ctx, endpoint)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}

		// A Kubernetes version change of a MachineSet-owned worker Machine is planned by the
		// MachineDeployment hook CanUpdateMachineSet and applied by UpdateMachine, which
		// upgrades the kubelet of the node. This hook is called by the control plane
		// controllers (docs .../implement-in-place-update-hooks.md: "This hook is called by
		// KCP when performing the 'can update in-place' for a control plane machine"), so a
		// node that runs no control-plane component gets no patch here and Cluster API keeps
		// the MachineSet's own rollout planning in charge of it.
		if !nodeStatus.hasControlPlane() {
			h.log.Info("machine cannot be updated in place", "machine", machine, "reason", "the machine runs no control-plane components")

			return
		}

		config, err = stagedKubernetesConfig(desiredConfig, nodeStatus, req.Desired.Machine.Spec.Version)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}
	}

	if len(ops) == 0 && !versionUpgrade {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", "the machine configuration does not change")

		return
	}

	// The owner's condition: only a change the node accepts with mode
	// ApplyConfigurationRequest_NO_REBOOT and DryRun is handled in place.
	status, err := h.applier.DryRun(ctx, endpoint, config)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if status.RebootRequired {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", "Talos needs a reboot to apply the machine configuration", "details", status.Details)

		return
	}

	if versionUpgrade {
		machinePatch, err := jsonPatch([]jsonPatchOp{{Op: "replace", Path: "/spec/version", Value: req.Desired.Machine.Spec.Version}})
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}

		resp.MachinePatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     machinePatch,
		}

		h.log.Info("machine Kubernetes version can be updated in place", "machine", machine, "endpoint", endpoint, "version", req.Desired.Machine.Spec.Version)
	}

	if len(ops) > 0 {
		patch, err := specPatch(req.Current.BootstrapConfig, req.Desired.BootstrapConfig)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}

		resp.BootstrapConfigPatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	h.log.Info("machine can be updated in place", "machine", machine, "endpoint", endpoint, "operations", len(ops))
}

// UpdateMachine is the UpdateMachine hook. It applies the machine configuration with
// mode NO_REBOOT and is idempotent: a second call with the same desired configuration
// reports the update as completed. When the node runs Kubernetes components, the
// configuration applied is the staged one of pkg/inplaceupdate/kubernetes.go, and the
// update is completed only when the node reports every Kubernetes component it runs -
// the control-plane components of a control-plane Machine, the kubelet of a worker
// Machine - at the Machine's version.
func (h *Handler) UpdateMachine(req *runtimehooksv1.UpdateMachineRequest, resp *runtimehooksv1.UpdateMachineResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	machine := req.Desired.Machine.Name

	spec, err := talosConfigSpec(req.Desired.BootstrapConfig)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	// CanUpdateMachine already rendered this configuration, so the same render is used
	// here; a failure is a real error rather than a reason to roll the machine out.
	config, err := h.renderMachineConfig(ctx, spec, req.Desired.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	endpoint, err := h.resolveEndpoint(req.Settings, req.Desired.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	nodeStatus, err := h.applier.NodeStatus(ctx, endpoint)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	// A node that runs Kubernetes components is upgraded one component at a time, in the
	// order of upgradeComponents (pkg/inplaceupdate/kubernetes.go): the control-plane
	// components of a control-plane Machine, the kubelet of a worker Machine, which is the
	// component talosctl upgrade-k8s patches on a node without control-plane components
	// (.lap/sources/reference/files/kubelet.go: upgradeKubeletOnNode). A node that runs no
	// Kubernetes component yet - a machine whose bootstrap has not started - gets the plain
	// machine configuration apply of stage L4b.
	target := req.Desired.Machine.Spec.Version
	staged := target != "" && nodeStatus.hasKubernetes()

	if staged {
		config, err = stagedKubernetesConfig(config, nodeStatus, target)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}
	}

	status, err := h.applier.DryRun(ctx, endpoint, config)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if status.NoChanges && (!staged || componentsAtTarget(nodeStatus, target)) {
		h.log.Info("machine configuration is already applied", "machine", machine, "endpoint", endpoint)
		resp.RetryAfterSeconds = 0

		return
	}

	if status.RebootRequired {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = fmt.Sprintf("Talos needs a reboot to apply the machine configuration of machine %q: %s", machine, status.Details)

		return
	}

	if err := h.applier.Apply(ctx, endpoint, config); err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	resp.RetryAfterSeconds = DefaultRetryAfterSeconds

	if staged {
		h.log.Info("machine configuration applied in place", "machine", machine, "endpoint", endpoint, "component", firstComponentNotAtTarget(nodeStatus, target))

		return
	}

	h.log.Info("machine configuration applied in place", "machine", machine, "endpoint", endpoint)
}

// Discovery is the Discovery hook, so the extension can be registered with an
// ExtensionConfig. The three in-place update hooks of the proposal are implemented:
// CanUpdateMachine and UpdateMachine for a Machine, CanUpdateMachineSet for a MachineSet.
// The APIVersion is the one the Runtime SDK catalog uses for the hook requests.
//
// Handler names must be unique inside an extension and valid DNS1123 labels: the Runtime
// SDK client rejects a discovery response with a repeated handler name
// (sigs.k8s.io/cluster-api/internal/runtime/client/client.go:774-786), so each hook gets
// its own name rather than the extension name.
func (h *Handler) Discovery(req *runtimehooksv1.DiscoveryRequest, resp *runtimehooksv1.DiscoveryResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.Handlers = []runtimehooksv1.ExtensionHandler{
		{
			Name:        "can-update-machine",
			RequestHook: runtimehooksv1.GroupVersionHook{APIVersion: runtimehooksv1.GroupVersion.String(), Hook: "CanUpdateMachine"},
		},
		{
			Name:        "can-update-machine-set",
			RequestHook: runtimehooksv1.GroupVersionHook{APIVersion: runtimehooksv1.GroupVersion.String(), Hook: "CanUpdateMachineSet"},
		},
		{
			Name:        "update-machine",
			RequestHook: runtimehooksv1.GroupVersionHook{APIVersion: runtimehooksv1.GroupVersion.String(), Hook: "UpdateMachine"},
		},
	}
}

// rolloutReason returns why the request cannot be handled by a machine
// configuration change, or "" when it can.
func rolloutReason(req *runtimehooksv1.CanUpdateMachineRequest) string {
	if req.Current.Machine.Spec.Version != req.Desired.Machine.Spec.Version && !kubernetesVersionUpgrade(req.Current.Machine.Spec.Version, req.Desired.Machine.Spec.Version) {
		return "the Kubernetes version changes"
	}

	if !equalJSON(req.Current.Machine.Spec.InfrastructureRef, req.Desired.Machine.Spec.InfrastructureRef) {
		return "the infrastructure machine reference changes"
	}

	// Machine.Spec.Bootstrap is deliberately not compared. spec.bootstrap.dataSecretName
	// is written by the Machine controller from the bootstrap config's status, so the
	// current and the desired Machine differ there for every control-plane Machine even
	// though nothing about the bootstrap configuration is being rolled out. Cluster API
	// zeroes the whole bootstrap spec before comparing Machine specs,
	// inplace_canupdatemachine.go:329-334, which CACPPT mirrors in
	// controllers/inplace_canupdatemachine.go:441-451. The bootstrap configuration an
	// in-place update has to apply is the TalosConfig in req.{Current,Desired}.BootstrapConfig,
	// compared from the rendered machine configuration below.
	if !equalJSON(infraSpec(req.Current.InfrastructureMachine), infraSpec(req.Desired.InfrastructureMachine)) {
		return "the infrastructure machine spec changes"
	}

	return ""
}

// rebootRequiredReason returns the first machine configuration path in the diff that
// Talos can only apply after a reboot.
func rebootRequiredReason(ops []jsonPatchOp) string {
	for _, op := range ops {
		path := strings.TrimPrefix(op.Path, "/")

		for _, prefix := range rebootRequiredConfigPaths {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return fmt.Sprintf("the machine configuration change at %q needs a reboot", op.Path)
			}
		}
	}

	return ""
}

// resolveEndpoint returns the Talos API address of a machine. Cluster API copies the
// addresses of the infrastructure machine into the Machine status
// (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineStatus.Addresses), but a hook request
// carries only the spec of the objects ("Only spec is provided, status fields are not
// included", docs/book/src/tasks/experimental-features/runtime-sdk/implement-in-place-update-hooks.md),
// so the extension reads the Machine from the management cluster when it was started with
// --kubeconfig. Without a Machine address the settings of the ExtensionConfig, the
// --endpoint fallback of the binary and the ProviderID of the machine are used.
func (h *Handler) resolveEndpoint(settings map[string]string, machine clusterv1.Machine) (string, error) {
	if address, err := h.machineAddress(machine); err != nil {
		h.log.Info("the addresses of the machine cannot be read from the management cluster", "machine", machine.Name, "error", err.Error())
	} else if address != "" {
		return address, nil
	}

	if address := settings[SettingAddress]; address != "" {
		return address, nil
	}

	if h.defaultEndpoint != "" {
		return h.defaultEndpoint, nil
	}

	if providerID := machine.Spec.ProviderID; strings.HasPrefix(providerID, "talos://") {
		return strings.TrimPrefix(providerID, "talos://"), nil
	}

	return "", errors.Errorf("cannot determine the Talos address of machine %q: set the %q setting of the ExtensionConfig or the --endpoint flag of the extension", machine.Name, SettingAddress)
}

// machineAddress returns the first address the infrastructure provider reported for a
// machine. Only a reader that implements MachineAddressReader can answer; without one the
// extension falls back to the settings, exactly as before stage L5.
func (h *Handler) machineAddress(machine clusterv1.Machine) (string, error) {
	reader, ok := h.metadata.(MachineAddressReader)
	if !ok {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	addresses, err := reader.MachineAddresses(ctx, machine.Namespace, machine.Name)
	if err != nil {
		return "", err
	}

	return pickMachineAddress(addresses), nil
}

// pickMachineAddress returns the address the extension reaches the node's Talos API at:
// the internal address the infrastructure provider reported, since Talos serves its API on
// the node's own network, falling back to any address the provider reported.
func pickMachineAddress(addresses []clusterv1.MachineAddress) string {
	for _, preferred := range []clusterv1.MachineAddressType{clusterv1.MachineInternalIP, clusterv1.MachineInternalDNS} {
		for _, address := range addresses {
			if address.Type == preferred && address.Address != "" {
				return address.Address
			}
		}
	}

	for _, address := range addresses {
		if address.Address != "" {
			return address.Address
		}
	}

	return ""
}

// renderMachineConfig builds the machine configuration a TalosConfig describes with the
// bootstrap provider's exported pkg/machineconfig: Generate for a generateType TalosConfig,
// then ApplyPatches for the RFC 6902 configPatches and the strategicPatches, whose
// templates are rendered with the metadata of metadataFrom and the templateVariables.
// The extension gathers only the inputs - the Cluster, the cluster's Talos secrets bundle
// and the metadata secret - and passes them, so the generated configuration and the
// rendered templates are identical to what the bootstrap controller writes.
//
// Follows the bootstrap provider's reconcileGenerate
// (controllers/talosconfig_controller.go:478-640), which calls the same
// pkg/machineconfig.Generate and pkg/machineconfig.ApplyPatches (stage T4b), reached
// through the go.mod replace.
func (h *Handler) renderMachineConfig(ctx context.Context, spec cabptv1.TalosConfigSpec, machine clusterv1.Machine) ([]byte, error) {
	base, standardPatches, err := h.machineConfigBase(ctx, spec, machine)
	if err != nil {
		return nil, err
	}

	metadata, metadataPending, err := h.templateContext(ctx, spec, machine)
	if err != nil {
		return nil, err
	}

	config, err := machineconfig.ApplyPatches(base, machineconfig.PatchInput{
		ConfigPatches:     machineConfigPatches(spec.ConfigPatches),
		StrategicPatches:  spec.StrategicPatches,
		StdPatches:        standardPatches,
		Metadata:          metadata,
		TemplateVariables: spec.TemplateVariables,
		MetadataPending:   metadataPending,
	})
	if err != nil {
		return nil, err
	}

	return []byte(config), nil
}

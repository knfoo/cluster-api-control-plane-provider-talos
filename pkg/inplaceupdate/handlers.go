// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The in-place update hooks the CACPPT manager serves for Talos machines.
//
// It answers CanUpdateMachine and UpdateMachine for a single Machine and CanUpdateMachineSet
// for the worker Machines of a MachineDeployment. A TalosConfig change is handled in place
// only when it is a machine configuration change that Talos accepts with ApplyConfiguration
// mode ApplyConfigurationRequest_NO_REBOOT after a DryRun. A Kubernetes version change is
// handled in place as well, one component at a time the way talosctl upgrade-k8s does it: the
// control-plane components of a control-plane Machine, the kubelet of a worker Machine.
// Everything else - a Talos version change, a Kubernetes version change that is not the next
// minor version, an infrastructure change, or a change Talos says needs a reboot - gets no
// patch, so Cluster API rolls the machine out.
package inplaceupdate

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	cabptv1beta1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1beta1"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultRetryAfterSeconds is how long UpdateMachine asks Cluster API to wait before it
	// looks at the machine again after applying a configuration.
	DefaultRetryAfterSeconds = 10

	// DefaultRequestTimeout bounds a single call to the Talos API.
	DefaultRequestTimeout = 30 * time.Second

	// talosConfigSecretName is the name of the secret the bootstrap provider stores the
	// talosconfig of a cluster under: clusterName + "-talosconfig".
	talosConfigSecretName = "-talosconfig"

	// talosConfigSecretKey is the key the bootstrap provider writes the talosconfig under in
	// that secret.
	talosConfigSecretKey = "talosconfig"
)

// rebootRequiredConfigPaths are the machine configuration keys Talos can only change by booting
// the node again: the machine install image and the cluster identity. They are the cheap guard in
// front of the DryRun, so these changes never need to reach a node. Every other change, in any
// document of the machine configuration, is decided by the DryRun with mode
// ApplyConfigurationRequest_NO_REBOOT, which is the authoritative answer: whether Talos applies a
// given change live depends on the Talos version the node runs.
var rebootRequiredConfigPaths = []string{
	"/machine/install",
	"/cluster/id",
	"/cluster/secret",
	"/cluster/token",
}

// Handler answers the in-place update hooks for the Talos machines of the clusters the manager
// manages.
type Handler struct {
	log            logr.Logger
	timeout        time.Duration
	metadata       MetadataReader
	manifestSyncer ManifestSyncer

	// newApplier builds the applier that talks to a machine with the talosconfig of its cluster.
	// NewHandler sets it to NewTalosApplier; a test hands in a fake node, the way manifestSyncer
	// already is.
	newApplier func(clusterName string, config *talosconfig.Config) Applier

	// manifestsSynced marks the clusters and Kubernetes versions whose bootstrap manifests were
	// already synced to the workload cluster. The sync is decided from the state of the control
	// plane, not from a flag recorded by CanUpdateMachine, so a restart of the provider in the
	// middle of an upgrade does not skip it; the map only avoids repeating a sync that is
	// idempotent anyway.
	manifestsSynced   map[string]bool
	manifestsSyncedMu sync.Mutex
}

// NewHandler builds the handlers the CACPPT manager serves. client is the manager's client of the
// management cluster: every hook reads the Machine it is called for, the addresses its
// infrastructure provider reported in the Machine status and the talosconfig secret the bootstrap
// provider created for the Machine's cluster. The rules the handlers need are the ones the control
// plane controller already declares in config/rbac/role.yaml (secrets, machines and
// machines/status, clusters).
func NewHandler(client client.Client, log logr.Logger) *Handler {
	return newHandler(NewKubeMetadataReader(client), log, DefaultRequestTimeout, nil)
}

// newHandler builds a handler with a given metadata reader and applier factory. The factory is nil
// in production, where NewTalosApplier talks to the node; a test replaces it with a fake node.
func newHandler(metadata MetadataReader, log logr.Logger, timeout time.Duration, applierFor func(clusterName string, config *talosconfig.Config) Applier) *Handler {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	if applierFor == nil {
		applierFor = func(_ string, config *talosconfig.Config) Applier {
			return NewTalosApplier(config)
		}
	}

	return &Handler{
		log:             log,
		timeout:         timeout,
		metadata:        metadata,
		manifestSyncer:  NewKubeManifestSyncer(log),
		newApplier:      applierFor,
		manifestsSynced: map[string]bool{},
	}
}

// CanUpdateMachine is the CanUpdateMachine hook. It returns a patch for the TalosConfig object only
// when the whole change is a machine configuration change that Talos applies with mode NO_REBOOT
// after a DryRun.
func (h *Handler) CanUpdateMachine(ctx context.Context, req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	if err := h.canUpdateMachine(ctx, req, resp); err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()
	}
}

func (h *Handler) canUpdateMachine(ctx context.Context, req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) error {
	machine := req.Current.Machine.Name

	// A refusal answers with Success and no patch: current and desired stay different, so Cluster
	// API rolls the Machine out.
	refuse := func(reason string) error {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", reason)

		return nil
	}

	if req.Current.Machine.Spec.Version != req.Desired.Machine.Spec.Version && !kubernetesVersionUpgrade(req.Current.Machine.Spec.Version, req.Desired.Machine.Spec.Version) {
		return refuse("the Kubernetes version changes")
	}

	if !equalJSON(req.Current.Machine.Spec.InfrastructureRef, req.Desired.Machine.Spec.InfrastructureRef) {
		return refuse("the infrastructure machine reference changes")
	}

	// Machine.Spec.Bootstrap is deliberately not compared: spec.bootstrap.dataSecretName is written
	// by the Machine controller from the bootstrap config's status, so the current and the desired
	// Machine differ there for every control-plane Machine even though nothing about the bootstrap
	// configuration is being rolled out. Cluster API zeroes the whole bootstrap spec before
	// comparing Machine specs. The bootstrap configuration an in-place update has to apply is the
	// TalosConfig in req.{Current,Desired}.BootstrapConfig, compared from the rendered machine
	// configuration below.
	if !equalJSON(infraSpec(req.Current.InfrastructureMachine), infraSpec(req.Desired.InfrastructureMachine)) {
		return refuse("the infrastructure machine spec changes")
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	plan, err := h.planInPlaceUpdate(ctx, req.Current.BootstrapConfig, req.Desired.BootstrapConfig, req.Current.Machine, req.Desired.Machine)
	if err != nil {
		return err
	}

	if plan.reason != "" {
		return refuse(plan.reason)
	}

	endpoint, applier, err := h.node(ctx, req.Desired.Machine)
	if err != nil {
		return err
	}

	config := plan.config

	// A Kubernetes version change of a control-plane Machine is handled in place too, the way the
	// in-place update hooks for KubeadmControlPlane handle it. The Machine patch carries
	// spec.version, and the machine configuration the node has to accept is the rendered desired
	// configuration with the control-plane component images patched step by step, so the DryRun
	// below covers both changes.
	if plan.versionChange {
		status, err := applier.NodeStatus(ctx, endpoint)
		if err != nil {
			return err
		}

		// A Kubernetes version change of a MachineSet-owned worker Machine is planned by the
		// MachineDeployment hook CanUpdateMachineSet and applied by UpdateMachine, which upgrades
		// the kubelet of the node. This hook is called by the control plane controllers, so a node
		// that runs no control-plane component gets no patch here and Cluster API keeps the
		// MachineSet's own rollout planning in charge of it.
		if !status.hasControlPlane() {
			return refuse("the machine runs no control-plane components")
		}

		config, err = stagedKubernetesConfig(config, status, req.Desired.Machine.Spec.Version)
		if err != nil {
			return err
		}
	}

	// The owner's condition: only a change the node accepts with mode
	// ApplyConfigurationRequest_NO_REBOOT after a DryRun is handled in place.
	reason, err := h.dryRunReason(ctx, applier, endpoint, config)
	if err != nil {
		return err
	}

	if reason != "" {
		return refuse(reason)
	}

	if plan.versionChange {
		patch, err := jsonPatch([]jsonPatchOp{{Op: "replace", Path: "/spec/version", Value: req.Desired.Machine.Spec.Version}})
		if err != nil {
			return err
		}

		resp.MachinePatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	if len(plan.ops) > 0 {
		patch, err := configPatch(req.Current.BootstrapConfig, req.Desired.BootstrapConfig)
		if err != nil {
			return err
		}

		resp.BootstrapConfigPatch = runtimehooksv1.Patch{
			PatchType: runtimehooksv1.JSONPatchType,
			Patch:     patch,
		}
	}

	h.log.Info("machine can be updated in place", "machine", machine, "endpoint", endpoint, "operations", len(plan.ops), "version", req.Desired.Machine.Spec.Version)

	return nil
}

// UpdateMachine is the UpdateMachine hook. It applies the machine configuration with mode NO_REBOOT
// and is idempotent: a second call with the same desired configuration reports the update as
// completed. When the node runs Kubernetes components, the configuration applied is the staged one
// and the update is completed only when the node reports every Kubernetes component it runs - the
// control-plane components of a control-plane Machine, the kubelet of a worker Machine - at the
// Machine's version.
func (h *Handler) UpdateMachine(ctx context.Context, req *runtimehooksv1.UpdateMachineRequest, resp *runtimehooksv1.UpdateMachineResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	if err := h.updateMachine(ctx, req, resp); err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()
	}
}

func (h *Handler) updateMachine(ctx context.Context, req *runtimehooksv1.UpdateMachineRequest, resp *runtimehooksv1.UpdateMachineResponse) error {
	machine := req.Desired.Machine.Name

	// CanUpdateMachine already rendered this configuration, so the same render is used here; a
	// failure is a real error rather than a reason to roll the machine out.
	spec, err := talosConfigSpec(req.Desired.BootstrapConfig)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	config, metadataPending, err := h.renderMachineConfig(ctx, spec, req.Desired.Machine)
	if err != nil {
		return err
	}

	// metadataFrom is set but the metadata secret the infrastructure provider (Metal3/CAPM3)
	// writes after the bootstrap data does not exist yet, so the rendered configuration is the
	// phase-1 one: it lacks every strategic patch that still contains a template expression.
	// Applying it would push a machine configuration to the node that is missing the patches its
	// metadata renders. The update is reported as in progress instead, so Cluster API calls the
	// hook again once the metadata is there, and the Machine is neither updated with a partial
	// configuration nor rolled out.
	if metadataPending {
		h.log.Info("the metadata of the machine configuration is not available yet, so the machine configuration is not applied", "machine", machine, "retryAfterSeconds", DefaultRetryAfterSeconds)
		resp.RetryAfterSeconds = DefaultRetryAfterSeconds

		return nil
	}

	endpoint, applier, err := h.node(ctx, req.Desired.Machine)
	if err != nil {
		return err
	}

	status, err := applier.NodeStatus(ctx, endpoint)
	if err != nil {
		return err
	}

	// A node that runs Kubernetes components is upgraded one component at a time, in the order
	// upgradeComponents defines: the control-plane components of a control-plane Machine, the
	// kubelet of a worker Machine, which is the component talosctl upgrade-k8s patches on a node
	// without control-plane components. A node that runs no Kubernetes component yet - a machine
	// whose bootstrap has not started - gets the plain machine configuration applied.
	target := req.Desired.Machine.Spec.Version
	staged := target != "" && status.hasKubernetes()

	if staged {
		config, err = stagedKubernetesConfig(config, status, target)
		if err != nil {
			return err
		}
	}

	dryRun, err := applier.DryRun(ctx, endpoint, config)
	if err != nil {
		return err
	}

	if dryRun.NoChanges && (!staged || componentsAtTarget(status, target)) {
		// The kube-proxy DaemonSet and the CoreDNS deployment live in the workload cluster, so a
		// control-plane Machine is finished only after the bootstrap manifests of the new version
		// are synced to it.
		if staged && status.hasControlPlane() {
			retryAfter, done, err := h.syncManifests(ctx, req.Desired.Machine, spec.TalosVersion, endpoint, target, applier)
			if err != nil {
				return err
			}

			if !done {
				resp.RetryAfterSeconds = retryAfter

				return nil
			}
		}

		h.log.Info("machine configuration is already applied", "machine", machine, "endpoint", endpoint)

		return nil
	}

	if dryRun.RebootRequired {
		return fmt.Errorf("Talos needs a reboot to apply the machine configuration of machine %q: %s", machine, dryRun.Details)
	}

	if err := applier.Apply(ctx, endpoint, config); err != nil {
		return err
	}

	resp.RetryAfterSeconds = DefaultRetryAfterSeconds

	if staged {
		h.log.Info("machine configuration applied in place", "machine", machine, "endpoint", endpoint, "component", firstComponentNotAtTarget(status, target))
	} else {
		h.log.Info("machine configuration applied in place", "machine", machine, "endpoint", endpoint)
	}

	return nil
}

// syncManifests syncs the bootstrap manifests a node rendered for the target Kubernetes version to
// the workload cluster, once per cluster and version. It reports how long the Machine has to wait
// and whether its update is complete.
//
// Whether the sync is needed is read from the control plane itself, not from anything
// CanUpdateMachine recorded, so restarting the provider in the middle of an upgrade does not skip
// it. The sync runs on the control-plane Machine whose peers are already at the target version, the
// last one triggered: the trigger rolls in-place updates out one Machine at a time and waits for the
// Machine that is in progress before it triggers the next one, and Success with a retry means "update
// in progress". Holding the first Machine open until its peers reach the target would mean the peers
// are never triggered, no Machine ever becomes the last one, and the control plane stalls with one
// Machine in progress. A Machine whose peers are not there yet is simply done: its own update is
// complete and the rollout continues. A peer that cannot be read at all is not a peer that is not at
// the target: the sync is not skipped for it, the Machine stays in progress and Cluster API calls the
// hook again.
func (h *Handler) syncManifests(ctx context.Context, machine clusterv1.Machine, talosVersion, endpoint, target string, applier Applier) (int32, bool, error) {
	if h.manifestSyncer == nil {
		return 0, true, nil
	}

	// The sync of one Kubernetes version of one cluster happens exactly once.
	key := machine.Namespace + "/" + machine.Spec.ClusterName + "/" + target

	// The mutex guards the map only. The peers are read outside the lock: a node that answers
	// slowly must not block the hook of every other machine.
	h.manifestsSyncedMu.Lock()
	alreadySynced := h.manifestsSynced[key]
	h.manifestsSyncedMu.Unlock()

	if alreadySynced {
		return 0, true, nil
	}

	if reader, ok := h.metadata.(ControlPlaneMachineReader); ok {
		// A peer whose state cannot be determined at all - its Machine cannot be read, it has no
		// address, or its node does not answer - keeps the update in progress instead of skipping a
		// sync the control plane has not finished.
		retry := func(reason string) (int32, bool, error) {
			h.log.Info("the Kubernetes version of a control-plane machine cannot be read, so the bootstrap manifests are not synced yet", "machine", machine.Name, "version", target, "reason", reason)

			return DefaultRetryAfterSeconds, false, nil
		}

		machines, err := reader.ControlPlaneMachines(ctx, machine.Namespace, machine.Spec.ClusterName)
		if err != nil {
			return retry(err.Error())
		}

		for _, peer := range machines {
			if peer.Name == machine.Name {
				continue
			}

			address, err := h.machineAddress(ctx, peer)
			if err != nil || address == "" {
				return retry(fmt.Sprintf("the peer cannot be reached: %v", err))
			}

			status, err := applier.NodeStatus(ctx, address)
			if err != nil {
				return retry(err.Error())
			}

			if !componentsAtTarget(status, target) {
				h.log.Info("the bootstrap manifests are synced by the last control-plane Machine to reach the target Kubernetes version", "machine", machine.Name, "version", target)

				return 0, false, nil
			}
		}
	}

	// The kubeconfig of the workload cluster is the one Cluster API stores in the secret named
	// <cluster>-kubeconfig, under the key secret.KubeconfigDataName.
	kubeconfigName := secret.Name(machine.Spec.ClusterName, secret.Kubeconfig)

	kubeconfigData, err := h.metadata.Secret(ctx, machine.Namespace, kubeconfigName)
	if err != nil {
		return 0, false, err
	}

	if kubeconfigData == nil {
		return 0, false, errors.Errorf("the kubeconfig secret %q of cluster %q does not exist yet, so the bootstrap manifests cannot be synced to the workload cluster", kubeconfigName, machine.Spec.ClusterName)
	}

	kubeconfig, ok := kubeconfigData[secret.KubeconfigDataName]
	if !ok || len(kubeconfig) == 0 {
		return 0, false, errors.Errorf("the kubeconfig secret %q of cluster %q has no key %q", kubeconfigName, machine.Spec.ClusterName, secret.KubeconfigDataName)
	}

	objects, err := applier.BootstrapManifests(ctx, endpoint)
	if err != nil {
		return 0, false, errors.Wrapf(err, "reading the bootstrap manifests of machine %q", machine.Name)
	}

	h.manifestsSyncedMu.Lock()
	defer h.manifestsSyncedMu.Unlock()

	if h.manifestsSynced[key] {
		return 0, true, nil
	}

	if err := h.manifestSyncer.SyncManifests(ctx, kubeconfig, objects, talosVersion); err != nil {
		if errors.Is(err, errManifestsNotReconciled) {
			h.log.Info("the synced bootstrap manifests are still reconciling in the workload cluster", "machine", machine.Name, "version", target)

			return DefaultRetryAfterSeconds, false, nil
		}

		return 0, false, err
	}

	h.manifestsSynced[key] = true
	h.log.Info("the bootstrap manifests were synced to the workload cluster", "cluster", machine.Spec.ClusterName, "version", target, "manifests", len(objects))

	return 0, true, nil
}

// rebootRequiredReason returns the first machine configuration path in the diff that Talos can only
// apply after a reboot. It matches the machine install image and the cluster identity of the
// machine and cluster documents and nothing else: every other change, in any document of the
// machine configuration, is decided by the DryRun of the node, whose Talos version says whether it
// accepts the change with mode ApplyConfigurationRequest_NO_REBOOT.
func rebootRequiredReason(ops []jsonPatchOp) string {
	for _, op := range ops {
		for _, prefix := range rebootRequiredConfigPaths {
			if op.Path == prefix || strings.HasPrefix(op.Path, prefix+"/") {
				return fmt.Sprintf("the machine configuration change at %q needs a reboot", op.Path)
			}
		}
	}

	return ""
}

// inPlacePlan is what the pipeline the CanUpdate hooks share computes for a request.
type inPlacePlan struct {
	// ops is the machine configuration diff between the current and the desired object.
	ops []jsonPatchOp

	// config is the rendered desired machine configuration the node has to accept.
	config []byte

	// reason is why the Machine or MachineSet has to roll out; "" means a node can apply the change
	// in place.
	reason string

	// versionChange is true when the Kubernetes version of the Machine or MachineSet changes, which
	// the hook patches on the Machine resp. the MachineSet template.
	versionChange bool
}

// planInPlaceUpdate is the pipeline both CanUpdate hooks share: it decodes the machine configuration
// spec of the current and the desired bootstrap configuration object of the request, renders the
// machine configuration the way the bootstrap provider builds it, diffs it and runs the cheap reboot
// guard. CanUpdateMachine passes the Machines of its request; CanUpdateMachineSet of
// pkg/inplaceupdate/machineset.go passes the Machine its template describes, whose spec carries the
// template's Kubernetes version.
//
// The machine configuration of a TalosConfig is not its spec: it is the generated or supplied base
// configuration with the configuration patches and the strategic patches applied, exactly as the
// bootstrap provider builds it. A configuration the hooks cannot render - a generated configuration
// without access to the cluster secrets, a Machine whose metadata is not available yet - is answered
// with Success and no patch, which leaves current and desired different and makes Cluster API roll
// the Machine out; answering with Failure would turn the call into an error.
func (h *Handler) planInPlaceUpdate(
	ctx context.Context,
	current, desired runtime.RawExtension,
	currentMachine, desiredMachine clusterv1.Machine,
) (inPlacePlan, error) {
	currentVersion, desiredVersion := currentMachine.Spec.Version, desiredMachine.Spec.Version

	currentSpec, err := talosConfigSpec(current)
	if err != nil {
		return inPlacePlan{}, err
	}

	desiredSpec, err := talosConfigSpec(desired)
	if err != nil {
		return inPlacePlan{}, err
	}

	if currentSpec.TalosVersion != desiredSpec.TalosVersion {
		return inPlacePlan{reason: "the Talos version changes"}, nil
	}

	currentConfig, _, err := h.renderMachineConfig(ctx, currentSpec, currentMachine)
	if err != nil {
		return inPlacePlan{reason: err.Error()}, nil
	}

	desiredConfig, _, err := h.renderMachineConfig(ctx, desiredSpec, desiredMachine)
	if err != nil {
		return inPlacePlan{reason: err.Error()}, nil
	}

	ops, err := configDiff(currentConfig, desiredConfig)
	if err != nil {
		return inPlacePlan{}, err
	}

	if reason := rebootRequiredReason(ops); reason != "" {
		return inPlacePlan{reason: reason}, nil
	}

	if len(ops) == 0 && currentVersion == desiredVersion {
		return inPlacePlan{reason: "the machine configuration does not change"}, nil
	}

	return inPlacePlan{ops: ops, config: desiredConfig, versionChange: currentVersion != desiredVersion}, nil
}

// dryRunReason is the rule of the in-place hooks: a machine configuration change is handled in place
// only when a node accepts it with mode ApplyConfigurationRequest_NO_REBOOT after a DryRun. It asks
// the node about config and returns why the change has to roll out, or "" when the node accepts it.
// An error is a failure of the DryRun call itself, which the caller turns into a hook failure or a
// rollout reason.
func (h *Handler) dryRunReason(ctx context.Context, applier Applier, endpoint string, config []byte) (string, error) {
	status, err := applier.DryRun(ctx, endpoint, config)
	if err != nil {
		return "", err
	}

	if status.RebootRequired {
		return fmt.Sprintf("Talos needs a reboot to apply the machine configuration at %q: %s", endpoint, status.Details), nil
	}

	return "", nil
}

// node returns the Talos API address of a machine and the applier that reaches it with the
// talosconfig the bootstrap provider created for the machine's cluster. Cluster API copies the
// addresses of the infrastructure machine into the Machine status, but a hook request carries only
// the spec of the objects, so the address is read from the Machine in the management cluster. There
// is no address setting of the ExtensionConfig and no --endpoint flag to fall back to: a Machine
// whose infrastructure provider has not reported an address cannot be updated in place.
func (h *Handler) node(ctx context.Context, machine clusterv1.Machine) (string, Applier, error) {
	address, err := h.machineAddress(ctx, machine)
	if err != nil {
		return "", nil, errors.Wrapf(err, "reading the address of machine %q", machine.Name)
	}

	if address == "" {
		return "", nil, errors.Errorf("machine %q has no address in its status, so its Talos API cannot be reached", machine.Name)
	}

	config, err := h.clusterTalosConfig(ctx, machine.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return "", nil, err
	}

	return address, h.newApplier(machine.Spec.ClusterName, config), nil
}

// clusterTalosConfig reads the talosconfig the bootstrap provider keeps for a cluster: the secret
// named <cluster>-talosconfig, with the talosconfig under the key "talosconfig".
func (h *Handler) clusterTalosConfig(ctx context.Context, namespace, clusterName string) (*talosconfig.Config, error) {
	name := clusterName + talosConfigSecretName

	data, err := h.metadata.Secret(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	if data == nil {
		return nil, errors.Errorf("the talosconfig secret %q of cluster %q does not exist yet, so no machine of the cluster can be reached", name, clusterName)
	}

	config, err := talosconfig.FromBytes(data[talosConfigSecretKey])
	if err != nil {
		return nil, errors.Wrapf(err, "decoding the talosconfig in the secret %q of cluster %q", name, clusterName)
	}

	return config, nil
}

// machineAddress returns the address the infrastructure provider reported for a machine.
func (h *Handler) machineAddress(ctx context.Context, machine clusterv1.Machine) (string, error) {
	addresses, err := h.metadata.MachineAddresses(ctx, machine.Namespace, machine.Name)
	if err != nil {
		return "", err
	}

	return pickMachineAddress(addresses), nil
}

// pickMachineAddress returns the address a hook reaches the node's Talos API at: the internal
// address the infrastructure provider reported, since Talos serves its API on the node's own
// network, falling back to any address the provider reported. TalosEndpoint is the rule every Talos
// API endpoint in the provider is built with, so the link-local rule exists once in the provider.
func pickMachineAddress(addresses []clusterv1.MachineAddress) string {
	for _, preferred := range []clusterv1.MachineAddressType{clusterv1.MachineInternalIP, clusterv1.MachineInternalDNS} {
		for _, address := range addresses {
			if endpoint, ok := TalosEndpoint(address.Address); address.Type == preferred && ok {
				return endpoint
			}
		}
	}

	for _, address := range addresses {
		if endpoint, ok := TalosEndpoint(address.Address); ok {
			return endpoint
		}
	}

	return ""
}

// TalosEndpoint returns the address to use as a Talos API endpoint, and false if the address cannot
// be one.
//
// Link-local addresses are skipped: they are only reachable on the link of the node, and an IPv6
// link-local address carries a zone (e.g. "fe80::1%eth0") that is not valid in the gRPC target the
// Talos client builds from its endpoints, so a single one of them breaks the client for every
// endpoint. Infrastructure providers such as Metal3 report them in the Machine addresses. Addresses
// that are not IPs (host names) are kept as they are.
func TalosEndpoint(address string) (string, bool) {
	if address == "" {
		return "", false
	}

	ip, err := netip.ParseAddr(address)
	if err != nil {
		return address, true
	}

	if ip.IsLinkLocalUnicast() {
		return "", false
	}

	return ip.WithZone("").String(), true
}

// renderMachineConfig builds the machine configuration a TalosConfig describes with the bootstrap
// provider's exported pkg/machineconfig: the base configuration, then ApplyPatches for the RFC 6902
// configPatches and the strategicPatches, whose templates are rendered with the data the metadata
// secret, the Cluster, the Machine and the templateVariables provide. The hooks gather only the
// inputs and pass them, so the rendered configuration is identical to what the bootstrap controller
// writes.
//
// The second return value reports that metadataFrom is set but the metadata secret does not exist
// yet: the configuration is then the phase-1 configuration of the two-phase bootstrap, missing the
// patches the metadata renders. It is complete enough to diff and to patch an object with, but it
// must never be applied to a node - see UpdateMachine.
func (h *Handler) renderMachineConfig(ctx context.Context, spec cabptv1beta1.TalosConfigSpec, machine clusterv1.Machine) ([]byte, bool, error) {
	base, standardPatches, err := h.machineConfigBase(ctx, spec, machine)
	if err != nil {
		return nil, false, err
	}

	templateData, metadataPending, err := h.templateContext(ctx, spec, machine)
	if err != nil {
		return nil, false, err
	}

	config, err := machineconfig.ApplyPatches(base, machineconfig.PatchInput{
		ConfigPatches:    spec.ConfigPatches,
		StrategicPatches: spec.StrategicPatches,
		StdPatches:       standardPatches,
		TemplateData:     templateData,
		MetadataPending:  metadataPending,
	})
	if err != nil {
		return nil, false, err
	}

	return []byte(config), metadataPending, nil
}

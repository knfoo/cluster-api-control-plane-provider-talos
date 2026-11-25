// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// The Cluster API Runtime SDK extension that updates Talos machines in place.
//
// It answers CanUpdateMachine and UpdateMachine for a single Machine and CanUpdateMachineSet
// for the worker Machines of a MachineDeployment (pkg/inplaceupdate/machineset.go). The
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
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/util/secret"
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
// is the authoritative answer, but these changes never need to reach a node. Everything
// else, including machine/env, is decided by the DryRun, because whether Talos applies a
// given change with mode ApplyConfigurationRequest_NO_REBOOT depends on the Talos version
// of the node.
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
	manifestSyncer  ManifestSyncer

	// manifestsSynced marks the clusters and Kubernetes versions whose bootstrap manifests
	// were already synced to the workload cluster. The sync is decided from the state of
	// the control plane, not from a flag recorded by CanUpdateMachine, so a restart of the
	// provider mid-upgrade does not skip it; the map only avoids repeating a sync that is
	// idempotent anyway.
	manifestsSynced   map[string]bool
	manifestsSyncedMu sync.Mutex
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
		manifestSyncer:  NewKubeManifestSyncer(log),
		manifestsSynced: map[string]bool{},
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

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	plan, err := h.planInPlaceUpdate(ctx, req.Current.BootstrapConfig, req.Desired.BootstrapConfig, req.Current.Machine, req.Desired.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if plan.reason != "" {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", plan.reason)

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
	config := plan.config

	if plan.versionChange {
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

		config, err = stagedKubernetesConfig(config, nodeStatus, req.Desired.Machine.Spec.Version)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = err.Error()

			return
		}
	}

	// The owner's condition: only a change the node accepts with mode
	// ApplyConfigurationRequest_NO_REBOOT and DryRun is handled in place.
	reason, err := h.dryRunReason(ctx, endpoint, config)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	if reason != "" {
		h.log.Info("machine cannot be updated in place", "machine", machine, "reason", reason)

		return
	}

	if plan.versionChange {
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

	if len(plan.ops) > 0 {
		patch, err := configPatch(req.Current.BootstrapConfig, req.Desired.BootstrapConfig)
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

	h.log.Info("machine can be updated in place", "machine", machine, "endpoint", endpoint, "operations", len(plan.ops))
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
	config, metadataPending, err := h.renderMachineConfig(ctx, spec, req.Desired.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()

		return
	}

	// metadataFrom is set but the metadata secret the infrastructure provider (Metal3/CAPM3)
	// writes after the bootstrap data does not exist yet, so the rendered configuration is
	// the phase-1 configuration: it lacks every strategic patch that still contains a
	// template expression. Applying it would push a machine configuration to the node that
	// is missing the patches its metadata renders. The update is reported as in progress
	// instead, so Cluster API calls the hook again once the metadata is there, and the
	// Machine is neither updated with a partial configuration nor rolled out.
	if metadataPending {
		h.log.Info("the metadata of the machine configuration is not available yet, so the machine configuration is not applied", "machine", machine, "retryAfterSeconds", DefaultRetryAfterSeconds)
		resp.RetryAfterSeconds = DefaultRetryAfterSeconds

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
	// (upgradeKubeletOnNode). A node that runs no
	// Kubernetes component yet - a machine whose bootstrap has not started - gets the plain
	// machine configuration apply.
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
		// The kube-proxy DaemonSet and the CoreDNS deployment live in the workload
		// cluster: a Kubernetes version change of a control-plane Machine is finished only
		// after the bootstrap manifests the node rendered for the new version are synced to
		// that cluster, the last step of talosctl upgrade-k8s (PerformManifestsSync). The
		// sync runs once, on the control-plane Machine that reaches the target version
		// last: a Machine whose peers are not there yet reports its own update as done,
		// because holding it open would stop the peers from ever being triggered. Whether
		// the sync is needed is read from the control plane itself, not from anything
		// CanUpdateMachine recorded, so restarting the provider in the middle of an upgrade
		// does not skip it.
		if h.manifestSyncer != nil && h.metadata != nil && staged && nodeStatus.hasControlPlane() {
			key := manifestSyncKey(req.Desired.Machine, target)

			h.manifestsSyncedMu.Lock()
			alreadySynced := h.manifestsSynced[key]
			h.manifestsSyncedMu.Unlock()

			if !alreadySynced {
				// The trigger rolls in-place updates out one Machine at a time and waits for the
				// Machine that is in progress before it triggers the next one, and Success with a
				// retry means "update in progress". Holding the first Machine open until its peers
				// reach the target would mean the peers are never triggered, no Machine ever
				// becomes the last one, and the control plane stalls with one Machine in progress.
				// So the sync runs on the Machine whose peers are already at the target version,
				// the last one triggered, and a Machine whose peers are not there yet is simply
				// done: its own update is complete and the rollout continues. A peer that cannot
				// be read at all is not a peer that is not at the target: the sync is not skipped
				// for it, the Machine stays in progress and Cluster API calls the hook again.
				atTarget, err := h.controlPlaneAtTarget(ctx, req, target)
				if err != nil {
					h.log.Info("the Kubernetes version of a control-plane machine cannot be read, so the bootstrap manifests are not synced yet", "machine", machine, "version", target, "error", err.Error())
					resp.RetryAfterSeconds = DefaultRetryAfterSeconds

					return
				}

				if !atTarget {
					h.log.Info("the bootstrap manifests are synced by the last control-plane Machine to reach the target Kubernetes version", "machine", machine, "version", target)
					resp.RetryAfterSeconds = 0

					return
				}

				if err := h.syncBootstrapManifests(ctx, req, spec, endpoint, target); err != nil {
					if errors.Is(err, errManifestsNotReconciled) {
						h.log.Info("the synced bootstrap manifests are still reconciling in the workload cluster", "machine", machine, "version", target)
						resp.RetryAfterSeconds = DefaultRetryAfterSeconds

						return
					}

					resp.Status = runtimehooksv1.ResponseStatusFailure
					resp.Message = err.Error()

					return
				}
			}
		}

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

// manifestSyncKey identifies the bootstrap manifest sync of one Kubernetes version of one
// cluster: namespace, cluster name and the target Kubernetes version.
func manifestSyncKey(machine clusterv1.Machine, version string) string {
	return machine.Namespace + "/" + machine.Spec.ClusterName + "/" + version
}

// controlPlaneAtTarget reports whether every control-plane Machine of the cluster runs
// its Kubernetes components at target, the condition for syncing the bootstrap manifests
// to the workload cluster. It returns an error when the state of a peer cannot be
// determined at all - its Machine cannot be read, it has no address, or its node does not
// answer. That is not the same as a peer that is not at the target yet: the caller keeps
// the update in progress and retries, instead of skipping a sync the control plane has
// not finished.
func (h *Handler) controlPlaneAtTarget(ctx context.Context, req *runtimehooksv1.UpdateMachineRequest, target string) (bool, error) {
	reader, ok := h.metadata.(ControlPlaneMachineReader)
	if !ok {
		return true, nil
	}

	machines, err := reader.ControlPlaneMachines(ctx, req.Desired.Machine.Namespace, req.Desired.Machine.Spec.ClusterName)
	if err != nil {
		return false, errors.Wrapf(err, "listing the control-plane machines of cluster %q", req.Desired.Machine.Spec.ClusterName)
	}

	for _, peer := range machines {
		if peer.Name == req.Desired.Machine.Name {
			continue
		}

		address, err := h.machineAddress(peer)
		if err != nil {
			return false, errors.Wrapf(err, "reading the address of control-plane machine %q", peer.Name)
		}

		if address == "" {
			return false, errors.Errorf("control-plane machine %q has no address, so its Kubernetes version cannot be checked", peer.Name)
		}

		status, err := h.applier.NodeStatus(ctx, address)
		if err != nil {
			return false, errors.Wrapf(err, "reading the Kubernetes components of control-plane machine %q at %q", peer.Name, address)
		}

		if !componentsAtTarget(status, target) {
			return false, nil
		}
	}

	return true, nil
}

// syncBootstrapManifests syncs the bootstrap manifests of the machine to the workload
// cluster exactly once per cluster and Kubernetes version. The kubeconfig of the workload
// cluster is the one Cluster API stores in the secret named <cluster>-kubeconfig (util/secret:
// secret.Name(clusterName, secret.Kubeconfig), kubeconfig stored under secret.KubeconfigDataName).
func (h *Handler) syncBootstrapManifests(ctx context.Context, req *runtimehooksv1.UpdateMachineRequest, spec cabptv1.TalosConfigSpec, endpoint, target string) error {
	clusterName := req.Desired.Machine.Spec.ClusterName
	key := manifestSyncKey(req.Desired.Machine, target)

	h.manifestsSyncedMu.Lock()
	defer h.manifestsSyncedMu.Unlock()

	if h.manifestsSynced[key] {
		return nil
	}

	kubeconfigName := secret.Name(clusterName, secret.Kubeconfig)

	kubeconfigData, err := h.metadata.Secret(ctx, req.Desired.Machine.Namespace, kubeconfigName)
	if err != nil {
		return err
	}

	if kubeconfigData == nil {
		return errors.Errorf("the kubeconfig secret %q of cluster %q does not exist yet, so the bootstrap manifests cannot be synced to the workload cluster", kubeconfigName, clusterName)
	}

	kubeconfig, ok := kubeconfigData[secret.KubeconfigDataName]
	if !ok || len(kubeconfig) == 0 {
		return errors.Errorf("the kubeconfig secret %q of cluster %q has no key %q", kubeconfigName, clusterName, secret.KubeconfigDataName)
	}

	objects, err := h.applier.BootstrapManifests(ctx, endpoint)
	if err != nil {
		return errors.Wrapf(err, "reading the bootstrap manifests of machine %q", req.Desired.Machine.Name)
	}

	if err := h.manifestSyncer.SyncManifests(ctx, kubeconfig, objects, spec.TalosVersion); err != nil {
		return err
	}

	h.manifestsSynced[key] = true

	h.log.Info("the bootstrap manifests were synced to the workload cluster", "cluster", clusterName, "version", target, "manifests", len(objects))

	return nil
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
// Talos can only apply after a reboot. It is only the cheap guard in front of the DryRun:
// every other change, machine/env included, is decided by the node, whose Talos version
// says whether it accepts the change with mode ApplyConfigurationRequest_NO_REBOOT.
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

// inPlacePlan is what the pipeline the two CanUpdate hooks share computes for a request.
type inPlacePlan struct {
	// ops is the machine configuration diff between the current and the desired object.
	ops []jsonPatchOp

	// config is the rendered desired machine configuration the node has to accept.
	config []byte

	// reason is why the Machine or MachineSet has to roll out; "" means a node can apply
	// the change in place.
	reason string

	// versionChange is true when the Kubernetes version of the Machine or MachineSet
	// changes, which the hook patches on the Machine resp. the MachineSet template.
	versionChange bool
}

// planInPlaceUpdate is the pipeline both CanUpdate hooks share: decode the machine
// configuration spec of the current and the desired bootstrap configuration object of the
// request, render the machine configuration the way the bootstrap provider builds it, diff
// it and run the cheap reboot guard. The Kubernetes version of the rollout is read from the
// Machine spec handed in: CanUpdateMachine passes the Machines of the request, CanUpdateMachineSet
// the Machine its template describes (templateMachine), whose spec carries the template's version.
//
// The machine configuration of a TalosConfig and of a TalosConfigTemplate is not the spec:
// it is the generated or supplied base configuration with the configuration patches and the
// (template rendered) strategic patches applied, exactly as the bootstrap provider builds
// it. A configuration the extension cannot render - a generated configuration without access
// to the cluster secrets, a template whose metadata is not available yet - is answered with
// Success and no patch, which leaves current and desired different and makes Cluster API
// roll the Machine resp. the MachineSet out; answering with Failure would turn the call into
// an error.
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

// dryRunReason is the owner's condition of docs/adr/ADR-0003: a machine configuration change
// is handled in place only when a node accepts it with mode
// ApplyConfigurationRequest_NO_REBOOT after a DryRun. It asks the node at endpoint about
// config and returns why the change has to roll out, or "" when the node accepts it. An error
// is a failure of the DryRun call itself, which the caller turns into a hook failure or a
// rollout reason.
func (h *Handler) dryRunReason(ctx context.Context, endpoint string, config []byte) (string, error) {
	status, err := h.applier.DryRun(ctx, endpoint, config)
	if err != nil {
		return "", err
	}

	if status.RebootRequired {
		return fmt.Sprintf("Talos needs a reboot to apply the machine configuration at %q: %s", endpoint, status.Details), nil
	}

	return "", nil
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
// extension falls back to the settings.
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
// The second return value reports that metadataFrom is set but the metadata secret does
// not exist yet: the configuration is then the phase-1 configuration of the two-phase
// bootstrap, which is missing the patches the metadata renders. It is complete enough to
// diff and to patch an object with, but it must never be applied to a node - see
// UpdateMachine.
//
// Follows the bootstrap provider's reconcileGenerate
// (controllers/talosconfig_controller.go:478-640), which calls the same
// pkg/machineconfig.Generate and pkg/machineconfig.ApplyPatches, reached
// through the go.mod replace.
func (h *Handler) renderMachineConfig(ctx context.Context, spec cabptv1.TalosConfigSpec, machine clusterv1.Machine) ([]byte, bool, error) {
	base, standardPatches, err := h.machineConfigBase(ctx, spec, machine)
	if err != nil {
		return nil, false, err
	}

	metadata, metadataPending, err := h.templateContext(ctx, spec, machine)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}

	return []byte(config), metadataPending, nil
}

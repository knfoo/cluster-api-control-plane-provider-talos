// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Syncing the bootstrap manifests of a Talos control-plane node (kube-proxy, CoreDNS) to
// the Kubernetes cluster the node belongs to.
//
// The staged Kubernetes update patches the machine configuration of every control-plane
// Machine, so the node's BootstrapManifestsConfig carries the kube-proxy manifest of the new
// Kubernetes version, but the kube-proxy DaemonSet of the workload cluster is only updated when
// those manifests are synced to it - the step talosctl upgrade-k8s performs last. Talos >= v1.13
// syncs with a server-side-apply inventory managed by
// github.com/siderolabs/go-kubernetes/kubernetes/ssa; Talos v1.12 has no SSA manifest support
// (compatibility.TalosVersion.SupportsSSAManifestSync) and falls back to the legacy
// manifests.SyncWithLog.
package inplaceupdate

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/siderolabs/go-kubernetes/kubernetes/manifests"
	"github.com/siderolabs/go-kubernetes/kubernetes/ssa"
	"github.com/siderolabs/go-kubernetes/kubernetes/ssa/cli"
	apimachine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// manifestSyncWaitInterval and manifestSyncWaitTimeout bound how long one hook call
	// waits for the applied bootstrap manifests to reconcile in the workload cluster.
	// upgrade-k8s waits for the whole reconcile; here the wait is short so the hook reports
	// the update as in progress and Cluster API calls it again rather than holding the
	// request open for minutes.
	manifestSyncWaitInterval = 2 * time.Second
	manifestSyncWaitTimeout  = 10 * time.Second
)

// errManifestsNotReconciled reports bootstrap manifests that were applied but are not
// reconciled in the workload cluster yet. It is not a failure: the sync is idempotent, so
// the hook retries until the objects are ready.
var errManifestsNotReconciled = errors.New("the synced bootstrap manifests are not reconciled yet")

// ManifestSyncer applies the bootstrap manifests read from a Talos node to the workload
// cluster. It is a seam: the extension runs it against the real cluster, a unit test runs it
// against a recorder.
type ManifestSyncer interface {
	// SyncManifests applies objects, read from the COSI state of a control-plane node, to
	// the Kubernetes cluster described by kubeconfig. talosVersion is the Talos version of
	// the machine whose manifests are synced; it decides whether the server-side-apply
	// sync is available. A returned error wrapping errManifestsNotReconciled means the
	// objects were applied and are still reconciling.
	SyncManifests(ctx context.Context, kubeconfig []byte, objects []*unstructured.Unstructured, talosVersion string) error
}

// NewKubeManifestSyncer creates the syncer used by the extension binary.
func NewKubeManifestSyncer(log logr.Logger) ManifestSyncer {
	return &kubeManifestSyncer{log: log}
}

type kubeManifestSyncer struct {
	log logr.Logger
}

// SyncManifests mirrors PerformManifestsSync of talosctl upgrade-k8s: the manifests are
// read from the node (pkg/inplaceupdate/talos.go), the sync chooses SSA or the legacy path
// by the Talos version, and both paths apply the objects idempotently.
func (s *kubeManifestSyncer) SyncManifests(ctx context.Context, kubeconfig []byte, objects []*unstructured.Unstructured, talosVersion string) error {
	if len(objects) == 0 {
		return nil
	}

	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfig)
	if err != nil {
		return errors.Wrap(err, "decoding the kubeconfig of the workload cluster")
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return errors.Wrap(err, "building the client for the workload cluster")
	}

	logFunc := func(format string, args ...any) {
		s.log.Info(fmt.Sprintf(format, args...))
	}

	if supportsSSAManifestSync(talosVersion) {
		manager, err := ssa.NewManager(
			ctx,
			restConfig,
			constants.KubernetesFieldManagerName,
			constants.KubernetesInventoryNamespace,
			constants.KubernetesBootstrapManifestsInventoryName,
		)
		if err != nil {
			return errors.Wrap(err, "creating the inventory manager for the bootstrap manifests")
		}

		defer manager.Close()

		// ConfigMaps and Secrets are applied in their own stage, before the DaemonSet, so
		// checksum-linked manifests reconcile after their inputs, as upgrade-k8s does.
		changes, err := manager.Apply(ctx, objects, ssa.ApplyOptions{
			CustomStageKinds: map[schema.GroupKind]struct{}{
				schema.ParseGroupKind("ConfigMap"): {},
				schema.ParseGroupKind("Secret"):    {},
			},
		})
		if err != nil {
			return errors.Wrap(err, "syncing the bootstrap manifests with server-side apply")
		}

		// upgrade-k8s does not report the node upgraded until the applied manifests are
		// reconciled in the cluster (syncManifestsSSA of pkg/cluster/kubernetes in Talos
		// v1.14 waits with ssa.WaitOptions). Waiting here for a short while keeps the hook
		// from completing an upgrade whose kube-proxy DaemonSet is still the old one: the
		// sync is reported as pending and Cluster API calls the hook again, where the apply
		// is a no-op. The wait is shorter than DefaultRequestTimeout so one hook call stays
		// within its own deadline.
		applied := false

		for _, change := range changes {
			if change.Action != ssa.UnchangedAction && change.Action != ssa.SkippedAction {
				applied = true

				break
			}
		}

		if !applied {
			return nil
		}

		waitCtx, cancel := context.WithTimeout(ctx, manifestSyncWaitTimeout)
		defer cancel()

		if err := cli.Wait(waitCtx, changes, logFunc, manager, ssa.WaitOptions{
			Interval: manifestSyncWaitInterval,
			Timeout:  manifestSyncWaitTimeout,
			FailFast: true,
		}); err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return fmt.Errorf("%w: %s", errManifestsNotReconciled, err)
			}

			return errors.Wrap(err, "waiting for the synced bootstrap manifests of the workload cluster")
		}

		return nil
	}

	if err := manifests.SyncWithLog(ctx, objects, restConfig, false, logFunc); err != nil {
		return errors.Wrap(err, "syncing the bootstrap manifests")
	}

	return nil
}

// supportsSSAManifestSync reports whether the Talos version of a machine supports the
// inventory-based manifest sync. compatibility.ParseTalosVersion fails on an empty or
// unparsable tag: an empty TalosVersion of a TalosConfig means the latest Talos release, so
// the SSA path is used; an unparsable tag falls back to the legacy sync, which every Talos
// version supports.
func supportsSSAManifestSync(talosVersion string) bool {
	if talosVersion == "" {
		return true
	}

	version, err := compatibility.ParseTalosVersion(&apimachine.VersionInfo{Tag: talosVersion})
	if err != nil {
		return false
	}

	return version.SupportsSSAManifestSync()
}

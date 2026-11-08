// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"

	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/collections"

	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

func (r *TalosControlPlaneReconciler) upgradeControlPlane(
	ctx context.Context,
	cluster *clusterv1.Cluster,
	tcp *controlplanev1.TalosControlPlane,
	controlPlane *ControlPlane,
	machinesRequireUpgrade collections.Machines,
) (ctrl.Result, error) {
	logger := controlPlane.Logger()

	if tcp.Spec.RolloutStrategy == nil || tcp.Spec.RolloutStrategy.RollingUpdate == nil {
		return ctrl.Result{}, errors.New("rolloutStrategy is not set")
	}

	switch tcp.Spec.RolloutStrategy.Type {
	case controlplanev1.RollingUpdateStrategyType:
		// RolloutStrategy is currently defaulted and validated to be RollingUpdate
		maxNodes := tcp.Spec.GetReplicas() + int32(tcp.Spec.RolloutStrategy.RollingUpdate.MaxSurge.IntValue())
		if int32(controlPlane.Machines.Len()) < maxNodes {
			// scaleUp ensures that we don't continue scaling up while waiting for Machines to have NodeRefs
			return r.scaleUpControlPlane(ctx, cluster, tcp, controlPlane)
		}

		// Without the feature gate, which is off by default, a Machine that is not up to date is
		// rolled out exactly as before (inplace.go:44-53).
		if !feature.Gates.Enabled(feature.InPlaceUpdates) {
			return r.scaleDownControlPlane(ctx, cluster, tcp, controlPlane, machinesRequireUpgrade)
		}

		// The Machine that would be deleted by the rollout is the one the in-place update is tried
		// on first.
		deleteMachine, err := selectMachineForScaleDown(ctx, controlPlane, machinesRequireUpgrade)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to pick Control Plane Machine to scale down")
		}

		machineUpToDateResult, err := controlPlane.inPlaceUpdateResult(deleteMachine)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to compute the in-place update for Machine %s", klog.KObj(deleteMachine))
		}

		fallbackToScaleDown, err := r.tryInPlaceUpdate(ctx, controlPlane, deleteMachine, machineUpToDateResult)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to try an in-place update for Machine %s", klog.KObj(deleteMachine))
		}
		if !fallbackToScaleDown {
			// The UpdateInProgressAnnotation and the pending UpdateMachine hook are written: the
			// Machine is kept and the Machine controller performs the update, so no Machine is
			// deleted here.
			logger.Info("Machine is being updated in-place", "Machine", klog.KObj(deleteMachine))
			return ctrl.Result{}, nil
		}

		return r.scaleDownControlPlane(ctx, cluster, tcp, controlPlane, collections.Machines{deleteMachine.Name: deleteMachine})
	case controlplanev1.OnDeleteStrategyType:
		// nothing to do, scale up handler will take care of creating machines with the new spec
		return ctrl.Result{}, nil
	default:
		logger.Info("RolloutStrategy type is not set to RollingUpdateStrategyType, unable to determine the strategy for rolling out machines")
		return ctrl.Result{}, nil
	}
}

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// controlPlaneMachine is a Machine of the control plane above: the name of the metadata secret of an
// infrastructure machine is derived from its infrastructure reference.
func controlPlaneMachine(name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       clusterv1.MachineSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{Kind: "Metal3Machine", Name: name + "-7df9"}},
	}
}

// TestMatchesControlPlaneConfigMetadataDefaults covers the Metal3 e2e failure: the TalosConfig CRD
// fills in the metadataFrom defaults when a TalosConfig is stored, while the TalosControlPlane CRD
// does not, so a control plane config that sets metadataFrom.infrastructureMachine: {} reads back
// as a different spec than the TalosConfig it generated, and every Machine is rolled out forever.
func TestMatchesControlPlaneConfigMetadataDefaults(t *testing.T) {
	tcp := &controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane"},
		Spec: controlplanev1.TalosControlPlaneSpec{
			ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					GenerateType:  "controlplane",
					DataSecretKey: "userData",
					MetadataFrom: &cabptv1.MetadataFrom{
						InfrastructureMachine: &cabptv1.InfrastructureMachineMetadata{},
					},
				},
			},
		},
	}

	talosConfig := &cabptv1.TalosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane-abcde"},
		Spec: cabptv1.TalosConfigSpec{
			GenerateType:  "controlplane",
			DataSecretKey: "userData",
			MetadataFrom: &cabptv1.MetadataFrom{
				InfrastructureMachine: &cabptv1.InfrastructureMachineMetadata{
					DataKey:          "metaData",
					SecretNameSuffix: "-metadata",
				},
			},
		},
	}

	machine := controlPlaneMachine("test-control-plane-abcde")

	assert.True(t, MatchesControlPlaneConfig(map[string]*cabptv1.TalosConfig{machine.Name: talosConfig}, tcp)(machine),
		"a TalosConfig that only differs by the CRD-applied metadataFrom defaults must match the control plane config")
}

func TestMatchesControlPlaneConfigSecretRefDefaults(t *testing.T) {
	tcp := &controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane"},
		Spec: controlplanev1.TalosControlPlaneSpec{
			ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					GenerateType: "controlplane",
					MetadataFrom: &cabptv1.MetadataFrom{
						SecretRef: &cabptv1.MetadataSecretRef{Name: "test-metadata"},
					},
				},
			},
		},
	}

	talosConfig := &cabptv1.TalosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane-abcde"},
		Spec: cabptv1.TalosConfigSpec{
			GenerateType: "controlplane",
			MetadataFrom: &cabptv1.MetadataFrom{
				SecretRef: &cabptv1.MetadataSecretRef{Name: "test-metadata", Key: "metaData"},
			},
		},
	}

	machine := controlPlaneMachine("test-control-plane-abcde")

	assert.True(t, MatchesControlPlaneConfig(map[string]*cabptv1.TalosConfig{machine.Name: talosConfig}, tcp)(machine),
		"a TalosConfig whose secretRef key is only filled in by the CRD default must match")
}

func TestMatchesControlPlaneConfigStillDetectsRealDiff(t *testing.T) {
	tcp := &controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane"},
		Spec: controlplanev1.TalosControlPlaneSpec{
			ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					GenerateType: "controlplane",
					TalosVersion: "v1.14.0",
				},
			},
		},
	}

	talosConfig := &cabptv1.TalosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane-abcde"},
		Spec: cabptv1.TalosConfigSpec{
			GenerateType: "controlplane",
			TalosVersion: "v1.13.0",
		},
	}

	machine := controlPlaneMachine("test-control-plane-abcde")

	assert.False(t, MatchesControlPlaneConfig(map[string]*cabptv1.TalosConfig{machine.Name: talosConfig}, tcp)(machine),
		"a genuinely different control plane config must still roll the Machine out")
}

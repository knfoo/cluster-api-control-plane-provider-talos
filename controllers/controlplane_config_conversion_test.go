// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"k8s.io/apimachinery/pkg/runtime/schema"

	cpv1alpha3 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	cpv1beta1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
)

// TestControlPlaneConfigTemplateFieldsRoundTrip converts a TalosControlPlane whose controlPlaneConfig
// carries the template-rendering fields from the CABPT fork (metadataFrom, templateVariables,
// dataSecretKey; owner patch cacppt-3a04b57-v1beta2-and-cabpt-template.patch:931 wires the dependency)
// v1alpha3 -> v1beta1 -> v1alpha3 and checks the fields survive both directions.
func TestControlPlaneConfigTemplateFieldsRoundTrip(t *testing.T) {
	g := NewWithT(t)
	cpv1alpha3.SetAPIVersionGetter(func(gk schema.GroupKind) (string, error) {
		return gk.Group + "/v1beta2", nil
	})

	metadataFrom := &cabptv1.MetadataFrom{
		SecretRef: &cabptv1.MetadataSecretRef{
			Name: "controlplane-metadata",
			Key:  "metaData",
		},
		InfrastructureMachine: &cabptv1.InfrastructureMachineMetadata{
			SecretNameSuffix: "-metadata",
			DataKey:          "metaData",
		},
	}
	templateVariables := map[string]string{
		"hostname":        "control-plane-1",
		"providerID":      "metal3://00000000-0000-0000-0000-000000000001",
		"externalAddress": "10.0.0.12",
	}

	src := &cpv1alpha3.TalosControlPlane{
		Spec: cpv1alpha3.TalosControlPlaneSpec{
			ControlPlaneConfig: cpv1alpha3.ControlPlaneConfig{
				InitConfig: cabptv1.TalosConfigSpec{
					GenerateType:      "controlplane",
					MetadataFrom:      metadataFrom,
					TemplateVariables: templateVariables,
					DataSecretKey:     "userData",
					StrategicPatches:  []string{"{{ get \"hostname\" }}"},
				},
				ControlPlaneConfig: cabptv1.TalosConfigSpec{
					GenerateType:      "controlplane",
					MetadataFrom:      metadataFrom,
					TemplateVariables: templateVariables,
					DataSecretKey:     "userData",
					StrategicPatches:  []string{"{{ get \"hostname\" }}"},
				},
			},
		},
	}

	hub := &cpv1beta1.TalosControlPlane{}
	g.Expect(src.ConvertTo(hub)).To(Succeed())

	for _, config := range []cabptv1.TalosConfigSpec{hub.Spec.ControlPlaneConfig.InitConfig, hub.Spec.ControlPlaneConfig.ControlPlaneConfig} {
		g.Expect(config.MetadataFrom).ToNot(BeNil())
		g.Expect(config.MetadataFrom.SecretRef).ToNot(BeNil())
		g.Expect(config.MetadataFrom.SecretRef.Name).To(Equal("controlplane-metadata"))
		g.Expect(config.MetadataFrom.InfrastructureMachine).ToNot(BeNil())
		g.Expect(config.MetadataFrom.InfrastructureMachine.SecretNameSuffix).To(Equal("-metadata"))
		g.Expect(config.TemplateVariables).To(Equal(templateVariables))
		g.Expect(config.DataSecretKey).To(Equal("userData"))
		g.Expect(config.StrategicPatches).To(Equal([]string{"{{ get \"hostname\" }}"}))
	}

	back := &cpv1alpha3.TalosControlPlane{}
	g.Expect(back.ConvertFrom(hub)).To(Succeed())
	g.Expect(cmp.Diff(src.Spec.ControlPlaneConfig, back.Spec.ControlPlaneConfig)).To(BeEmpty())
}

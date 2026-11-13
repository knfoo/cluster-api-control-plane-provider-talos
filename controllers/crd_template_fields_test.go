// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// moduleRoot walks up from the test's working directory (the package directory) to the module root,
// so the generated manifests can be read from any test invocation.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

// controlPlaneCRDs are the control-plane CRDs regenerated against the CABPT fork, whose
// TalosConfigSpec carries the template-rendering fields. The dependency is switched to that fork by
// the replace in go.mod, mirroring patch:931 of cacppt-3a04b57-v1beta2-and-cabpt-template.patch; the
// fields reach TalosControlPlane and TalosControlPlaneTemplate through the embedded spec, with no
// CACPPT API code change.
type controlPlaneCRD struct {
	path string
	kind string
	// property chain from the version schema down to the ControlPlaneConfig struct.
	configPath []string
}

var controlPlaneCRDs = []controlPlaneCRD{
	{
		path:       "config/crd/bases/controlplane.cluster.x-k8s.io_taloscontrolplanes.yaml",
		kind:       "TalosControlPlane",
		configPath: []string{"spec", "controlPlaneConfig"},
	},
	{
		path:       "config/crd/bases/controlplane.cluster.x-k8s.io_taloscontrolplanetemplates.yaml",
		kind:       "TalosControlPlaneTemplate",
		configPath: []string{"spec", "template", "spec", "controlPlaneConfig"},
	},
}

// TestTalosControlPlaneCRDCarriesTemplateFields checks that spec.controlPlaneConfig.controlplane and
// spec.controlPlaneConfig.init accept metadataFrom, templateVariables and dataSecretKey in every served
// apiVersion of TalosControlPlane and TalosControlPlaneTemplate, and that the fields stay optional so
// a control plane that sets none of them is validated exactly as before.
func TestTalosControlPlaneCRDCarriesTemplateFields(t *testing.T) {
	g := NewWithT(t)

	root, err := moduleRoot()
	g.Expect(err).ToNot(HaveOccurred())

	for _, manifest := range controlPlaneCRDs {
		raw, err := os.ReadFile(filepath.Join(root, manifest.path))
		g.Expect(err).ToNot(HaveOccurred(), "manifest %s", manifest.path)

		crd := apiextensionsv1.CustomResourceDefinition{}
		g.Expect(yaml.Unmarshal(raw, &crd)).To(Succeed(), "manifest %s", manifest.path)
		g.Expect(crd.Spec.Names.Kind).To(Equal(manifest.kind), "manifest %s", manifest.path)
		g.Expect(crd.Spec.Versions).ToNot(BeEmpty(), "manifest %s", manifest.path)

		for _, version := range crd.Spec.Versions {
			controlPlaneConfig := version.Schema.OpenAPIV3Schema
			for _, key := range manifest.configPath {
				g.Expect(controlPlaneConfig.Properties).To(HaveKey(key), "manifest %s, apiVersion %s", manifest.path, version.Name)
				child := controlPlaneConfig.Properties[key]
				controlPlaneConfig = &child
			}

			for _, configKey := range []string{"controlplane", "init"} {
				g.Expect(controlPlaneConfig.Properties).To(HaveKey(configKey), "manifest %s, apiVersion %s", manifest.path, version.Name)
				talosConfig := controlPlaneConfig.Properties[configKey]

				g.Expect(talosConfig.Properties).To(HaveKey("metadataFrom"), "manifest %s, apiVersion %s, %s", manifest.path, version.Name, configKey)
				g.Expect(talosConfig.Properties).To(HaveKey("templateVariables"), "manifest %s, apiVersion %s, %s", manifest.path, version.Name, configKey)
				g.Expect(talosConfig.Properties).To(HaveKey("dataSecretKey"), "manifest %s, apiVersion %s, %s", manifest.path, version.Name, configKey)

				metadataFrom := talosConfig.Properties["metadataFrom"]
				g.Expect(metadataFrom.Type).To(Equal("object"))
				g.Expect(metadataFrom.Properties).To(HaveKey("secretRef"))
				g.Expect(metadataFrom.Properties).To(HaveKey("infrastructureMachine"))

				templateVariables := talosConfig.Properties["templateVariables"]
				g.Expect(templateVariables.Type).To(Equal("object"))
				g.Expect(templateVariables.AdditionalProperties).ToNot(BeNil())
				g.Expect(templateVariables.AdditionalProperties.Schema).ToNot(BeNil())
				g.Expect(templateVariables.AdditionalProperties.Schema.Type).To(Equal("string"))
				g.Expect(talosConfig.Properties["dataSecretKey"].Type).To(Equal("string"))

				g.Expect(talosConfig.Required).ToNot(ContainElement("metadataFrom"))
				g.Expect(talosConfig.Required).ToNot(ContainElement("templateVariables"))
				g.Expect(talosConfig.Required).ToNot(ContainElement("dataSecretKey"))
			}
		}
	}
}

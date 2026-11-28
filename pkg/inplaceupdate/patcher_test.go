// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"encoding/json"
	"testing"

	evanphx "github.com/evanphx/json-patch/v5"
	. "github.com/onsi/gomega"
)

// TestJSONPatchBetween checks the patch the controller reports when the objects of a hook
// request still differ after the extension patches were applied: applying it to the current
// object yields the desired one.
func TestJSONPatchBetween(t *testing.T) {
	g := NewWithT(t)

	current := map[string]any{"spec": map[string]any{"data": "current config", "talosVersion": "v1.14.5"}}
	desired := map[string]any{"spec": map[string]any{"data": "desired config", "extra": "added"}}

	patch, err := JSONPatchBetween(current, desired)
	g.Expect(err).NotTo(HaveOccurred())

	ops := []jsonPatchOp{}
	g.Expect(json.Unmarshal(patch, &ops)).To(Succeed())
	g.Expect(ops).NotTo(BeEmpty())

	rfcPatch, err := evanphx.DecodePatch(patch)
	g.Expect(err).NotTo(HaveOccurred())

	currentJSON, err := json.Marshal(current)
	g.Expect(err).NotTo(HaveOccurred())

	patched, err := rfcPatch.Apply(currentJSON)
	g.Expect(err).NotTo(HaveOccurred())

	var patchedObject any
	g.Expect(json.Unmarshal(patched, &patchedObject)).To(Succeed())
	g.Expect(equalJSON(patchedObject, desired)).To(BeTrue())

	noChange, err := JSONPatchBetween(current, current)
	g.Expect(err).NotTo(HaveOccurred())

	ops = []jsonPatchOp{}
	g.Expect(json.Unmarshal(noChange, &ops)).To(Succeed())
	g.Expect(ops).To(BeEmpty())
}

// TestObjectPatch checks the patch built for a TalosConfig or template spec: one operation
// per changed top-level field of the spec, under the given path.
func TestObjectPatch(t *testing.T) {
	g := NewWithT(t)

	current := map[string]any{"data": "current config", "dropped": "gone"}
	desired := map[string]any{"data": "desired config", "added": "new"}

	patch, err := objectPatch("/spec", current, desired)
	g.Expect(err).NotTo(HaveOccurred())

	ops := []jsonPatchOp{}
	g.Expect(json.Unmarshal(patch, &ops)).To(Succeed())
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "replace", Path: "/spec/data", Value: "desired config"}))
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "remove", Path: "/spec/dropped"}))
	g.Expect(ops).To(ContainElement(jsonPatchOp{Op: "add", Path: "/spec/added", Value: "new"}))
}

// TestConfigDiffPairsDocumentsByKindAndName checks how the documents of a machine configuration
// are matched: the v1alpha1 document with the machine and cluster settings pairs with the
// v1alpha1 document, every other document with the document of its kind and name, so the order
// of the documents is not a change and a document present on one side only is an add or a
// remove operation rather than an error.
func TestConfigDiffPairsDocumentsByKindAndName(t *testing.T) {
	g := NewWithT(t)

	current := `version: v1alpha1
machine:
  network:
    hostname: control-plane-1
cluster:
  token: bootstrap-token
---
apiVersion: v1alpha1
kind: VolumeConfig
name: nvme
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth0
`

	reordered := `apiVersion: v1alpha1
kind: LinkConfig
name: eth0
---
version: v1alpha1
machine:
  network:
    hostname: control-plane-1
cluster:
  token: bootstrap-token
---
apiVersion: v1alpha1
kind: VolumeConfig
name: nvme
`

	ops, err := configDiff([]byte(current), []byte(reordered))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ops).To(BeEmpty(), "documents paired by kind and name are not changed by their order")

	// A link configuration is replaced by another one, so the documents differ in count: each
	// document on one side only is an operation, not a reason to refuse the diff.
	desired := `version: v1alpha1
machine:
  network:
    hostname: control-plane-1
cluster:
  token: bootstrap-token
---
apiVersion: v1alpha1
kind: VolumeConfig
name: nvme
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth1
`

	ops, err = configDiff([]byte(current), []byte(desired))
	g.Expect(err).NotTo(HaveOccurred())

	adds := []jsonPatchOp{}
	removes := []jsonPatchOp{}

	for _, op := range ops {
		switch op.Op {
		case "add":
			adds = append(adds, op)
		case "remove":
			removes = append(removes, op)
		}
	}

	g.Expect(adds).To(HaveLen(1))
	g.Expect(adds[0].Path).To(Equal("/document/LinkConfig/eth1"))
	g.Expect(removes).To(HaveLen(1))
	g.Expect(removes[0].Path).To(Equal("/document/LinkConfig/eth0"))
}

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package inplaceupdate

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

// TestApplyJSONPatch checks the RFC 6902 patch the hooks return, applied to the objects of
// a hook request: replace, add and remove, with the list indices machine configuration
// values such as machine/files use.
func TestApplyJSONPatch(t *testing.T) {
	g := NewWithT(t)

	object := map[string]any{
		"spec": map[string]any{
			"data":    "current config",
			"files":   []any{map[string]any{"path": "/etc/inplace/current"}},
			"removed": "gone",
		},
	}

	patch, err := json.Marshal([]jsonPatchOp{
		{Op: "replace", Path: "/spec/data", Value: "desired config"},
		{Op: "add", Path: "/spec/files/1", Value: map[string]any{"path": "/etc/inplace/added"}},
		{Op: "remove", Path: "/spec/removed"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(ApplyJSONPatch(object, patch)).To(Succeed())

	spec := object["spec"].(map[string]any)
	g.Expect(spec["data"]).To(Equal("desired config"))
	g.Expect(spec["files"]).To(ContainElement(map[string]any{"path": "/etc/inplace/added"}))
	g.Expect(spec).NotTo(HaveKey("removed"))

	g.Expect(ApplyJSONPatch(object, []byte(`[{"op":"replace","path":"/spec/missing"}]`))).ToNot(Succeed())
}

// TestApplyJSONMergePatch checks the RFC 7396 merge patch of a hook response: a key set to
// null is deleted, nested objects are merged.
func TestApplyJSONMergePatch(t *testing.T) {
	g := NewWithT(t)

	object := map[string]any{
		"spec": map[string]any{
			"data":     "current config",
			"obsolete": "gone",
		},
	}

	g.Expect(ApplyJSONMergePatch(object, []byte(`{"spec":{"data":"desired config","obsolete":null}}`))).To(Succeed())

	spec := object["spec"].(map[string]any)
	g.Expect(spec["data"]).To(Equal("desired config"))
	g.Expect(spec).NotTo(HaveKey("obsolete"))
}

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

	g.Expect(ApplyJSONPatch(current, patch)).To(Succeed())
	g.Expect(equalJSON(current, desired)).To(BeTrue())

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

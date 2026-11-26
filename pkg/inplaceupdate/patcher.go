// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// RFC 6902 JSON patch helpers for the in-place update hooks: applying the patches
// returned by an extension to the objects of a hook request, diffing a Talos machine
// configuration, and rendering the patch the extension returns to Cluster API.
package inplaceupdate

import (
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	evanphx "github.com/evanphx/json-patch/v5"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
)

// jsonPatchOp is a single RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// patchOperation converts a jsonpatch/v2 operation to the patch operations used by the hooks.
func patchOperation(op jsonpatch.Operation) jsonPatchOp {
	return jsonPatchOp{Op: op.Operation, Path: op.Path, Value: op.Value}
}

// ApplyJSONPatch applies an RFC 6902 patch document to a decoded object. The control plane
// controller uses it to apply the patches a CanUpdateMachine extension returns to the objects
// of its request, the way KubeadmControlPlane's patch helper does: decode the patch with
// github.com/evanphx/json-patch/v5 and apply it to the marshalled object.
func ApplyJSONPatch(object map[string]any, patch []byte) error {
	current, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("decoding object: %w", err)
	}

	rfcPatch, err := evanphx.DecodePatch(patch)
	if err != nil {
		return fmt.Errorf("decoding JSON patch: %w", err)
	}

	patched, err := rfcPatch.Apply(current)
	if err != nil {
		return fmt.Errorf("applying JSON patch: %w", err)
	}

	return loadPatchedObject(object, patched)
}

// ApplyJSONMergePatch applies an RFC 7396 JSON merge patch to a decoded object, as the patch
// helper of KubeadmControlPlane does with jsonpatch.MergePatch.
func ApplyJSONMergePatch(object map[string]any, patch []byte) error {
	current, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("decoding object: %w", err)
	}

	patched, err := evanphx.MergePatch(current, patch)
	if err != nil {
		return fmt.Errorf("applying JSON merge patch: %w", err)
	}

	return loadPatchedObject(object, patched)
}

// JSONPatchBetween returns the RFC 6902 operations that turn current into desired, as a patch
// document. The controller uses it to report which parts of an object still differ after a
// CanUpdateMachine call. The patch is built with gomodules.xyz/jsonpatch/v2 CreatePatch, the
// way Cluster API's in-place update extension builds its response patches.
func JSONPatchBetween(current, desired any) ([]byte, error) {
	currentJSON, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("decoding current object: %w", err)
	}

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return nil, fmt.Errorf("decoding desired object: %w", err)
	}

	ops, err := jsonpatch.CreatePatch(currentJSON, desiredJSON)
	if err != nil {
		return nil, fmt.Errorf("computing JSON patch: %w", err)
	}

	patch := make([]jsonPatchOp, 0, len(ops))
	for _, op := range ops {
		patch = append(patch, patchOperation(op))
	}

	return json.Marshal(patch)
}

// objectPatch returns the RFC 6902 patch that turns current into desired, one operation per
// top-level field. The hook patches the TalosConfig and TalosConfigTemplate specs as whole
// fields: a changed field is replaced with the desired value, a missing field is removed and a
// new field is added.
func objectPatch(prefix string, current, desired map[string]any) ([]byte, error) {
	ops := []jsonPatchOp{}

	for field, value := range desired {
		if _, ok := current[field]; ok {
			ops = append(ops, jsonPatchOp{Op: "replace", Path: prefix + "/" + field, Value: value})

			continue
		}

		ops = append(ops, jsonPatchOp{Op: "add", Path: prefix + "/" + field, Value: value})
	}

	for field := range current {
		if _, ok := desired[field]; !ok {
			ops = append(ops, jsonPatchOp{Op: "remove", Path: prefix + "/" + field})
		}
	}

	return json.Marshal(ops)
}

// loadPatchedObject writes the patched JSON back into the object of the hook request. It mutates
// the map in place so the controller sees the updated object.
func loadPatchedObject(object map[string]any, patched []byte) error {
	var result map[string]any
	if err := json.Unmarshal(patched, &result); err != nil {
		return fmt.Errorf("decoding patched object: %w", err)
	}

	for key := range object {
		delete(object, key)
	}

	for key, value := range result {
		object[key] = value
	}

	return nil
}

// jsonPatch renders operations as a JSON patch document.
func jsonPatch(ops []jsonPatchOp) ([]byte, error) {
	return json.Marshal(ops)
}

// documents splits a machine configuration into its YAML documents. Talos accepts
// multi-document configurations: a configuration supplied in TalosConfigSpec.Data is a
// single document, a generated one carries the machine and cluster settings in the first
// document and the extension documents after it.
func documents(data []byte) ([]map[string]any, error) {
	chunks := []string{}
	current := []string{}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "---" {
			chunks = append(chunks, strings.Join(current, "\n"))
			current = nil

			continue
		}

		current = append(current, line)
	}

	chunks = append(chunks, strings.Join(current, "\n"))

	docs := []map[string]any{}

	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}

		doc := map[string]any{}
		if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
			return nil, fmt.Errorf("decoding machine configuration document: %w", err)
		}

		docs = append(docs, doc)
	}

	if len(docs) == 0 {
		return nil, fmt.Errorf("machine configuration has no documents")
	}

	return docs, nil
}

// configDiff computes the operations that turn one machine configuration into another. Talos
// machine configurations are multi-document: the v1alpha1 document carries the machine and
// cluster settings, and the documents after it each have a kind (config.Document Kind(),
// github.com/siderolabs/talos/pkg/machinery/config/config) and, when they are named documents
// (config.NamedDocument Name()), a name. Documents are paired by that identity rather than by
// position, so a reordered configuration is no change and a document present on one side only
// is an add or a remove operation. Only the v1alpha1 document addresses the paths Talos keys by
// machine and cluster; the operations of the other documents are reported under their kind and
// name, which the reboot guard does not match.
func configDiff(current, desired []byte) ([]jsonPatchOp, error) {
	currentDocs, err := documents(current)
	if err != nil {
		return nil, err
	}

	desiredDocs, err := documents(desired)
	if err != nil {
		return nil, err
	}

	currentByKey := make(map[string]map[string]any, len(currentDocs))
	for _, doc := range currentDocs {
		currentByKey[documentKey(doc)] = doc
	}

	desiredByKey := make(map[string]map[string]any, len(desiredDocs))
	for _, doc := range desiredDocs {
		desiredByKey[documentKey(doc)] = doc
	}

	ops := []jsonPatchOp{}

	for _, doc := range currentDocs {
		key := documentKey(doc)

		if desiredDoc, ok := desiredByKey[key]; ok {
			docOps, err := documentPatch(key, doc, desiredDoc)
			if err != nil {
				return nil, err
			}

			ops = append(ops, docOps...)

			continue
		}

		ops = append(ops, jsonPatchOp{Op: "remove", Path: documentPath(key)})
	}

	for _, doc := range desiredDocs {
		key := documentKey(doc)

		if _, ok := currentByKey[key]; !ok {
			ops = append(ops, jsonPatchOp{Op: "add", Path: documentPath(key), Value: doc})
		}
	}

	return ops, nil
}

// documentKey identifies a machine configuration document: the v1alpha1 document, the only one
// without a kind, stands for itself, every other document is identified by its kind and name.
func documentKey(doc map[string]any) string {
	kind, _ := doc["kind"].(string)
	if kind == "" {
		return ""
	}

	name, _ := doc["name"].(string)

	return kind + "/" + name
}

// documentPath is the path the operations of a machine configuration document are reported
// under. The v1alpha1 document keeps the paths Talos keys its settings by.
func documentPath(key string) string {
	if key == "" {
		return ""
	}

	return "/document/" + key
}

// documentPatch computes the operations that turn one machine configuration document into
// another, prefixing the paths of a document other than the v1alpha1 one with its identity.
func documentPatch(key string, current, desired map[string]any) ([]jsonPatchOp, error) {
	prefix := documentPath(key)

	currentJSON, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("decoding machine configuration document: %w", err)
	}

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return nil, fmt.Errorf("encoding desired machine configuration document: %w", err)
	}

	docOps, err := jsonpatch.CreatePatch(currentJSON, desiredJSON)
	if err != nil {
		return nil, fmt.Errorf("computing machine configuration patch: %w", err)
	}

	ops := make([]jsonPatchOp, 0, len(docOps))

	for _, op := range docOps {
		if prefix != "" {
			op.Path = prefix + op.Path
		}

		ops = append(ops, patchOperation(op))
	}

	return ops, nil
}

// equalJSON compares two decoded YAML values by their JSON form.
func equalJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}

	right, err := json.Marshal(b)
	if err != nil {
		return false
	}

	return string(left) == string(right)
}

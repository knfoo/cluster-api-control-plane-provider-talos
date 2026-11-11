// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// RFC 6902 JSON patch helpers: computing the diff between two Talos machine
// configurations, applying the configuration patches stored in a TalosConfig,
// and rendering the patch the extension returns to Cluster API.
//
// The proposal (docs/proposals/in-place-updates.md, section 6 row L4) asks for a
// dependency-free component, so the diff and the patch application are written on
// top of the standard library and sigs.k8s.io/yaml instead of importing
// github.com/siderolabs/talos/pkg/machinery/config/configdiff and
// .../config/configpatcher, which would add four new modules to go.mod.
package inplaceupdate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// jsonPatchOp is a single RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// ApplyJSONPatch applies an RFC 6902 patch document to a decoded object. The control plane
// controller uses it to apply the patches a CanUpdateMachine extension returns to the objects
// of its request, the way KubeadmControlPlane does with the patch helper in
// sigs.k8s.io/cluster-api/internal/util/patch (inplace_canupdatemachine.go:272-292).
func ApplyJSONPatch(object map[string]any, patch []byte) error {
	ops := []jsonPatchOp{}
	if err := json.Unmarshal(patch, &ops); err != nil {
		return fmt.Errorf("decoding JSON patch: %w", err)
	}

	for _, op := range ops {
		if err := applyOp(object, op); err != nil {
			return err
		}
	}

	return nil
}

// ApplyJSONMergePatch applies an RFC 7396 JSON merge patch to a decoded object.
func ApplyJSONMergePatch(object map[string]any, patch []byte) error {
	merge := map[string]any{}
	if err := json.Unmarshal(patch, &merge); err != nil {
		return fmt.Errorf("decoding JSON merge patch: %w", err)
	}

	mergeInto(object, merge)

	return nil
}

// mergeInto writes merge into object, deleting the keys whose merge value is null, as RFC 7396
// defines the merge patch.
func mergeInto(object, merge map[string]any) {
	for key, value := range merge {
		if value == nil {
			delete(object, key)

			continue
		}

		if nested, ok := object[key].(map[string]any); ok {
			if mergeNested, ok := value.(map[string]any); ok {
				mergeInto(nested, mergeNested)

				continue
			}
		}

		object[key] = value
	}
}

// JSONPatchBetween returns the RFC 6902 operations that turn current into desired, as a patch
// document. The controller uses it to report which parts of an object still differ after a
// CanUpdateMachine call.
func JSONPatchBetween(current, desired any) ([]byte, error) {
	return jsonPatch(diffValue("", current, desired))
}

// jsonPatch renders operations as a JSON patch document.
func jsonPatch(ops []jsonPatchOp) ([]byte, error) {
	patch, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("encoding JSON patch: %w", err)
	}

	return patch, nil
}

// documents splits a machine configuration into its YAML documents. Talos accepts
// multi-document configurations; the extension handles a single document, which is
// what the bootstrap provider writes into TalosConfigSpec.Data.
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

// machineConfigDocument returns the single machine configuration document of a
// configuration, or an error if the configuration is not a single document.
func machineConfigDocument(data []byte) (map[string]any, error) {
	docs, err := documents(data)
	if err != nil {
		return nil, err
	}

	if len(docs) != 1 {
		return nil, fmt.Errorf("machine configuration has %d documents, the extension handles only one", len(docs))
	}

	return docs[0], nil
}

// applyConfigPatches applies the RFC 6902 patches of a TalosConfig spec to the raw
// machine configuration and returns the resulting configuration bytes.
func applyConfigPatches(data []byte, ops []jsonPatchOp) ([]byte, error) {
	if len(ops) == 0 {
		return data, nil
	}

	doc, err := machineConfigDocument(data)
	if err != nil {
		return nil, err
	}

	for _, op := range ops {
		if err := applyOp(doc, op); err != nil {
			return nil, err
		}
	}

	patched, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encoding patched machine configuration: %w", err)
	}

	return patched, nil
}

// applyOp applies one RFC 6902 operation to a decoded document. Machine
// configuration values are often lists (machine/files, kubelet args), so array
// indices in a patch path are supported.
func applyOp(doc map[string]any, op jsonPatchOp) error {
	path, err := patchPath(op.Path)
	if err != nil {
		return err
	}

	if len(path) == 0 {
		return fmt.Errorf("operation %q on the root of a machine configuration is not supported", op.Op)
	}

	// Walk to the container that holds the last path segment. parent and parentKey
	// locate that container in its own parent: when the container is a list, adding or
	// removing an element can allocate a new slice, which has to replace the old one in
	// the document.
	var container any = doc
	var parent any
	var parentKey string

	for _, segment := range path[:len(path)-1] {
		next, err := child(container, segment)
		if err != nil {
			return err
		}

		parent, parentKey, container = container, segment, next
	}

	key := path[len(path)-1]

	switch op.Op {
	case "add":
		updated, err := insertIn(container, key, op.Value)
		if err != nil {
			return err
		}

		return store(parent, parentKey, updated)
	case "replace":
		return replaceIn(container, key, op.Value)
	case "remove":
		updated, err := removeIn(container, key)
		if err != nil {
			return err
		}

		return store(parent, parentKey, updated)
	default:
		return fmt.Errorf("unsupported patch operation %q at %q", op.Op, op.Path)
	}
}

// patchPath splits an RFC 6902 path into unescaped segments.
func patchPath(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("patch path %q must start with /", path)
	}

	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, segment := range segments {
		segments[i] = strings.ReplaceAll(segment, "~1", "/")
		segments[i] = strings.ReplaceAll(segment, "~0", "~")
	}

	return segments, nil
}

// child returns the value a path segment addresses in an object or a list.
func child(container any, segment string) (any, error) {
	switch current := container.(type) {
	case map[string]any:
		value, ok := current[segment]
		if !ok {
			return nil, fmt.Errorf("path segment %q does not exist", segment)
		}

		return value, nil
	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(current) {
			return nil, fmt.Errorf("path segment %q is not an index in the list at this position", segment)
		}

		return current[index], nil
	default:
		return nil, fmt.Errorf("path segment %q is inside a value that is neither an object nor a list", segment)
	}
}

// store writes a container back into its parent, which is needed after a list changed
// its length.
func store(parent any, key string, value any) error {
	if parent == nil {
		return nil
	}

	switch current := parent.(type) {
	case map[string]any:
		current[key] = value

		return nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= len(current) {
			return fmt.Errorf("cannot store the patched list at index %q", key)
		}

		current[index] = value

		return nil
	default:
		return fmt.Errorf("cannot store the patched list under %q", key)
	}
}

// insertIn adds a value to an object, or inserts it into a list at the given index, as
// RFC 6902 defines the "add" operation for arrays.
func insertIn(container any, key string, value any) (any, error) {
	switch current := container.(type) {
	case map[string]any:
		current[key] = value

		return current, nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index > len(current) {
			return nil, fmt.Errorf("patch index %q is not valid for the list at this position", key)
		}

		return append(current[:index], append([]any{value}, current[index:]...)...), nil
	default:
		return nil, fmt.Errorf("cannot add %q to a value that is neither an object nor a list", key)
	}
}

// replaceIn overwrites the value an object key or a list index holds.
func replaceIn(container any, key string, value any) error {
	switch current := container.(type) {
	case map[string]any:
		current[key] = value

		return nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= len(current) {
			return fmt.Errorf("patch index %q does not exist in the list at this position", key)
		}

		current[index] = value

		return nil
	default:
		return fmt.Errorf("cannot replace %q in a value that is neither an object nor a list", key)
	}
}

// removeIn deletes the value an object key or a list index holds and returns the
// container, which may be a shorter list.
func removeIn(container any, key string) (any, error) {
	switch current := container.(type) {
	case map[string]any:
		if _, ok := current[key]; !ok {
			return nil, fmt.Errorf("path segment %q does not exist", key)
		}

		delete(current, key)

		return current, nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= len(current) {
			return nil, fmt.Errorf("patch index %q does not exist in the list at this position", key)
		}

		return append(current[:index], current[index+1:]...), nil
	default:
		return nil, fmt.Errorf("cannot remove %q from a value that is neither an object nor a list", key)
	}
}

// diffValue computes the operations that turn current into desired.
func diffValue(path string, current, desired any) []jsonPatchOp {
	if equalJSON(current, desired) {
		return nil
	}

	currentMap, currentIsMap := current.(map[string]any)
	desiredMap, desiredIsMap := desired.(map[string]any)

	if currentIsMap && desiredIsMap {
		ops := []jsonPatchOp{}

		for key, value := range desiredMap {
			if existing, ok := currentMap[key]; ok {
				ops = append(ops, diffValue(path+"/"+key, existing, value)...)

				continue
			}

			ops = append(ops, jsonPatchOp{Op: "add", Path: path + "/" + key, Value: value})
		}

		for key := range currentMap {
			if _, ok := desiredMap[key]; !ok {
				ops = append(ops, jsonPatchOp{Op: "remove", Path: path + "/" + key})
			}
		}

		return ops
	}

	return []jsonPatchOp{{Op: "replace", Path: path, Value: desired}}
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

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Decoding of the objects a hook request carries: the machine configuration spec of a
// TalosConfig or TalosConfigTemplate, the infrastructure machine spec, and the patch the
// extension returns for a bootstrap configuration object.
package inplaceupdate

import (
	"encoding/json"

	"github.com/pkg/errors"
	cabptv1beta1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// configObject decodes a bootstrap configuration object of a hook request. The Runtime SDK
// passes the objects of a request as unstructured objects, either as raw JSON or as a decoded
// map, so both forms are read here.
func configObject(raw runtime.RawExtension) (map[string]any, error) {
	data := raw.Raw

	if data == nil {
		if raw.Object == nil {
			return nil, errors.New("the request has no bootstrap configuration object")
		}

		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	if len(data) == 0 {
		return nil, errors.New("the request has no bootstrap configuration object")
	}

	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, errors.Wrap(err, "decoding the bootstrap configuration object of the request")
	}

	return object, nil
}

// configKind returns the kind of a bootstrap configuration object of a hook request.
func configKind(raw runtime.RawExtension) string {
	object, err := configObject(raw)
	if err != nil {
		return ""
	}

	kind, _ := object["kind"].(string)

	return kind
}

// configSpec returns the machine configuration spec of a bootstrap configuration object of a
// hook request and the JSON path of that spec inside the object: spec of a TalosConfig,
// spec.template.spec of a TalosConfigTemplate. The machine configuration of a TalosConfig
// rollout and of a MachineSet rollout is the same TalosConfigSpec.
func configSpec(raw runtime.RawExtension) (map[string]any, string, error) {
	object, err := configObject(raw)
	if err != nil {
		return nil, "", err
	}

	switch kind, _ := object["kind"].(string); kind {
	case "TalosConfig":
		spec, ok := object["spec"].(map[string]any)
		if !ok {
			return nil, "", errors.New("the TalosConfig of the request has no spec")
		}

		return spec, "/spec", nil
	case "TalosConfigTemplate":
		spec, found, err := unstructured.NestedMap(object, "spec", "template", "spec")
		if err != nil || !found {
			return nil, "", errors.New("the TalosConfigTemplate of the request has no spec.template.spec")
		}

		return spec, "/spec/template/spec", nil
	default:
		return nil, "", errors.Errorf("the bootstrap configuration of the request is a %q, not a TalosConfig or a TalosConfigTemplate", kind)
	}
}

// talosConfigSpec reads the machine configuration spec of a TalosConfig or TalosConfigTemplate
// object of a hook request. The spec is read as the bootstrap provider's v1beta1 type, which is
// the one pkg/machineconfig generates and patches machine configurations with; the TalosConfig
// of a request is served in its v1alpha3 API, whose spec has the same fields.
func talosConfigSpec(raw runtime.RawExtension) (cabptv1beta1.TalosConfigSpec, error) {
	spec, _, err := configSpec(raw)
	if err != nil {
		return cabptv1beta1.TalosConfigSpec{}, err
	}

	data, err := json.Marshal(spec)
	if err != nil {
		return cabptv1beta1.TalosConfigSpec{}, errors.WithStack(err)
	}

	talosSpec := cabptv1beta1.TalosConfigSpec{}
	if err := json.Unmarshal(data, &talosSpec); err != nil {
		return cabptv1beta1.TalosConfigSpec{}, errors.Wrap(err, "decoding the machine configuration spec of the request")
	}

	return talosSpec, nil
}

// configPatch computes the JSON patch that turns the current bootstrap configuration object of
// a request into the desired one. Only the machine configuration spec is patched, as the hook
// contract requires and as the controllers restrict the patch to.
func configPatch(current, desired runtime.RawExtension) ([]byte, error) {
	currentSpec, path, err := configSpec(current)
	if err != nil {
		return nil, err
	}

	desiredSpec, _, err := configSpec(desired)
	if err != nil {
		return nil, err
	}

	return objectPatch(path, currentSpec, desiredSpec)
}

// infraSpec returns the spec of an infrastructure machine object.
func infraSpec(raw runtime.RawExtension) any {
	data := raw.Raw

	if data == nil && raw.Object != nil {
		data, _ = json.Marshal(raw.Object)
	}

	if len(data) == 0 {
		return nil
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil
	}

	return object["spec"]
}

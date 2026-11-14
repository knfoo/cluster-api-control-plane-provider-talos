// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Decoding of the objects a hook request carries: the TalosConfig spec, the
// infrastructure machine spec and the TalosConfig spec patch the response returns.
package inplaceupdate

import (
	"encoding/json"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	cabptv1beta1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
)

// talosConfigSpec reads the TalosConfig spec out of the bootstrap config object of a
// hook request.
func talosConfigSpec(raw runtime.RawExtension) (cabptv1.TalosConfigSpec, error) {
	data := raw.Raw

	if data == nil {
		if raw.Object == nil {
			return cabptv1.TalosConfigSpec{}, errors.New("the bootstrap configuration of the machine is not a TalosConfig object")
		}

		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return cabptv1.TalosConfigSpec{}, errors.WithStack(err)
		}
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return cabptv1.TalosConfigSpec{}, errors.Wrap(err, "decoding the bootstrap configuration of the machine")
	}

	if kind, _ := object["kind"].(string); kind != "TalosConfig" {
		return cabptv1.TalosConfigSpec{}, errors.Errorf("the bootstrap configuration of the machine is a %q, not a TalosConfig", kind)
	}

	specBytes, err := json.Marshal(object["spec"])
	if err != nil {
		return cabptv1.TalosConfigSpec{}, errors.WithStack(err)
	}

	spec := cabptv1.TalosConfigSpec{}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return cabptv1.TalosConfigSpec{}, errors.Wrap(err, "decoding the TalosConfig spec of the machine")
	}

	return spec, nil
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

// specPatch computes the JSON patch that turns the current TalosConfig object into
// the desired one. Only the spec is patched, as the hook contract requires.
func specPatch(current, desired runtime.RawExtension) ([]byte, error) {
	currentSpec, err := specObject(current)
	if err != nil {
		return nil, err
	}

	desiredSpec, err := specObject(desired)
	if err != nil {
		return nil, err
	}

	return jsonPatch(diffValue("/spec", currentSpec, desiredSpec))
}

func specObject(raw runtime.RawExtension) (map[string]any, error) {
	data := raw.Raw

	if data == nil {
		if raw.Object == nil {
			return nil, errors.New("the bootstrap configuration of the machine is not a TalosConfig object")
		}

		var err error

		data, err = json.Marshal(raw.Object)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, errors.Wrap(err, "decoding the bootstrap configuration of the machine")
	}

	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("the bootstrap configuration of the machine has no spec")
	}

	return spec, nil
}

// machineConfigPatches converts the RFC 6902 configPatches of the TalosConfig the request
// carries into the type the bootstrap provider's pkg/machineconfig.ApplyPatches takes. The
// two API versions define the same fields, as the provider's own generated conversion
// (api/v1alpha3/zz_generated.conversion.go:172-177) shows.
func machineConfigPatches(patches []cabptv1.ConfigPatches) []cabptv1beta1.ConfigPatches {
	out := make([]cabptv1beta1.ConfigPatches, 0, len(patches))

	for _, patch := range patches {
		out = append(out, cabptv1beta1.ConfigPatches{Op: patch.Op, Path: patch.Path, Value: patch.Value})
	}

	return out
}

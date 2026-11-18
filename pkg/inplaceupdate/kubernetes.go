// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Kubernetes version updates of one control-plane machine.
//
// The component order and the patches are the ones talosctl upgrade-k8s uses (Talos
// v1.14.5): the kube-apiserver, kube-controller-manager and kube-scheduler static pods in
// that order (.lap/sources/reference/files/talos_managed.go:150-154), kube-proxy next
// (.lap/sources/reference/files/talos_managed.go:156), and the kubelet last
// (.lap/sources/reference/files/talos_managed.go:161, .lap/sources/reference/files/kubelet.go).
// upgradeStaticPodOnNode applies one component at a time and waits for that pod to run the
// new version before patching the next one; here one machine configuration per hook call
// takes the node one component further. Syncing the bootstrap manifests afterwards
// (PerformManifestsSync, .lap/sources/reference/files/talos_managed.go:168) is deferred to
// a later stage.
package inplaceupdate

import (
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/stdpatches"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// kubernetesComponent is one control-plane component of a Talos machine.
type kubernetesComponent struct {
	// name is the component as talosctl upgrade-k8s names it.
	name string
	// defaultImage is the registry Talos enforces for the component, used when the node
	// does not report an image for it yet.
	defaultImage string
	// optional is true for a component a machine may not run at all, so an empty image
	// does not hold the update back.
	optional bool
	// contract guesses which machine configuration format the component lives in.
	contract func(container config.Container) *config.VersionContract
	// patch returns the machine configuration patch that sets the component image.
	patch func(versionContract *config.VersionContract, image string) ([]byte, error)
	// image reads the image the node runs for this component.
	image func(status NodeStatus) string
}

// kubernetesComponents is the order in which the control plane is upgraded in place.
var kubernetesComponents = []kubernetesComponent{
	{
		name:         "kube-apiserver",
		defaultImage: constants.KubernetesAPIServerImage,
		contract:     stdpatches.GuessVersionContractKubeAPIServer,
		patch:        stdpatches.WithKubeAPIServerImage,
		image:        func(status NodeStatus) string { return status.APIServer },
	},
	{
		name:         "kube-controller-manager",
		defaultImage: constants.KubernetesControllerManagerImage,
		contract:     stdpatches.GuessVersionContractKubeControllerManager,
		patch:        stdpatches.WithKubeControllerManagerImage,
		image:        func(status NodeStatus) string { return status.ControllerManager },
	},
	{
		name:         "kube-scheduler",
		defaultImage: constants.KubernetesSchedulerImage,
		contract:     stdpatches.GuessVersionContractKubeScheduler,
		patch:        stdpatches.WithKubeSchedulerImage,
		image:        func(status NodeStatus) string { return status.Scheduler },
	},
	{
		name:         "kube-proxy",
		defaultImage: constants.KubeProxyImage,
		optional:     true,
		contract:     stdpatches.GuessVersionContractKubeProxy,
		patch:        stdpatches.WithKubeProxyImage,
		image:        func(status NodeStatus) string { return status.Proxy },
	},
	{
		name:         "kubelet",
		defaultImage: constants.KubeletImage,
		contract:     stdpatches.GuessVersionContractKubelet,
		patch:        stdpatches.WithKubeletImage,
		image:        func(status NodeStatus) string { return status.Kubelet },
	},
}

// kubernetesVersionUpgrade reports whether the Kubernetes version of a Machine changes to
// the next minor version, the only Kubernetes version change handled in place. Talos
// supports the versions around the one it ships (compatibility.SupportedKubernetesVersions),
// so a jump of more than one minor version stays a rollout.
func kubernetesVersionUpgrade(current, desired string) bool {
	currentMajor, currentMinor, err := kubernetesMinor(current)
	if err != nil {
		return false
	}

	desiredMajor, desiredMinor, err := kubernetesMinor(desired)
	if err != nil {
		return false
	}

	return desiredMajor == currentMajor && desiredMinor == currentMinor+1
}

// kubernetesMinor splits a Kubernetes version into its major and minor number.
func kubernetesMinor(version string) (int, int, error) {
	parsed, err := compatibility.ParseKubernetesVersion(version)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "parsing Kubernetes version %q", version)
	}

	parts := strings.Split(parsed.String(), ".")
	if len(parts) < 2 {
		return 0, 0, errors.Errorf("Kubernetes version %q has no minor version", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, errors.Wrapf(err, "parsing the major version of %q", version)
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, errors.Wrapf(err, "parsing the minor version of %q", version)
	}

	return major, minor, nil
}

// componentsAtTarget reports whether every control-plane component the node runs already
// runs the target Kubernetes version. The kubelet image carries the -fat/-slim suffix, so
// only the version part is compared: compatibility.KubernetesVersionFromImageRef reports
// "1.34.0-fat" for ghcr.io/siderolabs/kubelet:v1.34.0-fat.
func componentsAtTarget(status NodeStatus, target string) bool {
	for _, component := range kubernetesComponents {
		if componentAtTarget(component.image(status), target) {
			continue
		}

		if component.optional && component.image(status) == "" {
			continue
		}

		return false
	}

	return true
}

// firstComponentNotAtTarget returns the next component of the upgrade order the node does
// not yet run at the target version, or "" when every component is there.
func firstComponentNotAtTarget(status NodeStatus, target string) string {
	for _, component := range kubernetesComponents {
		if !componentAtTarget(component.image(status), target) {
			return component.name
		}
	}

	return ""
}

// componentAtTarget reports whether an image reference runs the target Kubernetes version.
// An empty image is unknown and counts as not at the target, so it gets patched.
func componentAtTarget(image, target string) bool {
	if image == "" {
		return false
	}

	version, err := compatibility.KubernetesVersionFromImageRef(image)
	if err != nil {
		return false
	}

	targetVersion, err := compatibility.ParseKubernetesVersion(target)
	if err != nil {
		return false
	}

	base, _, _ := strings.Cut(version.String(), "-")

	return base == targetVersion.String()
}

// stagedKubernetesConfig patches the machine configuration for one step of the Kubernetes
// version upgrade: the components of kubernetesComponents up to and including the first one
// the node does not yet run at the target version. Components the node already upgraded are
// patched to the same image again, which is what makes the configuration stable across the
// hook calls of one update.
func stagedKubernetesConfig(data []byte, status NodeStatus, target string) ([]byte, error) {
	container, err := configpatcher.WithBytes(data).Config()
	if err != nil {
		return nil, errors.Wrap(err, "decoding the machine configuration")
	}

	patches := []configpatcher.Patch{}

	for _, component := range kubernetesComponents {
		current := component.image(status)

		patch, err := stdpatches.PreparePatch(component.patch(component.contract(container), componentImage(current, component.defaultImage, target)))
		if err != nil {
			return nil, errors.Wrapf(err, "patching the image of %s", component.name)
		}

		patches = append(patches, patch)

		if !componentAtTarget(current, target) {
			break
		}
	}

	output, err := configpatcher.Apply(configpatcher.WithBytes(data), patches)
	if err != nil {
		return nil, errors.Wrap(err, "applying the Kubernetes version patches to the machine configuration")
	}

	patched, err := output.Bytes()
	if err != nil {
		return nil, errors.Wrap(err, "encoding the machine configuration patched for the Kubernetes version update")
	}

	return patched, nil
}

// componentImage builds the image reference of a component for the target Kubernetes
// version, keeping the registry of the image the node runs and the kubelet image suffix
// (-fat, -slim), as talosctl upgrade-k8s does when it pre-pulls and patches the kubelet
// image (.lap/sources/reference/files/kubelet.go, extractKubeletVersionSuffix).
func componentImage(current, fallback, target string) string {
	registry := fallback

	if reference := strings.Split(strings.Split(current, "@")[0], ":"); len(reference) > 1 {
		registry = reference[0]
	}

	return registry + ":v" + strings.TrimPrefix(target, "v") + imageSuffix(current)
}

// imageSuffix returns the suffix of an image tag after the Kubernetes version, e.g. "-fat"
// for ghcr.io/siderolabs/kubelet:v1.34.0-fat.
func imageSuffix(image string) string {
	version, err := compatibility.KubernetesVersionFromImageRef(image)
	if err != nil {
		return ""
	}

	_, suffix, _ := strings.Cut(version.String(), "-")
	if suffix == "" {
		return ""
	}

	return "-" + suffix
}

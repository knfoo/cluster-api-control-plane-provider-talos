// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Kubernetes version updates of one control-plane machine.
//
// The component order and the patches are the ones talosctl upgrade-k8s uses (Talos
// v1.14.5): the kube-apiserver, kube-controller-manager and kube-scheduler static pods in
// that order, kube-proxy next, and the kubelet last. upgradeStaticPodOnNode applies one
// component at a time and waits for that pod to run the new version before patching the
// next one; here one machine configuration per hook call takes the node one component
// further. Syncing the bootstrap manifests afterwards, the PerformManifestsSync step of
// talosctl upgrade-k8s, is performed by pkg/inplaceupdate/manifestsync.go once the whole
// control plane is at the target version.
package inplaceupdate

import (
	"strings"

	"github.com/pkg/errors"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/stdpatches"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/util/version"
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

// upgradeComponents returns the Kubernetes components a node has to run at the target
// Kubernetes version. A node with control-plane components is upgraded in the order of
// kubernetesComponents; a node without them - a worker Machine of a MachineDeployment -
// only runs the kubelet, and talosctl upgrade-k8s upgrades exactly that component on such
// a node (upgradeKubeletOnNode).
func upgradeComponents(status NodeStatus) []kubernetesComponent {
	if status.hasControlPlane() {
		return kubernetesComponents
	}

	// The kubelet is the last component of kubernetesComponents, and a worker node has no
	// static pod to patch: only machine/kubelet/image changes.
	return kubernetesComponents[len(kubernetesComponents)-1:]
}

// kubernetesVersionUpgrade reports whether the Kubernetes version of a Machine changes to
// the next minor version, the only Kubernetes version change handled in place. Talos
// supports the versions around the one it ships (compatibility.SupportedKubernetesVersions),
// so a jump of more than one minor version stays a rollout.
func kubernetesVersionUpgrade(current, desired string) bool {
	currentVersion, err := version.Parse(current)
	if err != nil {
		return false
	}

	desiredVersion, err := version.Parse(desired)
	if err != nil {
		return false
	}

	return currentVersion.Major() == desiredVersion.Major() && desiredVersion.Minor() == currentVersion.Minor()+1
}

// componentsAtTarget reports whether every Kubernetes component the node runs already
// runs the target Kubernetes version. The kubelet image carries the -fat/-slim suffix, so
// only the version part is compared: compatibility.KubernetesVersionFromImageRef reports
// "1.34.0-fat" for ghcr.io/siderolabs/kubelet:v1.34.0-fat.
func componentsAtTarget(status NodeStatus, target string) bool {
	for _, component := range upgradeComponents(status) {
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
	for _, component := range upgradeComponents(status) {
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
// version upgrade: the components of upgradeComponents(status) up to and including the first
// one the node does not yet run at the target version. Components the node already upgraded
// are patched to the same image again, which is what makes the configuration stable across
// the hook calls of one update. For a worker Machine the list is the kubelet alone, the
// patch talosctl upgrade-k8s applies to a node without control-plane components
// (upgradeKubeletOnNode).
func stagedKubernetesConfig(data []byte, status NodeStatus, target string) ([]byte, error) {
	container, err := configpatcher.WithBytes(data).Config()
	if err != nil {
		return nil, errors.Wrap(err, "decoding the machine configuration")
	}

	patches := []configpatcher.Patch{}

	for _, component := range upgradeComponents(status) {
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
// image (extractKubeletVersionSuffix).
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

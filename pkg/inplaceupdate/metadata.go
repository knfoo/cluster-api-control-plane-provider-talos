// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Read access to the management cluster objects the in-place update hooks need: the
// addresses the infrastructure provider reported for a Machine, the Cluster of the
// machine, the Talos secrets bundle CABPT generates a configuration from, and the
// infrastructure metadata secrets a TalosConfig's metadataFrom points at.
//
// The reader is always built from the manager's client, because the hooks are served by
// the CACPPT manager itself: a hook request carries only the spec of the objects, so the
// Machine addresses and the cluster's talosconfig are read here. The rules are the ones
// config/rbac/role.yaml already grants the manager.
package inplaceupdate

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	cabptv1beta1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1beta1"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	talosmachine "github.com/siderolabs/talos/pkg/machinery/config/machine"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MetadataReader reads the objects the hooks need to render a machine configuration and to
// reach the node it is applied to. Secret returns nil data when the secret does not exist,
// which is the normal transient state of a Metal3/CAPM3 metadata secret that the
// infrastructure provider has not created yet.
type MetadataReader interface {
	// Cluster returns the Cluster object of a machine.
	Cluster(ctx context.Context, namespace, name string) (*clusterv1.Cluster, error)

	// Secret returns the data of a secret, or nil when the secret does not exist.
	Secret(ctx context.Context, namespace, name string) (map[string][]byte, error)

	// MachineAddresses returns the addresses of a machine in the order Cluster API reports
	// them in Machine status.addresses, or nil when the Machine does not exist yet or has no
	// addresses. Machine status.addresses is a list of MachineAddress objects (type and
	// address), not a list of strings. Cluster API copies them from the infrastructure
	// provider reference (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineStatus.Addresses)
	// and a hook request carries only the spec of the objects ("Only spec is provided, status
	// fields are not included", docs/book/src/tasks/experimental-features/runtime-sdk/
	// implement-in-place-update-hooks.md), so this is how a hook reaches a node.
	MachineAddresses(ctx context.Context, namespace, name string) ([]clusterv1.MachineAddress, error)
}

// ControlPlaneMachineReader lists the control-plane Machines of a cluster. It is an
// optional capability of a MetadataReader: the bootstrap provider's control-plane controller
// labels every control-plane Machine with the cluster name and the control-plane label
// (controllers/taloscontrolplane_controller.go:669-670), so the hook can find the peers of
// the machine it is updating and wait for the whole control plane to reach the target
// Kubernetes version before syncing the bootstrap manifests to the workload cluster.
type ControlPlaneMachineReader interface {
	// ControlPlaneMachines returns the Machines of the cluster's control plane.
	ControlPlaneMachines(ctx context.Context, namespace, clusterName string) ([]clusterv1.Machine, error)
}

// MachineSetMachineReader lists the Machines Cluster API currently assigns to a machine
// set. It is an optional capability of a MetadataReader, like ControlPlaneMachineReader: the
// CanUpdateMachineSet request carries no Machine objects ("Only spec is provided, status
// fields are not included", docs/book/src/tasks/experimental-features/runtime-sdk/
// implement-in-place-update-hooks.md), so the hook has to read the management cluster to
// find a live Machine of the machine set it can DryRun the machine configuration against.
// Cluster API labels every Machine of a machine set with the name of the set
// (sigs.k8s.io/cluster-api/api/core/v1beta2.MachineSetNameLabel).
type MachineSetMachineReader interface {
	// MachineSetMachines returns the Machines of the machine set, or an empty list when the
	// machine set has no Machines yet.
	MachineSetMachines(ctx context.Context, namespace, machineSetName string) ([]clusterv1.Machine, error)
}

// NewKubeMetadataReader builds a reader that reads from the management cluster with
// the given client. The rules the reader needs are already in config/rbac/role.yaml
// (secrets get;list;watch, machines get;list;watch and the bootstrap/cluster API groups).
func NewKubeMetadataReader(c client.Client) MetadataReader {
	return &kubeMetadataReader{client: c}
}

type kubeMetadataReader struct {
	client client.Client
}

func (r *kubeMetadataReader) Cluster(ctx context.Context, namespace, name string) (*clusterv1.Cluster, error) {
	cluster := &clusterv1.Cluster{}

	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cluster); err != nil {
		return nil, errors.Wrapf(err, "reading the Cluster %q of namespace %q", name, namespace)
	}

	return cluster, nil
}

func (r *kubeMetadataReader) MachineAddresses(ctx context.Context, namespace, name string) ([]clusterv1.MachineAddress, error) {
	machine := &clusterv1.Machine{}

	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, errors.Wrapf(err, "reading the Machine %q of namespace %q", name, namespace)
	}

	return machine.Status.Addresses, nil
}

func (r *kubeMetadataReader) ControlPlaneMachines(ctx context.Context, namespace, clusterName string) ([]clusterv1.Machine, error) {
	machines, err := r.listMachines(ctx, map[string]string{
		clusterv1.ClusterNameLabel:         clusterName,
		clusterv1.MachineControlPlaneLabel: "",
	})
	if err != nil {
		return nil, errors.Wrapf(err, "listing the control-plane machines of cluster %q", clusterName)
	}

	return machines, nil
}

func (r *kubeMetadataReader) MachineSetMachines(ctx context.Context, namespace, machineSetName string) ([]clusterv1.Machine, error) {
	machines, err := r.listMachines(ctx, map[string]string{clusterv1.MachineSetNameLabel: machineSetName})
	if err != nil {
		return nil, errors.Wrapf(err, "listing the machines of machine set %q", machineSetName)
	}

	return machines, nil
}

// listMachines reads the Machines carrying the given labels.
func (r *kubeMetadataReader) listMachines(ctx context.Context, labels map[string]string) ([]clusterv1.Machine, error) {
	list := &clusterv1.MachineList{}

	if err := r.client.List(ctx, list, client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	return list.Items, nil
}

func (r *kubeMetadataReader) Secret(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	secret := &corev1.Secret{}

	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, errors.Wrapf(err, "reading the secret %q of namespace %q", name, namespace)
	}

	return secret.Data, nil
}

// metadataSecret resolves the metadata secret a TalosConfig's metadataFrom points at with the
// bootstrap provider's exported pkg/machineconfig.MetadataSecret, so the hooks and the bootstrap
// controller read the same secret with the same defaults.
func metadataSecret(spec cabptv1beta1.TalosConfigSpec, machine clusterv1.Machine) (string, string, error) {
	return machineconfig.MetadataSecret(&spec, machine.Spec.InfrastructureRef.Name)
}

// secretsBundleSecretName is the name the bootstrap provider stores the cluster's
// Talos secrets bundle under: controllers/secrets.go:53 of the bootstrap provider.
func secretsBundleSecretName(clusterName string) string {
	return clusterName + "-talos"
}

// machineConfigBase returns the base machine configuration of a TalosConfig and the
// standard patches the bootstrap provider adds to a generated one. A TalosConfig that
// supplies its own data starts from that data; a generateType TalosConfig is generated by
// the bootstrap provider's pkg/machineconfig.Generate, which the hooks reach through
// the go.mod replace. The hooks resolve only the inputs (the Cluster, the cluster's
// Talos secrets bundle, the Kubernetes version and the hostname source) and hands them to
// Generate.
//
// Follows the bootstrap provider's generateConfigs
// (controllers/talosconfig_controller.go:754-824) and its secrets bundle loading
// (controllers/secrets.go:63-104).
func (h *Handler) machineConfigBase(ctx context.Context, spec cabptv1beta1.TalosConfigSpec, machine clusterv1.Machine) (string, []string, error) {
	switch spec.GenerateType {
	case "none", "":
		return spec.Data, nil, nil
	}

	if _, err := talosmachine.ParseType(spec.GenerateType); err != nil {
		return "", nil, errors.Errorf("the TalosConfig has no machine configuration data and generate type %q is not a Talos machine type", spec.GenerateType)
	}

	cluster, err := h.metadata.Cluster(ctx, machine.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return "", nil, err
	}

	if cluster.Spec.ControlPlaneEndpoint.Host == "" {
		return "", nil, errors.Errorf("cluster %q has no control plane endpoint yet", cluster.Name)
	}

	bundle, err := h.clusterSecretsBundle(ctx, cluster, machine.Namespace)
	if err != nil {
		return "", nil, err
	}

	// The hostname is only set for machines, not machine pools, and the hook request
	// always carries a Machine (controllers/talosconfig_controller.go:780-789).
	hostname := ""

	switch spec.Hostname.Source {
	case cabptv1beta1.HostnameSourceMachineName:
		hostname = machine.Name

	case cabptv1beta1.HostnameSourceInfrastructureName:
		hostname = machine.Spec.InfrastructureRef.Name
	}

	return machineconfig.Generate(machineconfig.GenerateInput{
		ClusterName:          cluster.Name,
		ControlPlaneEndpoint: "https://" + net.JoinHostPort(cluster.Spec.ControlPlaneEndpoint.Host, strconv.Itoa(int(cluster.Spec.ControlPlaneEndpoint.Port))),
		KubernetesVersion:    strings.TrimPrefix(machine.Spec.Version, "v"),
		TalosVersion:         spec.TalosVersion,
		SecretsBundle:        bundle,
		GenerateType:         spec.GenerateType,
		Hostname:             hostname,
		DNSDomain:            cluster.Spec.ClusterNetwork.ServiceDomain,
		PodSubnets:           cluster.Spec.ClusterNetwork.Pods.CIDRBlocks,
		ServiceSubnets:       cluster.Spec.ClusterNetwork.Services.CIDRBlocks,
	})
}

// clusterSecretsBundle reads the Talos secrets bundle the bootstrap provider generated for
// the cluster. The hooks never create a bundle: without one the machine configuration
// cannot be rendered and the machine is rolled out instead.
// Ported from the read branch of the bootstrap provider's getSecretsBundle
// (controllers/secrets.go:83-104).
func (h *Handler) clusterSecretsBundle(ctx context.Context, cluster *clusterv1.Cluster, namespace string) (*secrets.Bundle, error) {
	name := secretsBundleSecretName(cluster.Name)

	data, err := h.metadata.Secret(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	if data == nil {
		return nil, errors.Errorf("the Talos secrets bundle secret %q of cluster %q does not exist yet", name, cluster.Name)
	}

	bundle := &secrets.Bundle{Clock: secrets.NewFixedClock(time.Now())}

	if _, ok := data["bundle"]; ok {
		if err := yaml.Unmarshal(data["bundle"], bundle); err != nil {
			return nil, errors.Wrapf(err, "decoding the Talos secrets bundle of cluster %q", cluster.Name)
		}

		return bundle, nil
	}

	// The legacy bundle format, still present on clusters bootstrapped before the bundle
	// key was introduced (controllers/secrets.go:92-103).
	if err := yaml.Unmarshal(data["certs"], &bundle.Certs); err != nil {
		return nil, errors.Wrapf(err, "decoding the Talos certificates of cluster %q", cluster.Name)
	}

	if err := yaml.Unmarshal(data["kubeSecrets"], &bundle.Secrets); err != nil {
		return nil, errors.Wrapf(err, "decoding the Kubernetes secrets of cluster %q", cluster.Name)
	}

	if err := yaml.Unmarshal(data["trustdInfo"], &bundle.TrustdInfo); err != nil {
		return nil, errors.Wrapf(err, "decoding the trustd information of cluster %q", cluster.Name)
	}

	bundle.Cluster = &secrets.Cluster{}

	return bundle, nil
}

// templateContext builds the data pkg/machineconfig.TemplateData renders the strategicPatches
// templates with, following resolveTemplateData of the bootstrap provider
// (controllers/talosconfig_controller.go:606-644). It returns nil when the TalosConfig uses
// no templates, and metadataPending=true while the metadata secret metadataFrom names, which
// the infrastructure provider (Metal3/CAPM3) creates after the bootstrap data, does not exist.
func (h *Handler) templateContext(ctx context.Context, spec cabptv1beta1.TalosConfigSpec, machine clusterv1.Machine) (map[string]any, bool, error) {
	if spec.MetadataFrom == nil && len(spec.TemplateVariables) == 0 {
		return nil, false, nil
	}

	cluster, err := h.metadata.Cluster(ctx, machine.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return nil, false, err
	}

	var raw []byte

	if spec.MetadataFrom != nil {
		name, key, err := metadataSecret(spec, machine)
		if err != nil {
			return nil, false, err
		}

		data, err := h.metadata.Secret(ctx, machine.Namespace, name)
		if err != nil {
			return nil, false, err
		}

		if data == nil {
			return nil, true, nil
		}

		metadata, found := data[key]
		if !found {
			return nil, false, errors.Errorf("the metadata secret %q of machine %q has no key %q", name, machine.Name, key)
		}

		raw = metadata
	}

	data, err := machineconfig.TemplateData(raw, cluster, machine.Name, machine.Spec.InfrastructureRef.Name, spec.TemplateVariables)
	if err != nil {
		return nil, false, err
	}

	return data, false, nil
}

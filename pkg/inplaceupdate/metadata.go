// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Read access to the management cluster objects the extension needs to render a
// machine configuration the way the bootstrap provider does: the Cluster of the
// machine, the Talos secrets bundle CABPT generates a configuration from, and the
// infrastructure metadata secrets a TalosConfig's metadataFrom points at.
//
// The extension ships dark and the reader is optional: without one the extension
// still renders TalosConfigs that carry their machine configuration in spec.data,
// and it answers "no patch" for generated or metadata-driven configurations, which
// makes Cluster API roll the machine out instead of guessing a configuration.
package inplaceupdate

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/machineconfig"
	"github.com/siderolabs/cluster-api-bootstrap-provider-talos/pkg/templatepatch"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	talosmachine "github.com/siderolabs/talos/pkg/machinery/config/machine"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MetadataReader reads the objects the extension needs to render a machine
// configuration. Secret returns nil data when the secret does not exist, which is
// the normal transient state of a Metal3/CAPM3 metadata secret that the
// infrastructure provider has not created yet.
type MetadataReader interface {
	// Cluster returns the Cluster object of a machine.
	Cluster(ctx context.Context, namespace, name string) (*clusterv1.Cluster, error)

	// Secret returns the data of a secret, or nil when the secret does not exist.
	Secret(ctx context.Context, namespace, name string) (map[string][]byte, error)
}

// NewKubeMetadataReader builds a reader that reads from the management cluster with
// the given client. The rules the reader needs are already in config/rbac/role.yaml
// (secrets get;list;watch and the bootstrap/cluster API groups).
func NewKubeMetadataReader(c client.Client) MetadataReader {
	return &kubeMetadataReader{client: c}
}

type kubeMetadataReader struct {
	client client.Client
}

func (r *kubeMetadataReader) Cluster(ctx context.Context, namespace, name string) (*clusterv1.Cluster, error) {
	// The Cluster is read unstructured so the reader does not depend on which
	// Cluster API version the cluster's CRD is served in.
	object := &unstructured.Unstructured{}

	object.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("Cluster"))

	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		return nil, errors.Wrapf(err, "reading the Cluster %q of namespace %q", name, namespace)
	}

	cluster := &clusterv1.Cluster{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, cluster); err != nil {
		return nil, errors.Wrapf(err, "decoding the Cluster %q of namespace %q", name, namespace)
	}

	return cluster, nil
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

// metadataSecret locates the metadata secret a TalosConfig's metadataFrom points at.
// The defaults (key "metaData", suffix "-metadata") are the ones the bootstrap
// provider's API types define, so a Metal3/CAPM3 Metal3DataTemplate metadata secret
// is found without any extra configuration.
func metadataSecret(spec cabptv1.TalosConfigSpec, machine clusterv1.Machine) (name, key string, err error) {
	if spec.MetadataFrom == nil {
		return "", "", errors.New("metadataFrom is not set")
	}

	switch {
	case spec.MetadataFrom.SecretRef != nil && spec.MetadataFrom.InfrastructureMachine != nil:
		return "", "", errors.New("metadataFrom sets both secretRef and infrastructureMachine, only one is allowed")

	case spec.MetadataFrom.SecretRef != nil:
		name = spec.MetadataFrom.SecretRef.Name

		key = spec.MetadataFrom.SecretRef.Key
		if key == "" {
			key = "metaData"
		}

	case spec.MetadataFrom.InfrastructureMachine != nil:
		suffix := spec.MetadataFrom.InfrastructureMachine.SecretNameSuffix
		if suffix == "" {
			suffix = "-metadata"
		}

		name = machine.Spec.InfrastructureRef.Name + suffix

		key = spec.MetadataFrom.InfrastructureMachine.DataKey
		if key == "" {
			key = "metaData"
		}

	default:
		return "", "", errors.New("metadataFrom sets neither secretRef nor infrastructureMachine")
	}

	if name == "" {
		return "", "", errors.New("metadataFrom names an empty metadata secret")
	}

	return name, key, nil
}

// secretsBundleSecretName is the name the bootstrap provider stores the cluster's
// Talos secrets bundle under: controllers/secrets.go:53 of the bootstrap provider.
func secretsBundleSecretName(clusterName string) string {
	return clusterName + "-talos"
}

// machineConfigBase returns the base machine configuration of a TalosConfig and the
// standard patches the bootstrap provider adds to a generated one. A TalosConfig that
// supplies its own data starts from that data; a generateType TalosConfig is generated by
// the bootstrap provider's pkg/machineconfig.Generate, which the extension reaches through
// the go.mod replace. The extension resolves only the inputs (the Cluster, the cluster's
// Talos secrets bundle, the Kubernetes version and the hostname source) and hands them to
// Generate.
//
// Follows the bootstrap provider's generateConfigs
// (controllers/talosconfig_controller.go:754-824) and its secrets bundle loading
// (controllers/secrets.go:63-104).
func (h *Handler) machineConfigBase(ctx context.Context, spec cabptv1.TalosConfigSpec, machine clusterv1.Machine) (string, []string, error) {
	switch spec.GenerateType {
	case "none", "":
		return spec.Data, nil, nil
	}

	if _, err := talosmachine.ParseType(spec.GenerateType); err != nil {
		return "", nil, errors.Errorf("the TalosConfig has no machine configuration data and generate type %q is not a Talos machine type", spec.GenerateType)
	}

	if h.metadata == nil {
		return "", nil, errors.New("the TalosConfig is generated with generateType and the extension cannot generate its machine configuration: start the extension with --kubeconfig so it can read the cluster's Talos secrets")
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
	case cabptv1.HostnameSourceMachineName:
		hostname = machine.Name

	case cabptv1.HostnameSourceInfrastructureName:
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
// the cluster. The extension never creates a bundle: without one the machine configuration
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

// templateContext builds the metadata the bootstrap provider's pkg/templatepatch engine
// renders the strategicPatches with, following resolveMetadata
// (controllers/talosconfig_controller.go:679-751). It returns nil metadata when the
// TalosConfig uses no templates, and metadataPending=true when metadataFrom is set but the
// infrastructure provider (Metal3/CAPM3) has not created the metadata claim yet. The
// templateVariables are passed to ApplyPatches, which merges them into the context.
func (h *Handler) templateContext(ctx context.Context, spec cabptv1.TalosConfigSpec, machine clusterv1.Machine) (map[string]string, bool, error) {
	hasMetadata := spec.MetadataFrom != nil
	hasVariables := len(spec.TemplateVariables) > 0

	if !hasMetadata && !hasVariables {
		return nil, false, nil
	}

	metadata := map[string]string{}

	if hasMetadata {
		if h.metadata == nil {
			return nil, false, errors.New("the TalosConfig reads metadataFrom, but the extension was started without --kubeconfig and cannot read the metadata secret")
		}

		name, key, err := metadataSecret(spec, machine)
		if err != nil {
			return nil, false, err
		}

		data, err := h.metadata.Secret(ctx, machine.Namespace, name)
		if err != nil {
			return nil, false, err
		}

		if data == nil {
			// The metadata claim is created after the bootstrap data, so the secret is
			// transiently absent. The bootstrap provider writes the configuration without
			// the rendered patches in that case (two-phase bootstrap, phase 1) and skips the
			// patches that still contain template expressions; the extension renders the
			// same configuration.
			return nil, true, nil
		}

		raw, ok := data[key]
		if !ok {
			return nil, false, errors.Errorf("the metadata secret %q of machine %q has no key %q", name, machine.Name, key)
		}

		metadata = templatepatch.ParseMetadata(raw)

		cluster, err := h.metadata.Cluster(ctx, machine.Namespace, machine.Spec.ClusterName)
		if err != nil {
			return nil, false, err
		}

		templatepatch.EnrichWithCluster(metadata, cluster)
	}

	templatepatch.EnrichWithMachine(metadata, machine.Name, machine.Spec.InfrastructureRef.Name)

	return metadata, false, nil
}

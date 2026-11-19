// Copyright 2020-2026 Politecnico di Torino
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// DefaultClusterAccessPort is the default port of the Gateway listener exposing the workload cluster API servers.
	DefaultClusterAccessPort = 9443
	// DefaultClusterAccessListenerName is the default name of the Gateway listener exposing the workload cluster API servers.
	DefaultClusterAccessListenerName = "cluster-api-access"

	// ClusterAccessKubeconfigKey is the key of the dedicated Secret containing the tenant kubeconfig.
	ClusterAccessKubeconfigKey = "kubeconfig"
	// ClusterAPIKubeconfigKey is the key of the Cluster API kubeconfig Secret containing the admin kubeconfig.
	ClusterAPIKubeconfigKey = "value"

	clusterAccessHashLength = 12
)

// GVKTenantControlPlane is the GVK of the Kamaji TenantControlPlane resource, created by the Kamaji control plane provider.
var GVKTenantControlPlane = kamajiv1alpha1.GroupVersion.WithKind("TenantControlPlane")

// ClusterAccessConfig configures the external access to the API server of the workload clusters,
// exposed through a TLS passthrough listener of an existing Gateway.
type ClusterAccessConfig struct {
	// BaseDomain is the DNS domain the per-environment hostnames belong to. Empty disables the external access.
	BaseDomain string
	// Port is the port of the Gateway listener.
	Port int32
	// ListenerName is the name of the TLS passthrough listener (i.e., the sectionName of the routes).
	ListenerName string
	// GatewayName is the name of the Gateway.
	GatewayName string
	// GatewayNamespace is the namespace of the Gateway.
	GatewayNamespace string
}

// Enabled returns whether the external access to the workload cluster API servers is enabled.
func (c *ClusterAccessConfig) Enabled() bool {
	return c.BaseDomain != ""
}

// Validate checks the configuration, in case the external access is enabled.
func (c *ClusterAccessConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	var errs []string
	if msgs := validation.IsDNS1123Subdomain(c.BaseDomain); len(msgs) > 0 {
		errs = append(errs, fmt.Sprintf("invalid base domain %q: %s", c.BaseDomain, strings.Join(msgs, ", ")))
	} else if len(c.BaseDomain)+1+validation.DNS1123LabelMaxLength > validation.DNS1123SubdomainMaxLength {
		errs = append(errs, fmt.Sprintf("base domain %q is too long", c.BaseDomain))
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Sprintf("invalid port %d", c.Port))
	}
	if msgs := validation.IsDNS1123Subdomain(c.ListenerName); len(msgs) > 0 {
		errs = append(errs, fmt.Sprintf("invalid listener name %q: %s", c.ListenerName, strings.Join(msgs, ", ")))
	}
	if c.GatewayName == "" || c.GatewayNamespace == "" {
		errs = append(errs, "the gateway reference is required")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ClusterAccessHostname returns the hostname exposing the API server of the given Cluster environment.
// The result is a single DNS label below the base domain (so that it matches a "*.<base domain>" wildcard),
// it is stable across reconciliations and unique across tenants (i.e., namespaces), instances and environments
// thanks to a hash of their identifiers. A readable, possibly truncated, prefix eases the identification.
func ClusterAccessHostname(baseDomain, namespace, instanceName, environmentName string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + instanceName + "/" + environmentName))
	hash := hex.EncodeToString(sum[:])[:clusterAccessHashLength]

	prefix := dnsLabelPrefix(instanceName + StringSeparator + environmentName)
	maxPrefix := validation.DNS1123LabelMaxLength - len(StringSeparator) - clusterAccessHashLength
	if len(prefix) > maxPrefix {
		prefix = prefix[:maxPrefix]
	}
	prefix = strings.Trim(prefix, StringSeparator)

	label := hash
	if prefix != "" {
		label = prefix + StringSeparator + hash
	}
	return label + "." + strings.TrimSuffix(strings.ToLower(baseDomain), ".")
}

// dnsLabelPrefix converts a string to lowercase alphanumeric characters and dashes.
func dnsLabelPrefix(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteString(StringSeparator)
		}
	}
	return strings.Trim(b.String(), StringSeparator)
}

// ClusterAccessEndpoint returns the external URL of the workload cluster API server.
func ClusterAccessEndpoint(hostname string, port int32) string {
	return fmt.Sprintf("https://%s:%d", hostname, port)
}

// ClusterAccessKubeconfigSecretName returns the name of the dedicated Secret containing the tenant kubeconfig.
func ClusterAccessKubeconfigSecretName(names ClusterResourceNames) string {
	return names.Cluster + StringSeparator + "tenant-kubeconfig"
}

// ClusterAccessResourceName returns the name shared by the TLSRoute, Role and RoleBinding granting the external access.
func ClusterAccessResourceName(names ClusterResourceNames) string {
	return names.Cluster + StringSeparator + "api-access"
}

// ClusterAPIKubeconfigSecretName returns the name of the kubeconfig Secret generated by Cluster API for the given cluster.
func ClusterAPIKubeconfigSecretName(clusterName string) string {
	return clusterName + StringSeparator + "kubeconfig"
}

// ExternalKubeconfig derives the tenant kubeconfig from the admin kubeconfig generated by Cluster API, keeping the
// credentials and the CA of its current context and pointing it to the external endpoint. The result contains a
// single cluster, user and context named after contextName.
func ExternalKubeconfig(source []byte, endpoint, contextName string) ([]byte, error) {
	cfg, err := clientcmd.Load(source)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the source kubeconfig: %w", err)
	}

	current, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || current == nil {
		return nil, fmt.Errorf("the source kubeconfig has no valid current context")
	}
	cluster, ok := cfg.Clusters[current.Cluster]
	if !ok || cluster == nil {
		return nil, fmt.Errorf("the source kubeconfig has no cluster %q", current.Cluster)
	}
	user, ok := cfg.AuthInfos[current.AuthInfo]
	if !ok || user == nil {
		return nil, fmt.Errorf("the source kubeconfig has no user %q", current.AuthInfo)
	}
	if len(cluster.CertificateAuthorityData) == 0 {
		return nil, fmt.Errorf("the source kubeconfig has no embedded certificate authority")
	}

	userName := contextName + StringSeparator + "admin"
	out := clientcmdapi.NewConfig()
	out.Clusters[contextName] = &clientcmdapi.Cluster{
		Server:                   endpoint,
		CertificateAuthorityData: cluster.CertificateAuthorityData,
	}
	out.AuthInfos[userName] = user.DeepCopy()
	out.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:   contextName,
		AuthInfo:  userName,
		Namespace: current.Namespace,
	}
	out.CurrentContext = contextName

	return clientcmd.Write(*out)
}

// TenantControlPlaneService returns the Service exposing the API server of a Kamaji TenantControlPlane, as reported
// in status.kubernetesResources.service. The found return value is false if the Service has not been reported yet.
func TenantControlPlaneService(tcp *unstructured.Unstructured) (name, namespace string, port int32, found bool) {
	service, ok, err := unstructured.NestedMap(tcp.Object, "status", "kubernetesResources", "service")
	if err != nil || !ok {
		return "", "", 0, false
	}
	name, _, _ = unstructured.NestedString(service, "name")
	namespace, _, _ = unstructured.NestedString(service, "namespace")
	rawPort, _, _ := unstructured.NestedFieldNoCopy(service, "port")
	switch p := rawPort.(type) {
	case int64:
		port = int32(p) //nolint:gosec // Validated below.
	case float64:
		port = int32(p)
	case int32:
		port = p
	case int:
		port = int32(p) //nolint:gosec // Validated below.
	}
	if name == "" || port < 1 || port > 65535 {
		return "", "", 0, false
	}
	if namespace == "" {
		namespace = tcp.GetNamespace()
	}
	return name, namespace, port, true
}

// ClusterAccessParentRef returns the reference to the Gateway listener exposing the workload cluster API servers.
func ClusterAccessParentRef(cfg *ClusterAccessConfig) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{
		Group:       ptr.To(gatewayv1.Group(gatewayv1.GroupName)),
		Kind:        ptr.To(gatewayv1.Kind("Gateway")),
		Name:        gatewayv1.ObjectName(cfg.GatewayName),
		Namespace:   ptr.To(gatewayv1.Namespace(cfg.GatewayNamespace)),
		SectionName: ptr.To(gatewayv1.SectionName(cfg.ListenerName)),
	}
}

// ClusterAccessTLSRouteSpec forges the spec of the TLSRoute forwarding the TLS traffic for the given hostname,
// without terminating it, to the Service of the workload cluster API server.
func ClusterAccessTLSRouteSpec(cfg *ClusterAccessConfig, hostname, routeNamespace, serviceName, serviceNamespace string, port int32) gatewayv1.TLSRouteSpec {
	backend := gatewayv1.BackendObjectReference{
		Group: ptr.To(gatewayv1.Group("")),
		Kind:  ptr.To(gatewayv1.Kind("Service")),
		Name:  gatewayv1.ObjectName(serviceName),
		Port:  ptr.To(port),
	}
	if serviceNamespace != "" && serviceNamespace != routeNamespace {
		backend.Namespace = ptr.To(gatewayv1.Namespace(serviceNamespace))
	}

	return gatewayv1.TLSRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{ClusterAccessParentRef(cfg)},
		},
		Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
		Rules: []gatewayv1.TLSRouteRule{{
			BackendRefs: []gatewayv1.BackendRef{{BackendObjectReference: backend}},
		}},
	}
}

// TLSRouteAccepted returns whether the TLSRoute has been accepted, with resolved references, by the configured
// Gateway listener, considering only the conditions referring to the current generation of the route.
func TLSRouteAccepted(route *gatewayv1.TLSRoute, cfg *ClusterAccessConfig) bool {
	for i := range route.Status.Parents {
		parent := &route.Status.Parents[i]
		if string(parent.ParentRef.Name) != cfg.GatewayName {
			continue
		}
		if parent.ParentRef.Namespace != nil && string(*parent.ParentRef.Namespace) != cfg.GatewayNamespace {
			continue
		}
		if parent.ParentRef.Namespace == nil && route.Namespace != cfg.GatewayNamespace {
			continue
		}
		if parent.ParentRef.SectionName != nil && string(*parent.ParentRef.SectionName) != cfg.ListenerName {
			continue
		}
		accepted := conditionTrue(parent.Conditions, string(gatewayv1.RouteConditionAccepted), route.Generation)
		resolved := conditionTrue(parent.Conditions, string(gatewayv1.RouteConditionResolvedRefs), route.Generation)
		if accepted && resolved {
			return true
		}
	}
	return false
}

func conditionTrue(conditions []metav1.Condition, conditionType string, generation int64) bool {
	condition := findCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue &&
		(condition.ObservedGeneration == 0 || condition.ObservedGeneration >= generation)
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// ClusterAccessRoleRules returns the rules allowing to get exclusively the dedicated kubeconfig Secret.
func ClusterAccessRoleRules(secretName string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: []string{secretName},
		Verbs:         []string{"get"},
	}}
}

// ClusterAccessRoleBinding configures the RoleBinding granting the Role to the owner of the instance.
func ClusterAccessRoleBinding(rb *rbacv1.RoleBinding, roleName, tenantName string) {
	rb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName}
	rb.Subjects = []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: tenantName}}
}

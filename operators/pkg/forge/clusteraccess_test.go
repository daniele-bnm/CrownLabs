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

package forge_test

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	clv1alpha2 "github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/forge"
)

func TestClusterAccessHostname(t *testing.T) {
	const base = "k8s.crownlabs.example.com"
	host := forge.ClusterAccessHostname(base, "tenant-john-doe", "my-instance", "cluster")

	if again := forge.ClusterAccessHostname(base, "tenant-john-doe", "my-instance", "cluster"); again != host {
		t.Fatalf("hostname is not stable: %q != %q", host, again)
	}
	if !strings.HasPrefix(host, "my-instance-cluster-") || !strings.HasSuffix(host, "."+base) {
		t.Fatalf("unexpected hostname format: %q", host)
	}

	others := map[string]string{
		"tenant":      forge.ClusterAccessHostname(base, "tenant-jane-doe", "my-instance", "cluster"),
		"instance":    forge.ClusterAccessHostname(base, "tenant-john-doe", "other-instance", "cluster"),
		"environment": forge.ClusterAccessHostname(base, "tenant-john-doe", "my-instance", "cluster2"),
		// The readable prefix is the same, the hash disambiguates the triple.
		"ambiguous split": forge.ClusterAccessHostname(base, "tenant-john-doe", "my", "instance-cluster"),
	}
	for name, other := range others {
		if other == host {
			t.Fatalf("%s: hostname collision %q", name, host)
		}
	}

	long := forge.ClusterAccessHostname(base+".", "tenant-"+strings.Repeat("a", 60), "Instance."+strings.Repeat("b", 80), "env")
	label, domain, _ := strings.Cut(long, ".")
	if msgs := validation.IsDNS1123Label(label); len(msgs) > 0 {
		t.Fatalf("invalid DNS label %q: %v", label, msgs)
	}
	if domain != base {
		t.Fatalf("hostname must be a single label below the base domain, got %q", long)
	}
	if msgs := validation.IsDNS1123Subdomain(long); len(msgs) > 0 {
		t.Fatalf("invalid hostname %q: %v", long, msgs)
	}
}

func TestClusterAccessConfigValidate(t *testing.T) {
	valid := forge.ClusterAccessConfig{
		BaseDomain: "k8s.crownlabs.local", Port: 9443, ListenerName: "cluster-api-access",
		GatewayName: "crownlabs", GatewayNamespace: "default",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := (&forge.ClusterAccessConfig{}).Validate(); err != nil {
		t.Fatalf("a disabled configuration must be valid: %v", err)
	}
	for name, mutate := range map[string]func(*forge.ClusterAccessConfig){
		"domain":   func(c *forge.ClusterAccessConfig) { c.BaseDomain = "Not_Valid" },
		"port":     func(c *forge.ClusterAccessConfig) { c.Port = 0 },
		"listener": func(c *forge.ClusterAccessConfig) { c.ListenerName = "" },
		"gateway":  func(c *forge.ClusterAccessConfig) { c.GatewayName = "" },
	} {
		cfg := valid
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
}

func sourceKubeconfig(t *testing.T) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["internal"] = &clientcmdapi.Cluster{Server: "https://10.96.0.10:6443", CertificateAuthorityData: []byte("CA")}
	cfg.Clusters["unrelated"] = &clientcmdapi.Cluster{Server: "https://unrelated:6443"}
	cfg.AuthInfos["kubernetes-admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("CERT"), ClientKeyData: []byte("KEY")}
	cfg.Contexts["admin@internal"] = &clientcmdapi.Context{Cluster: "internal", AuthInfo: "kubernetes-admin"}
	cfg.CurrentContext = "admin@internal"
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExternalKubeconfig(t *testing.T) {
	source := sourceKubeconfig(t)
	original := bytes.Clone(source)
	endpoint := "https://host.k8s.crownlabs.local:9443"

	data, err := forge.ExternalKubeconfig(source, endpoint, "instance-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) {
		t.Fatal("the source kubeconfig must not be modified")
	}
	again, err := forge.ExternalKubeconfig(source, endpoint, "instance-cluster")
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("the generated kubeconfig must be deterministic (err=%v)", err)
	}

	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "instance-cluster" || len(cfg.Contexts) != 1 || len(cfg.Clusters) != 1 || len(cfg.AuthInfos) != 1 {
		t.Fatalf("expected a single context, cluster and user, got %+v", cfg)
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	cluster := cfg.Clusters[ctx.Cluster]
	user := cfg.AuthInfos[ctx.AuthInfo]
	if cluster.Server != endpoint || string(cluster.CertificateAuthorityData) != "CA" || cluster.TLSServerName != "" {
		t.Fatalf("unexpected cluster: %+v", cluster)
	}
	if string(user.ClientCertificateData) != "CERT" || string(user.ClientKeyData) != "KEY" {
		t.Fatalf("the admin credentials must be preserved: %+v", user)
	}

	for name, src := range map[string][]byte{
		"invalid":    []byte("{not: [valid"),
		"no context": []byte("apiVersion: v1\nkind: Config\n"),
	} {
		if _, err := forge.ExternalKubeconfig(src, endpoint, "ctx"); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	noCA := clientcmdapi.NewConfig()
	noCA.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://10.0.0.1"}
	noCA.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "t"}
	noCA.Contexts["x"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	noCA.CurrentContext = "x"
	raw, _ := clientcmd.Write(*noCA)
	if _, err := forge.ExternalKubeconfig(raw, endpoint, "ctx"); err == nil {
		t.Fatal("expected an error for a kubeconfig without CA")
	}
}

func TestTenantControlPlaneService(t *testing.T) {
	tcp := func(status map[string]interface{}) *unstructured.Unstructured {
		u := forge.NewClusterAPIObject(forge.GVKTenantControlPlane, "cp", "tenant")
		u.Object["status"] = status
		return u
	}
	service := map[string]interface{}{"name": "cp", "namespace": "tenant", "port": int64(6443)}

	name, ns, port, found := forge.TenantControlPlaneService(tcp(map[string]interface{}{"kubernetesResources": map[string]interface{}{"service": service}}))
	if !found || name != "cp" || ns != "tenant" || port != 6443 {
		t.Fatalf("unexpected service: %s/%s:%d (found=%t)", ns, name, port, found)
	}
	// status.kubernetes.service is not the field populated by Kamaji.
	if _, _, _, found := forge.TenantControlPlaneService(tcp(map[string]interface{}{"kubernetes": map[string]interface{}{"service": service}})); found {
		t.Fatal("status.kubernetes.service must be ignored")
	}
	if _, _, _, found := forge.TenantControlPlaneService(tcp(map[string]interface{}{"kubernetesResources": map[string]interface{}{"service": map[string]interface{}{"name": "cp"}}})); found {
		t.Fatal("a service without port must not be considered available")
	}
	if _, _, _, found := forge.TenantControlPlaneService(tcp(nil)); found {
		t.Fatal("a tenant control plane without status must not report a service")
	}
}

func TestClusterAccessTLSRoute(t *testing.T) {
	cfg := &forge.ClusterAccessConfig{BaseDomain: "k8s.local", Port: 9443, ListenerName: "api", GatewayName: "crownlabs", GatewayNamespace: "default"}
	spec := forge.ClusterAccessTLSRouteSpec(cfg, "h.k8s.local", "tenant", "cp", "tenant", 6443)

	parent := spec.ParentRefs[0]
	if parent.Name != "crownlabs" || *parent.Namespace != "default" || *parent.SectionName != "api" || len(spec.ParentRefs) != 1 {
		t.Fatalf("unexpected parent: %+v", spec.ParentRefs)
	}
	backend := spec.Rules[0].BackendRefs[0]
	if backend.Name != "cp" || *backend.Port != 6443 || backend.Namespace != nil || *backend.Kind != "Service" {
		t.Fatalf("unexpected backend: %+v", backend)
	}
	if len(spec.Hostnames) != 1 || spec.Hostnames[0] != "h.k8s.local" {
		t.Fatalf("unexpected hostnames: %v", spec.Hostnames)
	}

	route := &gatewayv1.TLSRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Generation: 2}}
	setStatus := func(ref gatewayv1.ParentReference, accepted, resolved metav1.ConditionStatus, generation int64) {
		route.Status.Parents = []gatewayv1.RouteParentStatus{{ParentRef: ref, Conditions: []metav1.Condition{
			{Type: string(gatewayv1.RouteConditionAccepted), Status: accepted, ObservedGeneration: generation},
			{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: resolved, ObservedGeneration: generation},
		}}}
	}
	for _, tt := range []struct {
		name               string
		ref                gatewayv1.ParentReference
		accepted, resolved metav1.ConditionStatus
		generation         int64
		want               bool
	}{
		{"accepted and resolved", parent, metav1.ConditionTrue, metav1.ConditionTrue, 2, true},
		{"unresolved refs", parent, metav1.ConditionTrue, metav1.ConditionFalse, 2, false},
		{"not accepted", parent, metav1.ConditionFalse, metav1.ConditionTrue, 2, false},
		{"stale generation", parent, metav1.ConditionTrue, metav1.ConditionTrue, 1, false},
		{"other gateway", gatewayv1.ParentReference{Name: "other", Namespace: parent.Namespace}, metav1.ConditionTrue, metav1.ConditionTrue, 2, false},
		{"other listener", gatewayv1.ParentReference{Name: "crownlabs", Namespace: parent.Namespace, SectionName: ptr.To(gatewayv1.SectionName("https"))},
			metav1.ConditionTrue, metav1.ConditionTrue, 2, false},
	} {
		setStatus(tt.ref, tt.accepted, tt.resolved, tt.generation)
		if got := forge.TLSRouteAccepted(route, cfg); got != tt.want {
			t.Fatalf("%s: got %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestClusterAccessRBAC(t *testing.T) {
	rules := forge.ClusterAccessRoleRules("secret")
	if len(rules) != 1 {
		t.Fatalf("expected a single rule, got %v", rules)
	}
	rule := rules[0]
	if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" || len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "secret" ||
		len(rule.Resources) != 1 || rule.Resources[0] != "secrets" {
		t.Fatalf("the role must only allow getting the dedicated secret: %+v", rule)
	}

	names := forge.ClusterNamesFromInstance(&clv1alpha2.Instance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "tenant"}}, &clv1alpha2.Environment{Name: "env"})
	if forge.ClusterAccessKubeconfigSecretName(names) == forge.ClusterAPIKubeconfigSecretName(names.Cluster) {
		t.Fatal("the dedicated secret must be distinct from the Cluster API one")
	}
}

func TestKamajiControlPlaneCertSANs(t *testing.T) {
	flavor := &clv1alpha2.ClusterFlavor{}
	if sans := forge.KamajiControlPlaneSpec(flavor).Network.CertSANs; sans != nil {
		t.Fatalf("no SAN expected when the access is disabled, got %v", sans)
	}
	spec := forge.KamajiControlPlaneSpec(flavor, "h.k8s.local")
	if sans := spec.Network.CertSANs; len(sans) != 1 || sans[0] != "h.k8s.local" {
		t.Fatalf("unexpected SANs: %v", sans)
	}
	if spec.Network.ServiceType != "ClusterIP" || spec.Network.Gateway != nil {
		t.Fatalf("the control plane Service must remain ClusterIP and not managed through Kamaji Gateway: %+v", spec.Network)
	}
	if spec.Addons.Konnectivity.KonnectivityServerSpec.Port != 8132 {
		t.Fatal("the Konnectivity configuration must be unchanged")
	}
}

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

package instctrl

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	clv1alpha2 "github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	clctx "github.com/netgroup-polito/CrownLabs/operators/pkg/clcontext"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/forge"
)

type clusterAccessFixture struct {
	t        *testing.T
	client   client.Client
	r        *InstanceReconciler
	instance *clv1alpha2.Instance
	env      *clv1alpha2.Environment
	names    forge.ClusterResourceNames
	hostname string
}

func newClusterAccessFixture(t *testing.T, enabled bool) *clusterAccessFixture {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{scheme.AddToScheme, clv1alpha2.AddToScheme, gatewayv1.Install, kamajiv1alpha1.AddToScheme} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}

	instance := &clv1alpha2.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "tenant-john-doe", UID: "uid"},
		Spec: clv1alpha2.InstanceSpec{
			Template: clv1alpha2.GenericRef{Name: "tmpl", Namespace: "workspace-test"},
			Tenant:   clv1alpha2.GenericRef{Name: "john.doe"},
		},
		Status: clv1alpha2.InstanceStatus{Environments: []clv1alpha2.InstanceStatusEnv{{Name: "cluster"}}},
	}
	env := &clv1alpha2.Environment{Name: "cluster", EnvironmentType: clv1alpha2.ClassCluster}

	cfg := forge.ClusterAccessConfig{}
	if enabled {
		cfg = forge.ClusterAccessConfig{BaseDomain: "k8s.crownlabs.local", Port: 9443, ListenerName: "cluster-api-access",
			GatewayName: "crownlabs", GatewayNamespace: "default"}
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(instance.DeepCopy()).
		WithStatusSubresource(&gatewayv1.TLSRoute{}, &kamajiv1alpha1.TenantControlPlane{}).Build()
	f := &clusterAccessFixture{
		t: t, client: c, instance: instance, env: env,
		r:     &InstanceReconciler{Client: c, Scheme: sch, ClusterAccess: cfg},
		names: forge.ClusterNamesFromInstance(instance, env),
	}
	if enabled {
		f.hostname = forge.ClusterAccessHostname(cfg.BaseDomain, instance.Namespace, instance.Name, env.Name)
	}
	return f
}

// enforce runs the cluster access enforcement for the given environment phase, returning the requested requeue delay.
func (f *clusterAccessFixture) enforce(phase clv1alpha2.EnvironmentPhase) *requeueState {
	f.t.Helper()
	f.instance.Status.Environments[0].Phase = phase
	ctx, _ := clctx.InstanceInto(context.Background(), f.instance)
	ctx, _ = clctx.EnvironmentInto(ctx, f.env)
	ctx = clctx.EnvironmentIndexInto(ctx, 0)
	ctx, requeue := withRequeueState(ctx)
	if err := f.r.EnforceClusterAccess(ctx, f.names, f.hostname); err != nil {
		f.t.Fatalf("unexpected error: %v", err)
	}
	return requeue
}

func (f *clusterAccessFixture) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: f.instance.Namespace, Name: name}
}

func (f *clusterAccessFixture) createTCP(status map[string]interface{}) {
	f.t.Helper()
	tcp := forge.NewClusterAPIObject(forge.GVKTenantControlPlane, f.names.ControlPlane, f.instance.Namespace)
	if err := f.client.Create(context.Background(), tcp); err != nil {
		f.t.Fatal(err)
	}
	tcp.Object["status"] = status
	if err := f.client.Status().Update(context.Background(), tcp); err != nil {
		f.t.Fatal(err)
	}
}

func adminKubeconfig(t *testing.T, cert string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["internal"] = &clientcmdapi.Cluster{Server: "https://10.96.12.34:6443", CertificateAuthorityData: []byte("CA")}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte(cert), ClientKeyData: []byte("KEY")}
	cfg.Contexts["admin@internal"] = &clientcmdapi.Context{Cluster: "internal", AuthInfo: "admin"}
	cfg.CurrentContext = "admin@internal"
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertControlledByInstance(t *testing.T, obj metav1.Object) {
	t.Helper()
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "Instance" || owner.Name != "inst" {
		t.Fatalf("%s is not controlled by the instance: %v", obj.GetName(), obj.GetOwnerReferences())
	}
}

func TestEnforceClusterAccessDisabled(t *testing.T) {
	f := newClusterAccessFixture(t, false)
	f.instance.Status.Environments[0].ClusterAccess = &clv1alpha2.InstanceClusterAccessStatus{Ready: true}
	if requeue := f.enforce(clv1alpha2.EnvironmentPhaseReady); requeue.after != 0 {
		t.Fatalf("no requeue expected, got %v", requeue.after)
	}
	if f.instance.Status.Environments[0].ClusterAccess != nil {
		t.Fatal("the cluster access status must be cleared when disabled")
	}
	var roles rbacv1.RoleList
	if err := f.client.List(context.Background(), &roles); err != nil || len(roles.Items) != 0 {
		t.Fatalf("no role expected when disabled (err=%v, roles=%d)", err, len(roles.Items))
	}
}

func TestEnforceClusterAccessReconciliation(t *testing.T) {
	ctx := context.Background()
	f := newClusterAccessFixture(t, true)
	accessName := forge.ClusterAccessResourceName(f.names)
	secretName := forge.ClusterAccessKubeconfigSecretName(f.names)

	// Nothing created by Kamaji/Cluster API yet: only RBAC, the access is pending and requeued.
	requeue := f.enforce(clv1alpha2.EnvironmentPhaseImporting)
	status := f.instance.Status.Environments[0].ClusterAccess
	if status == nil || status.Ready || status.Endpoint != "https://"+f.hostname+":9443" || status.KubeconfigSecretName != secretName {
		t.Fatalf("unexpected status: %+v", status)
	}
	if requeue.after != clusterAccessPendingRequeue {
		t.Fatalf("expected a requeue while pending, got %v", requeue.after)
	}

	assertClusterAccessRBAC(t, f, accessName, secretName)
	if err := f.client.Get(ctx, f.key(accessName), &gatewayv1.TLSRoute{}); !kerrors.IsNotFound(err) {
		t.Fatalf("no route expected without the tenant control plane, got %v", err)
	}
	if err := f.client.Get(ctx, f.key(secretName), &corev1.Secret{}); !kerrors.IsNotFound(err) {
		t.Fatalf("no kubeconfig expected without the source secret, got %v", err)
	}

	// The Service is reported only in status.kubernetesResources.service: other fields are ignored.
	service := map[string]interface{}{"name": f.names.ControlPlane, "namespace": f.instance.Namespace, "port": int64(6443)}
	f.createTCP(map[string]interface{}{"kubernetes": map[string]interface{}{"service": service}})
	f.enforce(clv1alpha2.EnvironmentPhaseImporting)
	if err := f.client.Get(ctx, f.key(accessName), &gatewayv1.TLSRoute{}); !kerrors.IsNotFound(err) {
		t.Fatalf("no route expected while the service is not reported, got %v", err)
	}

	tcp := forge.NewClusterAPIObject(forge.GVKTenantControlPlane, f.names.ControlPlane, f.instance.Namespace)
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(tcp), tcp); err != nil {
		t.Fatal(err)
	}
	tcp.Object["status"] = map[string]interface{}{"kubernetesResources": map[string]interface{}{"service": service}}
	if err := f.client.Status().Update(ctx, tcp); err != nil {
		t.Fatal(err)
	}
	sourceData := adminKubeconfig(t, "CERT-1")
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forge.ClusterAPIKubeconfigSecretName(f.names.Cluster), Namespace: f.instance.Namespace},
		Data:       map[string][]byte{forge.ClusterAPIKubeconfigKey: sourceData},
	}
	if err := f.client.Create(ctx, source); err != nil {
		t.Fatal(err)
	}

	// Route and kubeconfig created, but the route has not been accepted yet.
	f.enforce(clv1alpha2.EnvironmentPhaseReady)
	if f.instance.Status.Environments[0].ClusterAccess.Ready {
		t.Fatal("the access must not be ready before the route is accepted")
	}
	assertClusterAccessResources(t, f, accessName, secretName)

	// Accepted, but with unresolved references: still not ready.
	setRouteStatus(t, f, accessName, metav1.ConditionFalse)
	f.enforce(clv1alpha2.EnvironmentPhaseReady)
	if f.instance.Status.Environments[0].ClusterAccess.Ready {
		t.Fatal("the access must not be ready with unresolved references")
	}

	setRouteStatus(t, f, accessName, metav1.ConditionTrue)
	if requeue := f.enforce(clv1alpha2.EnvironmentPhaseReady); !f.instance.Status.Environments[0].ClusterAccess.Ready || requeue.after != 0 {
		t.Fatalf("the access must be ready without requeue (requeue=%v)", requeue.after)
	}
	f.enforce(clv1alpha2.EnvironmentPhaseRunning)
	if f.instance.Status.Environments[0].ClusterAccess.Ready {
		t.Fatal("the access must not be ready while the cluster is not ready")
	}

	// Rotated source credentials are propagated, while the source secret is never modified.
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	rotated := adminKubeconfig(t, "CERT-2")
	source.Data[forge.ClusterAPIKubeconfigKey] = rotated
	if err := f.client.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	f.enforce(clv1alpha2.EnvironmentPhaseReady)
	var secret corev1.Secret
	if err := f.client.Get(ctx, f.key(secretName), &secret); err != nil {
		t.Fatal(err)
	}
	assertKubeconfig(t, secret.Data[forge.ClusterAccessKubeconfigKey], "https://"+f.hostname+":9443", "CERT-2")
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source.Data[forge.ClusterAPIKubeconfigKey], rotated) || len(source.OwnerReferences) != 0 || len(source.Labels) != 0 {
		t.Fatalf("the source secret must not be modified: %+v", source.ObjectMeta)
	}
}

func assertClusterAccessRBAC(t *testing.T, f *clusterAccessFixture, accessName, secretName string) {
	t.Helper()
	ctx := context.Background()
	var role rbacv1.Role
	if err := f.client.Get(ctx, f.key(accessName), &role); err != nil {
		t.Fatal(err)
	}
	assertControlledByInstance(t, &role)
	if len(role.Rules) != 1 || len(role.Rules[0].ResourceNames) != 1 || role.Rules[0].ResourceNames[0] != secretName ||
		len(role.Rules[0].Verbs) != 1 || role.Rules[0].Verbs[0] != "get" {
		t.Fatalf("the role must only grant get on the dedicated secret: %+v", role.Rules)
	}
	var binding rbacv1.RoleBinding
	if err := f.client.Get(ctx, f.key(accessName), &binding); err != nil {
		t.Fatal(err)
	}
	assertControlledByInstance(t, &binding)
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != accessName ||
		len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.UserKind || binding.Subjects[0].Name != "john.doe" {
		t.Fatalf("unexpected role binding: %+v", binding)
	}
}

func assertClusterAccessResources(t *testing.T, f *clusterAccessFixture, accessName, secretName string) {
	t.Helper()
	ctx := context.Background()
	var route gatewayv1.TLSRoute
	if err := f.client.Get(ctx, f.key(accessName), &route); err != nil {
		t.Fatal(err)
	}
	assertControlledByInstance(t, &route)
	backend := route.Spec.Rules[0].BackendRefs[0]
	if string(route.Spec.Hostnames[0]) != f.hostname || string(backend.Name) != f.names.ControlPlane || *backend.Port != 6443 {
		t.Fatalf("unexpected route spec: %+v", route.Spec)
	}

	var secret corev1.Secret
	if err := f.client.Get(ctx, f.key(secretName), &secret); err != nil {
		t.Fatal(err)
	}
	assertControlledByInstance(t, &secret)
	assertKubeconfig(t, secret.Data[forge.ClusterAccessKubeconfigKey], "https://"+f.hostname+":9443", "CERT-1")
	if !reflect.DeepEqual(secret.Labels, forge.EnvironmentObjectLabels(nil, f.instance, f.env)) || len(secret.Annotations) != 0 {
		t.Fatalf("only the environment labels are expected on the dedicated secret: %v %v", secret.Labels, secret.Annotations)
	}
}

func setRouteStatus(t *testing.T, f *clusterAccessFixture, name string, resolved metav1.ConditionStatus) {
	t.Helper()
	var route gatewayv1.TLSRoute
	if err := f.client.Get(context.Background(), f.key(name), &route); err != nil {
		t.Fatal(err)
	}
	route.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      forge.ClusterAccessParentRef(&f.r.ClusterAccess),
		ControllerName: "gateway.envoyproxy.io/gatewayclass-controller",
		Conditions: []metav1.Condition{
			{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.Now()},
			{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: resolved, Reason: "ResolvedRefs", LastTransitionTime: metav1.Now()},
		},
	}}
	if err := f.client.Status().Update(context.Background(), &route); err != nil {
		t.Fatal(err)
	}
}

func assertKubeconfig(t *testing.T, data []byte, endpoint, cert string) {
	t.Helper()
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil || cfg.Clusters[ctx.Cluster].Server != endpoint || string(cfg.AuthInfos[ctx.AuthInfo].ClientCertificateData) != cert {
		t.Fatalf("unexpected kubeconfig, expected endpoint %s and credentials %s", endpoint, cert)
	}
}

func TestSecretToInstance(t *testing.T) {
	f := newClusterAccessFixture(t, true)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forge.GVKCluster)
	cluster.SetName(f.names.Cluster)
	cluster.SetNamespace(f.instance.Namespace)
	cluster.SetLabels(forge.EnvironmentObjectLabels(nil, f.instance, f.env))
	if err := f.client.Create(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	want := f.key(f.instance.Name)

	capi := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: forge.ClusterAPIKubeconfigSecretName(f.names.Cluster), Namespace: f.instance.Namespace,
		Labels: map[string]string{"cluster.x-k8s.io/cluster-name": f.names.Cluster},
	}}
	if got := f.r.secretToInstance(context.Background(), capi); len(got) != 1 || got[0].NamespacedName != want {
		t.Fatalf("the Cluster API kubeconfig must map to the instance, got %v", got)
	}

	other := capi.DeepCopy()
	other.Name = f.names.Cluster + "-ca"
	if got := f.r.secretToInstance(context.Background(), other); len(got) != 0 {
		t.Fatalf("other Cluster API secrets must be ignored, got %v", got)
	}

	dedicated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dedicated", Namespace: f.instance.Namespace}}
	if err := ctrl.SetControllerReference(f.instance, dedicated, f.r.Scheme); err != nil {
		t.Fatal(err)
	}
	if got := f.r.secretToInstance(context.Background(), dedicated); len(got) != 1 || got[0].NamespacedName != want {
		t.Fatalf("the dedicated secret must map to the instance, got %v", got)
	}

	unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: f.instance.Namespace}}
	if got := f.r.secretToInstance(context.Background(), unrelated); len(got) != 0 {
		t.Fatalf("unrelated secrets must be ignored, got %v", got)
	}
}

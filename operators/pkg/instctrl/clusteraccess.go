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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	clv1alpha2 "github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	clctx "github.com/netgroup-polito/CrownLabs/operators/pkg/clcontext"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/forge"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/utils"
)

// clusterAccessPendingRequeue is the delay before checking again the resources created asynchronously
// by Kamaji and Cluster API (or the route status set by the Gateway) while the cluster access is not ready.
const clusterAccessPendingRequeue = 15 * time.Second

type requeueKey struct{}

// requeueState collects, during a reconciliation, the shortest delay requested to reconcile again
// the instance without considering the current reconciliation as failed.
type requeueState struct {
	after time.Duration
}

func withRequeueState(ctx context.Context) (context.Context, *requeueState) {
	state := &requeueState{}
	return context.WithValue(ctx, requeueKey{}, state), state
}

func requestRequeue(ctx context.Context, after time.Duration) {
	if state, ok := ctx.Value(requeueKey{}).(*requeueState); ok && (state.after == 0 || after < state.after) {
		state.after = after
	}
}

// EnforceClusterAccess enforces the resources granting the instance owner the access to the API server of the
// workload cluster of the current Cluster environment through the external hostname: a TLSRoute attached to
// the TLS passthrough listener of the Gateway, a dedicated Secret containing the kubeconfig, and a Role and
// RoleBinding allowing the owner to get exclusively that Secret. All the resources are owned by the instance.
// Resources created asynchronously by Kamaji and Cluster API that are not available yet do not cause an error,
// but the access is marked as not ready and the reconciliation is requeued.
func (r *InstanceReconciler) EnforceClusterAccess(ctx context.Context, names forge.ClusterResourceNames, hostname string) error {
	instance := clctx.InstanceFrom(ctx)
	envIndex := clctx.EnvironmentIndexFrom(ctx)
	envStatus := &instance.Status.Environments[envIndex]

	if !r.ClusterAccess.Enabled() {
		envStatus.ClusterAccess = nil
		return nil
	}

	secretName := forge.ClusterAccessKubeconfigSecretName(names)
	status := &clv1alpha2.InstanceClusterAccessStatus{
		Endpoint:             forge.ClusterAccessEndpoint(hostname, r.ClusterAccess.Port),
		KubeconfigSecretName: secretName,
	}
	envStatus.ClusterAccess = status

	if err := r.enforceClusterAccessRBAC(ctx, names, secretName); err != nil {
		return err
	}

	routeAccepted, err := r.enforceClusterAccessRoute(ctx, names, hostname)
	if err != nil {
		return err
	}

	kubeconfigAvailable, err := r.enforceClusterAccessKubeconfig(ctx, names, secretName, status.Endpoint)
	if err != nil {
		return err
	}

	status.Ready = envStatus.Phase == clv1alpha2.EnvironmentPhaseReady && routeAccepted && kubeconfigAvailable
	if !status.Ready {
		requestRequeue(ctx, clusterAccessPendingRequeue)
	}
	return nil
}

func (r *InstanceReconciler) enforceClusterAccessRBAC(ctx context.Context, names forge.ClusterResourceNames, secretName string) error {
	instance := clctx.InstanceFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)
	name := forge.ClusterAccessResourceName(names)

	role := rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
	if err := r.enforceOwnedObject(ctx, &role, func() error {
		role.SetLabels(forge.EnvironmentObjectLabels(role.GetLabels(), instance, environment))
		role.Rules = forge.ClusterAccessRoleRules(secretName)
		return nil
	}); err != nil {
		return err
	}

	binding := rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
	return r.enforceOwnedObject(ctx, &binding, func() error {
		binding.SetLabels(forge.EnvironmentObjectLabels(binding.GetLabels(), instance, environment))
		forge.ClusterAccessRoleBinding(&binding, name, instance.Spec.Tenant.Name)
		return nil
	})
}

// enforceClusterAccessRoute enforces the TLSRoute towards the Service of the TenantControlPlane, returning whether
// it has been accepted by the Gateway. It returns false without errors while the Service is not reported yet.
func (r *InstanceReconciler) enforceClusterAccessRoute(ctx context.Context, names forge.ClusterResourceNames, hostname string) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	instance := clctx.InstanceFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)

	// The Kamaji control plane provider creates the TenantControlPlane with the same name and namespace of the KamajiControlPlane.
	tcp := forge.NewClusterAPIObject(forge.GVKTenantControlPlane, names.ControlPlane, instance.Namespace)
	if err := r.Get(ctx, client.ObjectKeyFromObject(tcp), tcp); err != nil {
		if kerrors.IsNotFound(err) {
			log.V(utils.LogDebugLevel).Info("tenant control plane not yet created", "tenantcontrolplane", klog.KObj(tcp))
			return false, nil
		}
		log.Error(err, "failed retrieving the tenant control plane", "tenantcontrolplane", klog.KObj(tcp))
		return false, err
	}

	serviceName, serviceNamespace, port, found := forge.TenantControlPlaneService(tcp)
	if !found {
		log.V(utils.LogDebugLevel).Info("tenant control plane service not yet reported", "tenantcontrolplane", klog.KObj(tcp))
		return false, nil
	}

	route := gatewayv1.TLSRoute{ObjectMeta: metav1.ObjectMeta{Name: forge.ClusterAccessResourceName(names), Namespace: instance.Namespace}}
	if err := r.enforceOwnedObject(ctx, &route, func() error {
		route.SetLabels(forge.EnvironmentObjectLabels(route.GetLabels(), instance, environment))
		route.Spec = forge.ClusterAccessTLSRouteSpec(&r.ClusterAccess, hostname, instance.Namespace, serviceName, serviceNamespace, port)
		return nil
	}); err != nil {
		return false, err
	}

	return forge.TLSRouteAccepted(&route, &r.ClusterAccess), nil
}

// enforceClusterAccessKubeconfig enforces the dedicated Secret with the kubeconfig pointing to the external endpoint,
// derived from the one generated by Cluster API, which is never modified. It returns false without errors while
// the source Secret is not available yet.
func (r *InstanceReconciler) enforceClusterAccessKubeconfig(ctx context.Context, names forge.ClusterResourceNames, secretName, endpoint string) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	instance := clctx.InstanceFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)

	var source corev1.Secret
	sourceKey := types.NamespacedName{Name: forge.ClusterAPIKubeconfigSecretName(names.Cluster), Namespace: instance.Namespace}
	if err := r.Get(ctx, sourceKey, &source); err != nil {
		if kerrors.IsNotFound(err) {
			log.V(utils.LogDebugLevel).Info("cluster kubeconfig not yet generated", "secret", sourceKey)
			return false, nil
		}
		log.Error(err, "failed retrieving the cluster kubeconfig", "secret", sourceKey)
		return false, err
	}

	data, ok := source.Data[forge.ClusterAPIKubeconfigKey]
	if !ok || len(data) == 0 {
		log.V(utils.LogDebugLevel).Info("cluster kubeconfig not yet populated", "secret", sourceKey)
		return false, nil
	}

	kubeconfig, err := forge.ExternalKubeconfig(data, endpoint, names.Cluster)
	if err != nil {
		// The error never includes the kubeconfig content.
		log.Error(err, "failed generating the tenant kubeconfig", "secret", sourceKey)
		return false, fmt.Errorf("failed generating the tenant kubeconfig from %s: %w", sourceKey, err)
	}

	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: instance.Namespace}}
	if err := r.enforceOwnedObject(ctx, &secret, func() error {
		secret.SetLabels(forge.EnvironmentObjectLabels(secret.GetLabels(), instance, environment))
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{forge.ClusterAccessKubeconfigKey: kubeconfig}
		return nil
	}); err != nil {
		return false, err
	}
	return true, nil
}

// enforceOwnedObject creates or updates an object controlled by the current instance.
func (r *InstanceReconciler) enforceOwnedObject(ctx context.Context, obj client.Object, mutate func() error) error {
	log := ctrl.LoggerFrom(ctx)
	instance := clctx.InstanceFrom(ctx)

	res, err := ctrl.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		return ctrl.SetControllerReference(instance, obj, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to enforce object", "kind", fmt.Sprintf("%T", obj), "object", klog.KObj(obj))
		return err
	}
	log.V(utils.FromResult(res)).Info("object enforced", "kind", fmt.Sprintf("%T", obj), "object", klog.KObj(obj), "result", res)
	return nil
}

// secretToInstance maps a Secret to the Instance to be reconciled: the dedicated kubeconfig Secret is controlled by
// the Instance, while the kubeconfig Secret generated by Cluster API is associated with the Instance through the
// labels of the corresponding Cluster.
func (r *InstanceReconciler) secretToInstance(ctx context.Context, o client.Object) []reconcile.Request {
	if owner := metav1.GetControllerOf(o); owner != nil {
		if owner.Kind == "Instance" && owner.APIVersion == clv1alpha2.GroupVersion.String() {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: owner.Name}}}
		}
	}

	clusterName, ok := o.GetLabels()[clusterv1beta2.ClusterNameLabel]
	if !ok || o.GetName() != forge.ClusterAPIKubeconfigSecretName(clusterName) {
		return nil
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forge.GVKCluster)
	if err := r.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: clusterName}, cluster); err != nil {
		return nil
	}
	if instance, found := forge.InstanceNameFromLabels(cluster.GetLabels()); found {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: instance}}}
	}
	return nil
}

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

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apicommon "github.com/netgroup-polito/CrownLabs/operators/api/common"
)

// ComponentResources specifies the CPU and memory limits assigned to a single control plane component.
type ComponentResources struct {
	// The CPU limit assigned to the component.
	CPU resource.Quantity `json:"cpu"`

	// The memory limit assigned to the component.
	Memory resource.Quantity `json:"memory"`
}

// ControlPlaneSpec describes the shape of the control plane nodes of the cluster.
type ControlPlaneSpec struct {
	apicommon.ResourceSpec `json:",inline"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// The number of control plane replicas (use an odd number greater than 1 for HA setups).
	Replicas int32 `json:"replicas"`

	// The datastore backend used by the control plane (e.g. "etcd", "sqlite").
	DataStoreName string `json:"dataStoreName"`

	// The CPU and memory limits assigned to the kube-apiserver.
	APIServerResources ComponentResources `json:"apiServerResources,omitempty"`

	// The CPU and memory limits assigned to the kube-scheduler.
	SchedulerResources ComponentResources `json:"schedulerResources,omitempty"`

	// The CPU and memory limits assigned to the kube-controller-manager.
	ControllerManagerResources ComponentResources `json:"controllerManagerResources,omitempty"`
}

// WorkerSpec describes the shape of the worker nodes of the cluster.
type WorkerSpec struct {
}

// AddonSpec describes an additional Helm chart to be installed on the cluster.
type AddonSpec struct {
	// The name of the addon.
	Name string `json:"name"`

	// The URL of the Helm chart repository.
	RepoURL string `json:"repoURL"`

	// The name of the Helm chart to be installed.
	ChartName string `json:"chartName"`

	// The version of the Helm chart to be installed.
	Version string `json:"version"`

	// The namespace in which the Helm chart should be installed.
	Namespace string `json:"namespace"`

	// ValuesTemplate is an inline YAML representing the values for the Helm chart.
	// This YAML supports Go templating to reference
	// fields from each selected workload Cluster and programatically create and
	// set values.
	ValuesTemplate string `json:"values,omitempty"`
}

// ClusterFlavorSpec is the specification of the desired state of the ClusterFlavor.
type ClusterFlavorSpec struct {
	// The Kubernetes version to be installed on the cluster, e.g. "1.28".
	Version string `json:"version"`

	// The specification of the control plane nodes of the cluster.
	ControlPlane ControlPlaneSpec `json:"controlPlane"`

	// The specification of the worker nodes of the cluster.
	Workers WorkerSpec `json:"workers"`

	// The set of additional Helm charts to be installed on the cluster.
	AddonList []AddonSpec `json:"addonList,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName="cflv"
// +kubebuilder:storageversion

// ClusterFlavor describes a reusable blueprint for provisioning a Kubernetes cluster in CrownLabs.
type ClusterFlavor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterFlavorSpec `json:"spec,omitempty"`
}

func init() {
	SchemeBuilder.Register(&ClusterFlavor{})
}
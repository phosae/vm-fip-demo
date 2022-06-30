/*
.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
)

// QvmSpec defines the desired state of Qvm
type QvmSpec struct {
	// VM is the definition of VirtualMachine workload
	VM virtv1.VirtualMachineSpec `json:"vm"`
	// FloatingIPs binding to VM
	FloatingIPs []string `json:"floatingIPs"`
}

type IngressRoute struct {
	NodeName string `json:"nodeName"`
	Ready    bool   `json:"ready"`
}

type QvmNetworkStatus struct {
	IngressRoutes []IngressRoute `json:"ingressRoutes,omitempty"`
}

// QvmStatus defines the observed state of Qvm
type QvmStatus struct {
	NodeName string            `json:"nodeName,omitempty"`
	Phase    string            `json:"phase,omitempty"`
	Network  *QvmNetworkStatus `json:"network,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".spec.status.phase",description="Qvm Phase"
// +kubebuilder:printcolumn:name="NODENAME",type="string",JSONPath=".spec.status.nodeName",description="Node Name"
// +kubebuilder:printcolumn:name="EXTERNAL-IPs",type="string",JSONPath=".spec.floatingIPs",description="Floating IPs"

// Qvm is the Schema for the qvms API
type Qvm struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   QvmSpec   `json:"spec,omitempty"`
	Status QvmStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// QvmList contains a list of Qvm
type QvmList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Qvm `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Qvm{}, &QvmList{})
}

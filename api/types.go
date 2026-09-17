package api

import (
	"encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "nlb.independent.dev", Version: "v1alpha1"}

const Finalizer = "nlb.independent.dev/cleanup"

type PoolSpec struct {
	CompartmentID  string `json:"compartmentId"`
	SubnetID       string `json:"subnetId"`
	Occupancy      int    `json:"occupancy"`
	MaxShards      int    `json:"maxShards"`
	PortStart      int    `json:"portStart"`
	PreserveSource bool   `json:"preserveSource"`
	AllocationMode string `json:"allocationMode,omitempty"` // Empty/Dynamic preserves sequential allocation; Explicit reserves requested ports.
	ProvisionEmpty bool   `json:"provisionEmpty,omitempty"` // Create shard zero before any bindings exist.
	MaxWorkers     int    `json:"maxWorkers,omitempty"`     // Zero means 20; includes replacement/surge workers.
}
type WorkerIdentity struct {
	UID        string `json:"uid"`
	InstanceID string `json:"instanceId"`
	IP         string `json:"ip"`
}
type Shard struct {
	ID       string `json:"id,omitempty"`
	PublicIP string `json:"publicIP,omitempty"`
}
type Allocation struct {
	BindingName string `json:"bindingName"`
	ServiceName string `json:"serviceName"`
	ServiceUID  string `json:"serviceUID"`
	PortName    string `json:"portName"`
	Mode        string `json:"mode"`
	Shard       int    `json:"shard"`
	Port        int    `json:"port"`
	Name        string `json:"name"`
	Tombstone   bool   `json:"tombstone,omitempty"`
	NodePort    int    `json:"nodePort,omitempty"` // NodePortCluster destination port, pinned before cloud mutation.
}
type PoolStatus struct {
	PendingWork string                    `json:"pendingWork,omitempty"`
	Allocations map[string]Allocation     `json:"allocations,omitempty"`
	Shards      []Shard                   `json:"shards,omitempty"`
	FrozenSpec  *PoolSpec                 `json:"frozenSpec,omitempty"`
	Conditions  []metav1.Condition        `json:"conditions,omitempty"`
	Used        int                       `json:"used"`
	Available   int                       `json:"available"`
	Workers     map[string]WorkerIdentity `json:"workers,omitempty"`
}
type NLBPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PoolSpec   `json:"spec"`
	Status            PoolStatus `json:"status,omitempty"`
}
type NLBPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NLBPool `json:"items"`
}
type BindingSpec struct {
	Pool          string `json:"pool"`
	Service       string `json:"service"`
	PortName      string `json:"portName"`
	Mode          string `json:"mode"`                    // NodePort, PodIP, or NodePortCluster; immutable after allocation.
	HealthPort    int    `json:"healthPort"`              // TCP NodePort in NodePort; pod target in PodIP; 10256 in NodePortCluster.
	RequestedPort int    `json:"requestedPort,omitempty"` // Required only for Explicit pools; immutable after allocation.
	Suspended     bool   `json:"suspended,omitempty"`     // Fail closed while preserving the allocated endpoint lease.
}
type BindingStatus struct {
	ExternalIP   string             `json:"externalIP,omitempty"`
	ExternalPort int                `json:"externalPort,omitempty"`
	BackendSet   string             `json:"backendSet,omitempty"`
	NLBID        string             `json:"nlbId,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
type TunnelBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BindingSpec   `json:"spec"`
	Status            BindingStatus `json:"status,omitempty"`
}
type TunnelBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelBinding `json:"items"`
}

func copyJSON[T any](in *T) *T {
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	out := new(T)
	if err = json.Unmarshal(b, out); err != nil {
		panic(err)
	}
	return out
}
func (p *NLBPool) DeepCopyObject() runtime.Object           { return copyJSON(p) }
func (p *NLBPoolList) DeepCopyObject() runtime.Object       { return copyJSON(p) }
func (p *TunnelBinding) DeepCopyObject() runtime.Object     { return copyJSON(p) }
func (p *TunnelBindingList) DeepCopyObject() runtime.Object { return copyJSON(p) }
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &NLBPool{}, &NLBPoolList{}, &TunnelBinding{}, &TunnelBindingList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

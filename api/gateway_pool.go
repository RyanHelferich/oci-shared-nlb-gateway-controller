package api

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GatewayPool is an opt-in allocator. A generated Gateway always represents one NLB.
type GatewayPoolSpec struct {
	GatewayClassName string `json:"gatewayClassName"`
	Occupancy        int    `json:"occupancy"`
	MaxGateways      int    `json:"maxGateways"`
	PortStart        int    `json:"portStart"`
	MaxWorkers       int    `json:"maxWorkers,omitempty"`
}

type GatewayAllocation struct {
	RouteName    string `json:"routeName"`
	ServiceName  string `json:"serviceName"`
	ServiceUID   string `json:"serviceUID"`
	ServicePort  int32  `json:"servicePort"`
	HealthPort   int32  `json:"healthPort"`
	GatewayName  string `json:"gatewayName"`
	Shard        int    `json:"shard"`
	ListenerName string `json:"listenerName"`
	Port         int32  `json:"port"`
	Tombstone    bool   `json:"tombstone,omitempty"`
	Attached     bool   `json:"attached,omitempty"`
	Mode         string `json:"mode,omitempty"` // PodIP, NodePortCluster, or public NodePortLocal; empty in old leases means PodIP.
}

type GatewayPoolStatus struct {
	Allocations    map[string]GatewayAllocation `json:"allocations,omitempty"`
	Gateways       map[string]string            `json:"gateways,omitempty"`
	FrozenSpec     *GatewayPoolSpec             `json:"frozenSpec,omitempty"`
	RejectedRoutes map[string]string            `json:"rejectedRoutes,omitempty"`
	Conditions     []metav1.Condition           `json:"conditions,omitempty"`
	Used           int                          `json:"used"`
	Available      int                          `json:"available"`
}

type GatewayPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GatewayPoolSpec   `json:"spec"`
	Status            GatewayPoolStatus `json:"status,omitempty"`
}

type GatewayPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayPool `json:"items"`
}

func (p *GatewayPool) DeepCopyObject() runtime.Object     { return copyJSON(p) }
func (p *GatewayPoolList) DeepCopyObject() runtime.Object { return copyJSON(p) }
func AddGatewayPoolToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &GatewayPool{}, &GatewayPoolList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

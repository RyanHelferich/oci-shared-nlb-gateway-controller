package internalcontroller

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func ptr[T any](v T) *T { return &v }
func TestEndpointOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Service, *discovery.EndpointSlice, *core.Pod)
		valid  bool
	}{
		{"ready", func(*core.Service, *discovery.EndpointSlice, *core.Pod) {}, true},
		{"valid-with-extra-foreign-slice", func(*core.Service, *discovery.EndpointSlice, *core.Pod) {}, false},
		{"extra-address", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			e.Endpoints[0].Addresses = append(e.Endpoints[0].Addresses, "10.244.0.99")
		}, false},
		{"conflicting-duplicate-pod", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			other := e.Endpoints[0].DeepCopy()
			other.Addresses = []string{"10.244.0.99"}
			e.Endpoints = append(e.Endpoints, *other)
		}, false},
		{"identical-duplicate-pod", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			e.Endpoints = append(e.Endpoints, *e.Endpoints[0].DeepCopy())
		}, true},
		{"wrong-endpoint-node", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			e.Endpoints[0].NodeName = ptr("other-node")
		}, false},
		{"stale-selector", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			s.Spec.Selector = map[string]string{"app": "expected"}
			p.Labels = map[string]string{"app": "different"}
		}, false},
		{"unready", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			e.Endpoints[0].Conditions.Ready = ptr(false)
		}, false},
		{"terminating", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			e.Endpoints[0].Conditions.Terminating = ptr(true)
		}, false},
		{"ambiguous", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			other := e.Endpoints[0].DeepCopy()
			other.TargetRef.UID = "another-pod"
			e.Endpoints = append(e.Endpoints, *other)
		}, false},
		{"recreated-service", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) { s.UID = "new-service" }, false},
		{"foreign-slice", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) { e.OwnerReferences = nil }, false},
		{"stale-pod", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) { p.UID = "new-pod" }, false},
		{"foreign-health-port", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) { s.Spec.Ports[1].NodePort = 30902 }, false},
		{"loadbalancer-service", func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "wireguard", Namespace: "tenant", UID: "service"}, Spec: core.ServiceSpec{Type: core.ServiceTypeNodePort, ExternalTrafficPolicy: core.ServiceExternalTrafficPolicyLocal, Ports: []core.ServicePort{{Name: "wg", Protocol: core.ProtocolUDP, NodePort: 30001}}}}
			ep := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "tenant", Labels: map[string]string{discovery.LabelServiceName: "wireguard"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: "service"}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: ptr("wg"), Protocol: ptr(core.ProtocolUDP), Port: ptr(int32(51820))}}, Endpoints: []discovery.Endpoint{{Addresses: []string{"10.244.0.2"}, Conditions: discovery.EndpointConditions{Ready: ptr(true)}, TargetRef: &core.ObjectReference{Kind: "Pod", Name: "wg", Namespace: "tenant", UID: "pod"}}}}
			p := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "wg", Namespace: "tenant", UID: "pod"}, Spec: core.PodSpec{NodeName: "node"}, Status: core.PodStatus{PodIP: "10.244.0.2", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}}
			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: core.NodeSpec{ProviderID: "oci://ocid1.instance.test"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "10.0.0.2"}}}}
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "health", Protocol: core.ProtocolTCP, NodePort: 30901})
			tc.mutate(s, ep, p)
			scheme := runtime.NewScheme()
			_ = core.AddToScheme(scheme)
			_ = discovery.AddToScheme(scheme)
			objects := []client.Object{s, ep, p, node}
			if tc.name == "valid-with-extra-foreign-slice" {
				other := ep.DeepCopy()
				other.Name = "foreign"
				other.OwnerReferences = nil
				objects = append(objects, other)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"}, Spec: api.BindingSpec{Service: "wireguard", PortName: "wg", Mode: "NodePort"}}
			b.Spec.HealthPort = 30901
			backend, err := Resolve(context.Background(), c, b, api.Allocation{ServiceUID: "service"})
			if (err == nil) != tc.valid {
				t.Fatal(backend, err)
			}
			if tc.valid && (backend.Port != 30001 || backend.TargetID != "ocid1.instance.test") {
				t.Fatal(backend)
			}
		})
	}
}

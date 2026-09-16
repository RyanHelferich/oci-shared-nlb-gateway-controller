package internalcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func podIPFixture() (*api.TunnelBinding, *core.Service, *discovery.EndpointSlice, *core.Pod, *core.Node) {
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "managed", UID: "binding", Finalizers: []string{api.Finalizer}}, Spec: api.BindingSpec{Pool: "pool", Service: "tunnel", PortName: "wg", Mode: "PodIP", HealthPort: 8080}}
	s := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "managed", UID: "service"}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, Selector: map[string]string{"app": "tunnel"}, Ports: []core.ServicePort{
		{Name: "wg", Protocol: core.ProtocolUDP, Port: 51821, TargetPort: intstr.FromString("wireguard")},
		{Name: "health", Protocol: core.ProtocolTCP, Port: 80, TargetPort: intstr.FromString("health")},
	}}}
	p := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "managed", UID: "pod", Labels: map[string]string{"app": "tunnel"}}, Spec: core.PodSpec{NodeName: "node", Containers: []core.Container{{Name: "tunnel", Ports: []core.ContainerPort{{Name: "wireguard", Protocol: core.ProtocolUDP, ContainerPort: 51820}, {Name: "health", Protocol: core.ProtocolTCP, ContainerPort: 8080}}}}}, Status: core.PodStatus{PodIP: "10.0.8.2", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}}
	e := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "managed", Labels: map[string]string{discovery.LabelServiceName: s.Name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: s.UID}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: ptr("wg"), Protocol: ptr(core.ProtocolUDP), Port: ptr(int32(51820))}, {Name: ptr("health"), Protocol: ptr(core.ProtocolTCP), Port: ptr(int32(8080))}}, Endpoints: []discovery.Endpoint{{Addresses: []string{p.Status.PodIP}, NodeName: ptr("node"), TargetRef: &core.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID}, Conditions: discovery.EndpointConditions{Ready: ptr(true)}}}}
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}}
	return b, s, e, p, node
}

func podIPScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = core.AddToScheme(s)
	_ = discovery.AddToScheme(s)
	return s
}

func TestPodIPOwnershipAndTargetPorts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		valid  bool
		change func(*api.TunnelBinding, *core.Service, *discovery.EndpointSlice, *core.Pod, *core.Node)
	}{
		{name: "named target ports", valid: true},
		{name: "numeric targets", valid: true, change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Ports[0].TargetPort = intstr.FromInt(51820)
			s.Spec.Ports[1].TargetPort = intstr.FromInt(8080)
			p.Spec.Containers = nil
		}},
		{name: "default target ports", valid: true, change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Ports[0].Port = 51820
			s.Spec.Ports[0].TargetPort = intstr.IntOrString{}
		}},
		{name: "duplicate observation same Pod", valid: true, change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = append(e.Endpoints, *e.Endpoints[0].DeepCopy())
		}},
		{name: "foreign service UID", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.UID = "other"
		}},
		{name: "foreign slice", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.OwnerReferences = nil
		}},
		{name: "stale Pod UID", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.UID = "other"
		}},
		{name: "changed Pod address", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Status.PodIP = "10.0.8.3"
		}},
		{name: "foreign selector", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Labels = nil
		}},
		{name: "selectorless", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Selector = nil
		}},
		{name: "host network", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Spec.HostNetwork = true
		}},
		{name: "NodePort Service", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Type = core.ServiceTypeNodePort
		}},
		{name: "LoadBalancer Service", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
		}},
		{name: "ExternalName Service", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.Type = core.ServiceTypeExternalName
		}},
		{name: "published unready", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			s.Spec.PublishNotReadyAddresses = true
		}},
		{name: "UDP endpoint port mismatch", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Ports[0].Port = ptr(int32(51822))
		}},
		{name: "UDP named port wrong protocol", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Spec.Containers[0].Ports[0].Protocol = core.ProtocolTCP
		}},
		{name: "health endpoint port mismatch", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Ports[1].Port = ptr(int32(8081))
		}},
		{name: "foreign health destination", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			b.Spec.HealthPort = 8081
		}},
		{name: "ambiguous named port", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Spec.Containers = append(p.Spec.Containers, p.Spec.Containers[0])
		}},
		{name: "no endpoints never retained", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
		}},
		{name: "unready endpoint", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Ready = ptr(false)
		}},
		{name: "terminating endpoint", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Terminating = ptr(true)
		}},
		{name: "unready Pod", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Status.Conditions[0].Status = core.ConditionFalse
		}},
		{name: "unready Node", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			n.Status.Conditions[0].Status = core.ConditionFalse
		}},
		{name: "wrong node", change: func(b *api.TunnelBinding, s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].NodeName = ptr("other")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, s, e, p, n := podIPFixture()
			if tc.change != nil {
				tc.change(b, s, e, p, n)
			}
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithObjects(s, e, p, n).Build()
			backend, err := Resolve(context.Background(), c, b, api.Allocation{ServiceUID: "service"})
			if (err == nil) != tc.valid {
				t.Fatalf("backend=%+v err=%v", backend, err)
			}
			var unavailable *BackendUnavailable
			if errors.As(err, &unavailable) {
				t.Fatal("PodIP must not retain removed addresses")
			}
			if tc.valid && *backend != (Backend{IP: "10.0.8.2", Port: 51820}) {
				t.Fatalf("unexpected direct destination %+v", backend)
			}
		})
	}
}

func TestPodIPReconcileCloudAndReadFailure(t *testing.T) {
	for _, scenario := range []string{"replace", "empty", "timeout", "ambiguous", "suspended"} {
		t.Run(scenario, func(t *testing.T) {
			b, s, e, p, node := podIPFixture()
			var lb n.NetworkLoadBalancer
			var updates []n.UpdateBackendSetDetails
			cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(lb)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected mutation %s", r.Method)
				}
				var update n.UpdateBackendSetDetails
				if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
					t.Error(err)
				}
				updates = append(updates, update)
				w.Header().Set("opc-work-request-id", "test-update")
				w.WriteHeader(http.StatusAccepted)
			})
			pool.Name, pool.Namespace = "pool", "managed"
			pool.Finalizers = []string{api.Finalizer}
			pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart = 2, 1, 20000
			frozen := pool.Spec
			pool.Status.FrozenSpec = &frozen
			a := pool.Status.Allocations["binding"]
			a.BindingName, a.ServiceName, a.ServiceUID, a.PortName, a.Mode = b.Name, s.Name, string(s.UID), "wg", "PodIP"
			pool.Status.Allocations["binding"] = a
			lb = cloudModel(cloud, pool, 0)
			bs := convergedSet(&Backend{IP: "10.0.8.99", Port: 51820})
			bs.HealthChecker.Port = ptr(8080)
			lb.BackendSets[a.Name], lb.Listeners[a.Name] = bs, convergedListener(a)
			objects := []client.Object{pool, b, s, e, p, node}
			if scenario == "suspended" {
				b.Spec.Suspended = true
			}
			if scenario == "empty" {
				e.Endpoints = nil
			}
			if scenario == "ambiguous" {
				other := p.DeepCopy()
				other.Name = "other"
				other.UID = "other"
				other.Status.PodIP = "10.0.8.3"
				objects = append(objects, other)
				ep := *e.Endpoints[0].DeepCopy()
				ep.TargetRef.Name, ep.TargetRef.UID = "other", "other"
				ep.Addresses = []string{other.Status.PodIP}
				e.Endpoints = append(e.Endpoints, ep)
			}
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(objects...).Build()
			var reader client.Reader = c
			if scenario == "timeout" {
				reader = endpointErrorReader{Reader: c, getKind: "Pod", err: context.DeadlineExceeded}
			}
			r := &Reconciler{Client: c, Reader: reader, Cloud: cloud, Recorder: record.NewFakeRecorder(20), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			if scenario == "timeout" {
				if err == nil || len(updates) != 0 {
					t.Fatalf("read failure must not mutate: err=%v updates=%v", err, updates)
				}
				return
			}
			if err != nil || len(updates) != 1 {
				t.Fatalf("err=%v updates=%v", err, updates)
			}
			if scenario == "replace" {
				if len(updates[0].Backends) != 1 || value(updates[0].Backends[0].IpAddress) != p.Status.PodIP || value(updates[0].Backends[0].Port) != 51820 || updates[0].Backends[0].TargetId != nil || value(updates[0].HealthChecker.Port) != 8080 {
					t.Fatalf("wrong direct-pod SDK request %+v", updates[0])
				}
			} else if len(updates[0].Backends) != 0 {
				t.Fatal("unsafe or absent Pod must clear old address")
			}
		})
	}
}

func TestBindingModeValidatedBeforeAllocation(t *testing.T) {
	for _, scenario := range []string{"PodIP accepted", "unsupported", "source preservation", "selectorless", "LoadBalancer", "suspended"} {
		t.Run(scenario, func(t *testing.T) {
			b, svc, _, _, _ := podIPFixture()
			calls := 0
			cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected cloud call", 500) })
			pool.Name, pool.Namespace = "pool", "managed"
			pool.Finalizers = []string{api.Finalizer}
			pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart = 2, 1, 20000
			switch scenario {
			case "suspended":
				b.Spec.Suspended = true
			case "unsupported":
				b.Spec.Mode = "unknown"
			case "source preservation":
				pool.Spec.PreserveSource = true
			case "selectorless":
				svc.Spec.Selector = nil
			case "LoadBalancer":
				svc.Spec.Type = core.ServiceTypeLoadBalancer
			}
			frozen := pool.Spec
			pool.Status = api.PoolStatus{FrozenSpec: &frozen}
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(pool, b, svc).Build()
			r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool); err != nil {
				t.Fatal(err)
			}
			_, allocated := pool.Status.Allocations[string(b.UID)]
			if allocated != (scenario == "PodIP accepted") || calls != 0 {
				t.Fatalf("allocated=%v calls=%d", allocated, calls)
			}
		})
	}
}

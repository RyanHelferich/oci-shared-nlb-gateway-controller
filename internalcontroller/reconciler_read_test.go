package internalcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type endpointErrorReader struct {
	client.Reader
	getKind   string
	listError bool
	err       error
	cancel    context.CancelFunc
}

func (r endpointErrorReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	kind := ""
	switch obj.(type) {
	case *core.Service:
		kind = "Service"
	case *core.Pod:
		kind = "Pod"
	case *core.Node:
		kind = "Node"
	}
	if kind != "" && kind == r.getKind {
		if r.cancel != nil {
			r.cancel()
		}
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r endpointErrorReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kind := ""
	switch list.(type) {
	case *core.ServiceList:
		kind = "Service"
	case *core.PodList:
		kind = "Pod"
	case *core.NodeList:
		kind = "Node"
	}
	if kind != "" && kind == r.getKind {
		if r.cancel != nil {
			r.cancel()
		}
		return r.err
	}
	if _, ok := list.(*discovery.EndpointSliceList); ok && r.listError {
		return r.err
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestEndpointReadFailuresDoNotMutateCloud(t *testing.T) {
	for _, tc := range []struct {
		name, getKind, missingKind string
		listError, cancel          bool
		hold, emptyCloud, deleting bool
		change                     func(*core.Service, *discovery.EndpointSlice, *core.Pod, *core.Node)
		err                        error
	}{
		{name: "Service timeout", getKind: "Service", err: apierrors.NewTimeoutError("test timeout", 1)},
		{name: "Service throttled", getKind: "Service", err: apierrors.NewTooManyRequests("test throttling", 1)},
		{name: "Pod timeout", getKind: "Pod", err: context.DeadlineExceeded},
		{name: "Node timeout", getKind: "Node", err: context.DeadlineExceeded},
		{name: "EndpointSlice list timeout", listError: true, err: apierrors.NewTimeoutError("test timeout", 1)},
		{name: "EndpointSlice list throttled", listError: true, err: apierrors.NewTooManyRequests("test throttling", 1)},
		{name: "EndpointSlice API missing", listError: true, err: apierrors.NewNotFound(discovery.Resource("endpointslices"), "")},
		{name: "context canceled during Service read", getKind: "Service", cancel: true, err: context.Canceled},
		{name: "Service truly absent clears backend", missingKind: "Service"},
		{name: "Pod truly absent clears backend", missingKind: "Pod"},
		{name: "Node truly absent clears backend", missingKind: "Node"},
		{name: "Pod terminating before slice update retains backend", hold: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/terminating"}
		}},
		{name: "Pod unready before slice update retains backend", hold: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.Status.Conditions[0].Status = core.ConditionFalse
		}},
		{name: "stale Pod UID never becomes availability gap", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			p.UID = "recreated-pod"
			p.Status.Conditions[0].Status = core.ConditionFalse
		}},
		{name: "empty endpoints retain verified backend", hold: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) { e.Endpoints = nil }},
		{name: "unready endpoint retains verified backend", hold: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Ready = ptr(false)
		}},
		{name: "verified serving terminating endpoint can drain", hold: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Ready = ptr(false)
			e.Endpoints[0].Conditions.Terminating = ptr(true)
			e.Endpoints[0].Conditions.Serving = ptr(true)
		}},
		{name: "empty cloud backend is never resurrected", hold: true, emptyCloud: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) { e.Endpoints = nil }},
		{name: "changed NodePort during gap clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
			s.Spec.Ports[0].NodePort++
		}},
		{name: "changed node identity during gap clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
			n.Spec.ProviderID = "oci://ocid1.instance.other"
		}},
		{name: "node unready during gap clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
			n.Status.Conditions[0].Status = core.ConditionFalse
		}},
		{name: "foreign slice during gap clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
			e.OwnerReferences = nil
		}},
		{name: "recreated Service during gap clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints = nil
			s.UID = "other-service"
		}},
		{name: "unknown readiness clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Ready = nil
		}},
		{name: "ambiguous ready endpoints clear backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			other := *e.Endpoints[0].DeepCopy()
			other.TargetRef.UID = "another-pod"
			e.Endpoints = append(e.Endpoints, other)
		}},
		{name: "stale serving terminating Pod clears backend", change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) {
			e.Endpoints[0].Conditions.Ready = ptr(false)
			e.Endpoints[0].Conditions.Terminating = ptr(true)
			e.Endpoints[0].Conditions.Serving = ptr(true)
			p.UID = "recreated-pod"
		}},
		{name: "binding deletion during gap still deletes listener", deleting: true, change: func(s *core.Service, e *discovery.EndpointSlice, p *core.Pod, n *core.Node) { e.Endpoints = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutations := 0
			mutationMethod := ""
			var updated map[string]json.RawMessage
			cloud, p := cloudFixture(t, func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.Method != http.MethodGet {
					mutations++
					mutationMethod = req.Method
					if req.Method != http.MethodPut && !tc.deleting {
						t.Errorf("unexpected mutation: %s", req.Method)
					}
					if !tc.deleting {
						if err := json.NewDecoder(req.Body).Decode(&updated); err != nil {
							t.Error(err)
						}
					}
					w.Header().Set("opc-work-request-id", "clear-work")
					w.WriteHeader(http.StatusAccepted)
					return
				}
				backends := []any{map[string]any{"name": "t-binding-active", "ipAddress": "10.0.0.2", "targetId": "ocid1.instance.test", "port": 30001, "weight": 1}}
				if tc.emptyCloud {
					backends = []any{}
				}
				json.NewEncoder(w).Encode(map[string]any{
					"id": "lb", "compartmentId": "comp", "subnetId": "sub", "lifecycleState": "ACTIVE",
					"freeformTags": map[string]string{"controller": "independent-shared-nlb", "installation": "test", "pool-uid": "pool", "shard": "0"},
					"backendSets":  map[string]any{"t-binding": map[string]any{"name": "t-binding", "policy": "FIVE_TUPLE", "isFailOpen": false, "isInstantFailoverEnabled": true, "backends": backends, "healthChecker": map[string]any{"protocol": "TCP", "port": 30901, "intervalInMillis": 10000, "timeoutInMillis": 3000, "retries": 3}}},
					"listeners":    map[string]any{"t-binding": map[string]any{"name": "t-binding", "port": 20000, "protocol": "UDP", "defaultBackendSetName": "t-binding", "udpIdleTimeout": 120}},
				})
			})
			p.Name, p.Namespace = "pool", "managed"
			p.Finalizers = []string{api.Finalizer}
			p.Spec.Occupancy, p.Spec.MaxShards, p.Spec.PortStart = 2, 1, 20000
			frozen := p.Spec
			p.Status.FrozenSpec = &frozen
			a := p.Status.Allocations["binding"]
			a.BindingName, a.ServiceName, a.ServiceUID, a.PortName, a.Mode = "binding", "tunnel", "service", "wg", "NodePort"
			p.Status.Allocations["binding"] = a
			b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "managed", UID: "binding", Finalizers: []string{api.Finalizer}}, Spec: api.BindingSpec{Pool: "pool", Service: "tunnel", PortName: "wg", Mode: "NodePort", HealthPort: 30901}}
			svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "managed", UID: "service"}, Spec: core.ServiceSpec{Type: core.ServiceTypeNodePort, ExternalTrafficPolicy: core.ServiceExternalTrafficPolicyLocal, Ports: []core.ServicePort{{Name: "wg", Protocol: core.ProtocolUDP, NodePort: 30001}, {Name: "health", Protocol: core.ProtocolTCP, NodePort: 30901}}}}
			ep := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "managed", Labels: map[string]string{discovery.LabelServiceName: "tunnel"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: "service"}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: ptr("wg"), Protocol: ptr(core.ProtocolUDP), Port: ptr(int32(51820))}}, Endpoints: []discovery.Endpoint{{Addresses: []string{"10.244.0.2"}, Conditions: discovery.EndpointConditions{Ready: ptr(true)}, TargetRef: &core.ObjectReference{Kind: "Pod", Name: "pod", Namespace: "managed", UID: "pod"}}}}
			pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "managed", UID: "pod"}, Spec: core.PodSpec{NodeName: "node"}, Status: core.PodStatus{PodIP: "10.244.0.2", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}}
			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: core.NodeSpec{ProviderID: "oci://ocid1.instance.test"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "10.0.0.2"}}}}
			if tc.change != nil {
				tc.change(svc, ep, pod, node)
			}
			if tc.deleting {
				now := metav1.Now()
				b.DeletionTimestamp = &now
			}
			scheme := runtime.NewScheme()
			_ = api.AddToScheme(scheme)
			_ = core.AddToScheme(scheme)
			_ = discovery.AddToScheme(scheme)
			objects := []client.Object{p, b, ep}
			if tc.missingKind != "Service" {
				objects = append(objects, svc)
			}
			if tc.missingKind != "Pod" {
				objects = append(objects, pod)
			}
			if tc.missingKind != "Node" {
				objects = append(objects, node)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(objects...).Build()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := endpointErrorReader{Reader: c, getKind: tc.getKind, listError: tc.listError, err: tc.err}
			if tc.cancel {
				reader.cancel = cancel
			}
			r := &Reconciler{Client: c, Reader: reader, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
			if tc.err != nil {
				var readError *EndpointReadError
				if !errors.As(err, &readError) || !errors.Is(err, tc.err) || mutations != 0 {
					t.Fatalf("read failure must back off without mutation: err=%v mutations=%d", err, mutations)
				}
			} else if tc.hold {
				if err != nil || mutations != 0 {
					t.Fatalf("verified gap must not mutate cloud: err=%v mutations=%d", err, mutations)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
					t.Fatal(err)
				}
				if len(b.Status.Conditions) != 1 || b.Status.Conditions[0].Status != metav1.ConditionFalse || !strings.Contains(b.Status.Conditions[0].Message, "waiting for one ready endpoint") {
					t.Fatalf("gap must publish waiting status: %#v", b.Status)
				}
			} else if tc.deleting {
				if err != nil || mutations != 1 || mutationMethod != http.MethodDelete {
					t.Fatalf("binding deletion must remove listener: %v %d %s", err, mutations, mutationMethod)
				}
			} else {
				if err != nil || mutations != 1 || string(updated["backends"]) != "[]" {
					t.Fatalf("proven identity absence must clear backend: err=%v mutations=%d body=%s", err, mutations, updated["backends"])
				}
			}
		})
	}
}

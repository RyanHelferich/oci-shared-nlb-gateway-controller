package internalcontroller

import (
	"context"
	"fmt"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type countingEndpointReader struct {
	client.Reader
	gets, lists int
}

func (r *countingEndpointReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	return r.Reader.Get(ctx, key, obj, opts...)
}
func (r *countingEndpointReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	return r.Reader.List(ctx, list, opts...)
}

func TestEndpointSnapshot1200BindingsUsesFourBulkReadsAndRefreshes(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)
	_ = discovery.AddToScheme(scheme)
	objects := []client.Object{&core.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: core.NodeSpec{ProviderID: "oci://ocid1.instance.test"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "10.0.0.2"}}}}}
	for i := 0; i < 1200; i++ {
		name := fmt.Sprintf("tunnel-%d", i)
		uid := types.UID(name)
		objects = append(objects,
			&core.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "managed", UID: uid}, Spec: core.ServiceSpec{Type: core.ServiceTypeNodePort, ExternalTrafficPolicy: core.ServiceExternalTrafficPolicyLocal, Ports: []core.ServicePort{{Name: "wg", Protocol: core.ProtocolUDP, NodePort: int32(30000 + i)}, {Name: "health", Protocol: core.ProtocolTCP, NodePort: int32(32000 + i)}}}},
			&core.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "managed", UID: uid}, Spec: core.PodSpec{NodeName: "node"}, Status: core.PodStatus{PodIP: "10.244.0.2", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}},
			&discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "managed", Labels: map[string]string{discovery.LabelServiceName: name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: uid}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: ptr("wg"), Protocol: ptr(core.ProtocolUDP), Port: ptr(int32(51820))}}, Endpoints: []discovery.Endpoint{{Addresses: []string{"10.244.0.2"}, Conditions: discovery.EndpointConditions{Ready: ptr(true)}, TargetRef: &core.ObjectReference{Kind: "Pod", Name: name, Namespace: "managed", UID: uid}}}},
		)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reader := &countingEndpointReader{Reader: c}
	snapshot, err := newEndpointSnapshot(ctx, reader, "managed")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		name := fmt.Sprintf("tunnel-%d", i)
		b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "managed"}, Spec: api.BindingSpec{Service: name, PortName: "wg", Mode: "NodePort", HealthPort: 32000 + i}}
		backend, err := Resolve(ctx, snapshot, b, api.Allocation{ServiceUID: name})
		if err != nil || backend.Port != 30000+i {
			t.Fatalf("binding %d failed: backend=%v err=%v", i, backend, err)
		}
	}
	if reader.gets != 0 || reader.lists != 4 {
		t.Fatalf("expected four bulk reads for 1200 bindings, got Get=%d List=%d", reader.gets, reader.lists)
	}
	// The next reconciliation must see deletion/recreation under the same name.
	svc := &core.Service{}
	key := client.ObjectKey{Namespace: "managed", Name: "tunnel-0"}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	svc.UID, svc.ResourceVersion = "replacement-service", ""
	if err := c.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	fresh, err := newEndpointSnapshot(ctx, reader, "managed")
	if err != nil {
		t.Fatal(err)
	}
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "managed"}, Spec: api.BindingSpec{Service: "tunnel-0", PortName: "wg", Mode: "NodePort", HealthPort: 32000}}
	if backend, err := Resolve(ctx, fresh, b, api.Allocation{ServiceUID: "tunnel-0"}); err == nil || backend != nil {
		t.Fatal("fresh snapshot accepted a replaced Service UID")
	}
	if reader.lists != 8 {
		t.Fatalf("next reconciliation must refresh all four resources, got %d", reader.lists)
	}
}

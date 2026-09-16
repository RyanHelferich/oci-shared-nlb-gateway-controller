package internalcontroller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkerGapAndReplacementMembershipSequence(t *testing.T) {
	for _, loseNodeReadiness := range []bool{false, true} {
		name := "ready old worker retains until direct replacement"
		if loseNodeReadiness {
			name = "NotReady old worker still clears before replacement"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var lb n.NetworkLoadBalancer
			var updates []n.UpdateBackendSetDetails
			cloud, pool := cloudFixture(t, func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(req.URL.Path, "/workRequests/") {
					json.NewEncoder(w).Encode(map[string]any{"status": "SUCCEEDED"})
					return
				}
				if req.Method == http.MethodGet {
					json.NewEncoder(w).Encode(lb)
					return
				}
				if req.Method != http.MethodPut || !strings.HasSuffix(req.URL.Path, "/backendSets/t-binding") {
					t.Errorf("unexpected mutation: %s %s", req.Method, req.URL.Path)
				}
				var update n.UpdateBackendSetDetails
				if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
					t.Error(err)
				}
				updates = append(updates, update)
				raw, _ := json.Marshal(update)
				var bs n.BackendSet
				if err := json.Unmarshal(raw, &bs); err != nil {
					t.Error(err)
				}
				lb.BackendSets["t-binding"] = bs
				w.Header().Set("opc-work-request-id", "transition-work")
				w.WriteHeader(http.StatusAccepted)
			})
			pool.Name, pool.Namespace = "pool", "managed"
			pool.Finalizers = []string{api.Finalizer}
			pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart = 2, 1, 20000
			frozen := pool.Spec
			pool.Status.FrozenSpec = &frozen
			a := pool.Status.Allocations["binding"]
			a.BindingName, a.ServiceName, a.ServiceUID, a.PortName, a.Mode = "binding", "tunnel", "service", "wg", "NodePort"
			pool.Status.Allocations["binding"] = a
			oldBackend := &Backend{IP: "10.0.0.2", TargetID: "ocid1.instance.old", Port: 30001}
			lb = cloudModel(cloud, pool, 0)
			lb.BackendSets[a.Name], lb.Listeners[a.Name] = convergedSet(oldBackend), convergedListener(a)
			binding := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "managed", UID: "binding", Finalizers: []string{api.Finalizer}}, Spec: api.BindingSpec{Pool: "pool", Service: "tunnel", PortName: "wg", Mode: "NodePort", HealthPort: 30901}}
			svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "managed", UID: "service"}, Spec: core.ServiceSpec{Type: core.ServiceTypeNodePort, ExternalTrafficPolicy: core.ServiceExternalTrafficPolicyLocal, Ports: []core.ServicePort{{Name: "wg", Protocol: core.ProtocolUDP, NodePort: 30001}, {Name: "health", Protocol: core.ProtocolTCP, NodePort: 30901}}}}
			oldPod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod", Namespace: "managed", UID: "old-pod"}, Spec: core.PodSpec{NodeName: "old-node"}, Status: core.PodStatus{PodIP: "10.244.0.2", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}}
			oldNode := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: "old-node"}, Spec: core.NodeSpec{ProviderID: "oci://ocid1.instance.old"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: oldBackend.IP}}}}
			newNode := oldNode.DeepCopy()
			newNode.Name = "new-node"
			newNode.Spec.ProviderID = "oci://ocid1.instance.new"
			newNode.Status.Addresses[0].Address = "10.0.0.3"
			oldEndpoint := discovery.Endpoint{Addresses: []string{oldPod.Status.PodIP}, NodeName: ptr(oldNode.Name), Conditions: discovery.EndpointConditions{Ready: ptr(true)}, TargetRef: &core.ObjectReference{Kind: "Pod", Name: oldPod.Name, Namespace: "managed", UID: oldPod.UID}}
			slice := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "managed", Labels: map[string]string{discovery.LabelServiceName: svc.Name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: svc.UID}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: ptr("wg"), Protocol: ptr(core.ProtocolUDP), Port: ptr(int32(51820))}}, Endpoints: []discovery.Endpoint{oldEndpoint}}
			scheme := runtime.NewScheme()
			_ = api.AddToScheme(scheme)
			_ = core.AddToScheme(scheme)
			_ = discovery.AddToScheme(scheme)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}, &core.Node{}).WithObjects(pool, binding, svc, oldPod, oldNode, newNode, slice).Build()
			r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(20), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			reconcile := func() {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			if len(updates) != 0 {
				t.Fatal("converged old worker was changed")
			}
			slice.Endpoints = nil
			if err := c.Update(ctx, slice); err != nil {
				t.Fatal(err)
			}
			if loseNodeReadiness {
				oldNode.Status.Conditions[0].Status = core.ConditionFalse
				if err := c.Status().Update(ctx, oldNode); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			beforeReplacement := 0
			if loseNodeReadiness {
				beforeReplacement = 1
				if len(updates) != 1 || len(updates[0].Backends) != 0 {
					t.Fatal("NotReady worker must preserve current clear behavior")
				}
				reconcile() // Persist terminal work completion before next mutation.
			} else if len(updates) != 0 {
				t.Fatal("verified gap caused an empty-backend update")
			}
			if err := c.Delete(ctx, oldPod); err != nil {
				t.Fatal(err)
			}
			newPod := oldPod.DeepCopy()
			newPod.Name, newPod.UID, newPod.ResourceVersion = "new-pod", "new-pod", ""
			newPod.Spec.NodeName = newNode.Name
			newPod.Status.PodIP = "10.244.1.2"
			if err := c.Create(ctx, newPod); err != nil {
				t.Fatal(err)
			}
			oldEndpoint.Conditions = discovery.EndpointConditions{Ready: ptr(false), Terminating: ptr(true), Serving: ptr(true)}
			newEndpoint := discovery.Endpoint{Addresses: []string{newPod.Status.PodIP}, NodeName: ptr(newNode.Name), Conditions: discovery.EndpointConditions{Ready: ptr(true)}, TargetRef: &core.ObjectReference{Kind: "Pod", Name: newPod.Name, Namespace: "managed", UID: newPod.UID}}
			// A stale terminating reference to the now-absent old Pod must not
			// prevent choosing the verified ready replacement on the new worker.
			slice.Endpoints = []discovery.Endpoint{oldEndpoint, newEndpoint}
			if err := c.Update(ctx, slice); err != nil {
				t.Fatal(err)
			}
			reconcile()
			if len(updates) != beforeReplacement+1 {
				t.Fatalf("expected one direct replacement update, got %d", len(updates))
			}
			last := updates[len(updates)-1]
			if len(last.Backends) != 1 || value(last.Backends[0].IpAddress) != "10.0.0.3" || value(last.Backends[0].TargetId) != "ocid1.instance.new" || value(last.Backends[0].Port) != 30001 {
				t.Fatalf("wrong replacement membership: %#v", last.Backends)
			}
			reconcile()
			reconcile() // Complete work, then observe convergence.
			if len(updates) != beforeReplacement+1 {
				t.Fatal("converged replacement triggered another mutation")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
				t.Fatal(err)
			}
			if len(binding.Status.Conditions) != 1 || binding.Status.Conditions[0].Status != metav1.ConditionTrue {
				t.Fatalf("replacement did not converge: %#v", binding.Status)
			}
			foreign := slice.DeepCopy()
			foreign.Name, foreign.ResourceVersion = "foreign", ""
			foreign.OwnerReferences = nil
			if err := c.Create(ctx, foreign); err != nil {
				t.Fatal(err)
			}
			reconcile()
			if len(updates) != beforeReplacement+2 || len(updates[len(updates)-1].Backends) != 0 {
				t.Fatal("foreign labelled slice must still clear a valid replacement")
			}
		})
	}
}

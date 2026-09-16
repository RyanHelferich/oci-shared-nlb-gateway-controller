package internalcontroller

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPodIPAliasClearsOnlyItsOldRouteAndDoesNotBlockSibling(t *testing.T) {
	var lb n.NetworkLoadBalancer
	updates := map[string][]n.UpdateBackendSetDetails{}
	gets := 0
	cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/workRequests/") {
			json.NewEncoder(w).Encode(map[string]any{"status": "SUCCEEDED"})
			return
		}
		if r.Method == http.MethodGet {
			gets++
			json.NewEncoder(w).Encode(lb)
			return
		}
		if r.Method != http.MethodPut {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		name := path.Base(r.URL.Path)
		var update n.UpdateBackendSetDetails
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Error(err)
		}
		updates[name] = append(updates[name], update)
		raw, _ := json.Marshal(update)
		var set n.BackendSet
		if err := json.Unmarshal(raw, &set); err != nil {
			t.Error(err)
		}
		lb.BackendSets[name] = set
		w.Header().Set("opc-work-request-id", "test-work")
		w.WriteHeader(http.StatusAccepted)
	})
	pool.Name, pool.Namespace = "pool", "managed"
	pool.Finalizers = []string{api.Finalizer}
	pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart = 3, 1, 20000
	frozen := pool.Spec
	pool.Status.FrozenSpec = &frozen
	pool.Status.Allocations = map[string]api.Allocation{}
	lb = cloudModel(cloud, pool, 0)
	objects := []client.Object{pool}
	for index, name := range []string{"a-alias", "b-owner", "z-sibling"} {
		b, s, e, pod, node := podIPFixture()
		b.Name, b.UID, b.Spec.Service = name, types.UID(name), name
		s.Name, s.UID = name, types.UID("service-"+name)
		e.Name, e.Labels["kubernetes.io/service-name"], e.OwnerReferences[0].UID = name, name, s.UID
		if name == "z-sibling" {
			pod.Name, pod.UID, pod.Status.PodIP = "sibling-pod", "sibling-pod", "10.0.8.3"
			e.Endpoints[0].TargetRef.Name, e.Endpoints[0].TargetRef.UID = pod.Name, pod.UID
			e.Endpoints[0].Addresses = []string{pod.Status.PodIP}
		}
		if name != "a-alias" {
			objects = append(objects, pod)
		}
		if index == 0 {
			objects = append(objects, node)
		}
		objects = append(objects, b, s, e)
		a := api.Allocation{BindingName: name, ServiceName: name, ServiceUID: string(s.UID), PortName: "wg", Mode: "PodIP", Shard: 0, Port: 20000 + index, Name: name}
		pool.Status.Allocations[string(b.UID)] = a
		oldIP := "10.0.8.2"
		if name == "a-alias" {
			oldIP = "10.0.8.98"
		}
		if name == "z-sibling" {
			oldIP = "10.0.8.99"
		}
		set := convergedSet(&Backend{IP: oldIP, Port: 51820})
		set.HealthChecker.Port = ptr(8080)
		lb.BackendSets[name], lb.Listeners[name] = set, convergedListener(a)
	}
	ownerBefore := lb.BackendSets["b-owner"]
	c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(objects...).Build()
	r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(30), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
	for i := 0; i < 5; i++ {
		gets = 0
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
			t.Fatal(err)
		}
		if gets > 1 {
			t.Fatalf("duplicate guard bypassed shard snapshot: %d native reads", gets)
		}
	}
	if len(updates["a-alias"]) != 1 || len(updates["a-alias"][0].Backends) != 0 {
		t.Fatalf("alias old route not cleared: %+v", updates)
	}
	if len(updates["b-owner"]) != 0 || !reflect.DeepEqual(ownerBefore, lb.BackendSets["b-owner"]) {
		t.Fatal("alias modified destination owner")
	}
	if len(updates["z-sibling"]) != 1 || value(updates["z-sibling"][0].Backends[0].IpAddress) != "10.0.8.3" {
		t.Fatal("conflict stalled unrelated sibling")
	}
	for _, name := range []string{"a-alias", "b-owner", "z-sibling"} {
		b := &api.TunnelBinding{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "managed", Name: name}, b); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(b.Status.Conditions, "Configured")
		want := metav1.ConditionTrue
		if name == "a-alias" {
			want = metav1.ConditionFalse
		}
		if condition == nil || condition.Status != want {
			t.Fatalf("%s condition=%+v", name, condition)
		}
	}
}

func TestPodIPDestinationConflictIsShardLocal(t *testing.T) {
	var lb n.NetworkLoadBalancer
	cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(lb) })
	a := pool.Status.Allocations["binding"]
	lb = cloudModel(cloud, pool, 0)
	backend := &Backend{IP: "10.0.8.2", Port: 51820}
	lb.BackendSets[a.Name] = convergedSet(backend)
	conflict, err := cloud.destinationConflict(context.Background(), pool, a, backend)
	if err != nil || conflict {
		t.Fatalf("own backend is not a conflict: %v %v", conflict, err)
	}
	// A matching destination in another set is rejected even if its backend
	// name or target identity differs; it must not be treated as our own.
	lb.BackendSets["another"] = convergedSet(backend)
	cloud.ResetSnapshot()
	conflict, err = cloud.destinationConflict(context.Background(), pool, a, backend)
	if err != nil || !conflict {
		t.Fatalf("other set destination was missed: %v %v", conflict, err)
	}
	// The matching destination is legal on a different NLB. Only that shard's
	// owned snapshot is considered, never another shard's destination set.
	pool.Status.Shards = append(pool.Status.Shards, api.Shard{ID: "other-lb"})
	a.Shard = len(pool.Status.Shards) - 1
	lb = cloudModel(cloud, pool, a.Shard)
	cloud.ResetSnapshot()
	conflict, err = cloud.destinationConflict(context.Background(), pool, a, backend)
	if err != nil || conflict {
		t.Fatalf("another NLB must not inherit the conflict: %v %v", conflict, err)
	}
}

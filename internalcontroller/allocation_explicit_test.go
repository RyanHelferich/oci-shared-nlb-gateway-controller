package internalcontroller

import (
	"context"
	"encoding/json"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestExplicitLeaseSurvivesRestartAndDeletion(t *testing.T) {
	p := &api.NLBPool{Spec: api.PoolSpec{CompartmentID: "c", SubnetID: "s", AllocationMode: "Explicit", MaxShards: 1, Occupancy: 2, PortStart: 1}}
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: "route-a"}, Spec: api.BindingSpec{RequestedPort: 53}}
	a, err := Allocate(p, b, "svc-a")
	if err != nil || a.Port != 53 || a.Shard != 0 {
		t.Fatal(a, err)
	}
	raw, _ := json.Marshal(p)
	p = &api.NLBPool{}
	_ = json.Unmarshal(raw, p)
	if restored, err := Allocate(p, b, "svc-a"); err != nil || restored != a {
		t.Fatal(restored, err)
	}
	a.Tombstone = true
	p.Status.Allocations[string(b.UID)] = a
	b.UID = "route-b"
	if _, err := Allocate(p, b, "svc-b"); err == nil {
		t.Fatal("reused retired explicit port")
	}
	b.Spec.RequestedPort = 65535
	if _, err := Allocate(p, b, "svc-b"); err != nil {
		t.Fatal(err)
	}
	b.UID = "route-c"
	b.Spec.RequestedPort = 1
	if _, err := Allocate(p, b, "svc-c"); err == nil {
		t.Fatal("ignored occupancy including tombstones")
	}
}

func TestExplicitValidationAndMutation(t *testing.T) {
	p := &api.NLBPool{Spec: api.PoolSpec{CompartmentID: "c", SubnetID: "s", AllocationMode: "Explicit", MaxShards: 1, Occupancy: 50, PortStart: 1}}
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: "b"}, Spec: api.BindingSpec{RequestedPort: 65536}}
	for _, port := range []int{-1, 0, 65536} {
		b.Spec.RequestedPort = port
		if _, err := Allocate(p, b, "svc"); err == nil {
			t.Fatal("accepted invalid explicit port", port)
		}
	}
	b.Spec.RequestedPort = 51820
	if _, err := Allocate(p, b, "svc"); err != nil {
		t.Fatal(err)
	}
	b.Spec.RequestedPort = 51821
	if _, err := Allocate(p, b, "svc"); err == nil {
		t.Fatal("changed existing port")
	}
	p.Spec.MaxShards = 2
	if ValidatePool(p.Spec) == nil {
		t.Fatal("Explicit multiple shards")
	}
	p.Spec.MaxShards = 1
	p.Spec.AllocationMode = "Dynamic"
	p.Spec.PortStart = 20000
	if _, err := Allocate(p, b, "svc"); err == nil {
		t.Fatal("dynamic accepted requested port")
	}
	p.Spec.AllocationMode = "unknown"
	if ValidatePool(p.Spec) == nil {
		t.Fatal("unknown mode")
	}
}

func TestEagerEmptyPoolJournalsCreateAndDoesNotCreateWhenDeleting(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "provision", true: "delete"}[deleting], func(t *testing.T) {
			calls := []string{}
			cloud, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
					return
				}
				if r.Method != "POST" {
					t.Error("unexpected", r.Method)
					return
				}
				w.Header().Set("opc-work-request-id", "create-work")
				w.WriteHeader(202)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-lb"})
			})
			p.Name = "gateway-pool"
			p.Namespace = "managed"
			p.Spec.Occupancy = 50
			p.Spec.MaxShards = 1
			p.Spec.PortStart = 1
			p.Spec.AllocationMode = "Explicit"
			p.Spec.ProvisionEmpty = true
			frozen := p.Spec
			p.Status = api.PoolStatus{FrozenSpec: &frozen}
			p.Finalizers = []string{api.Finalizer}
			if deleting {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			}
			scheme := runtime.NewScheme()
			_ = api.AddToScheme(scheme)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}).WithObjects(p).Build()
			reconciler := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
				t.Fatal(err)
			}
			if deleting {
				if len(calls) != 0 {
					t.Fatal("deleting empty pool created NLB", calls)
				}
				return
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), p); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || p.Status.PendingWork != "create-work" || len(p.Status.Shards) != 1 || p.Status.Shards[0].ID != "new-lb" {
				t.Fatal(calls, p.Status)
			}
		})
	}
}

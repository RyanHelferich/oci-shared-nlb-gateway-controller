package internalcontroller

import (
	"context"
	"encoding/json"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
	"testing"
)

func TestRejectedUnallocatedBindingCanBeDeleted(t *testing.T) {
	for _, test := range []struct {
		name, problem                      string
		exhausted, service, otherFinalizer bool
	}{
		{name: "missing service", problem: "not found"},
		{name: "exhausted pool", problem: "pool exhausted", exhausted: true, service: true},
		{name: "preserve unrelated finalizer", problem: "not found", otherFinalizer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			cloudCalls := 0
			cloud, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				cloudCalls++
				http.Error(w, "unexpected cloud call", http.StatusBadRequest)
			})
			p.Name, p.Namespace = "pool", "managed"
			p.Finalizers = []string{api.Finalizer}
			p.Spec.Occupancy, p.Spec.MaxShards, p.Spec.PortStart = 1, 1, 20000
			frozen := p.Spec
			p.Status = api.PoolStatus{FrozenSpec: &frozen}
			if test.exhausted {
				p.Status.Allocations = map[string]api.Allocation{"retired": {Shard: 0, Port: 20000, Name: "retired", Tombstone: true}}
			}
			b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "rejected", Namespace: "managed", UID: "rejected-uid"}, Spec: api.BindingSpec{Pool: "pool", Service: "tunnel", Mode: "NodePort", PortName: "wireguard", HealthPort: 31001}}
			if test.otherFinalizer {
				b.Finalizers = []string{"example.org/other-owner"}
			}
			scheme := runtime.NewScheme()
			_ = api.AddToScheme(scheme)
			_ = core.AddToScheme(scheme)
			objects := []client.Object{p, b}
			if test.service {
				objects = append(objects, &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "managed", UID: "service-uid"}, Spec: core.ServiceSpec{Type: core.ServiceTypeNodePort}})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(objects...).Build()
			r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			reconcile := func() {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
					t.Fatal(err)
				}
			}
			reconcile() // Install our finalizer before attempting allocation.
			reconcile() // Allocation is rejected, leaving the finalizer installed.
			if err := c.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(b.Status.Conditions, "Configured")
			if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, test.problem) || !controllerutil.ContainsFinalizer(b, api.Finalizer) {
				t.Fatalf("expected rejected binding with cleanup finalizer, got %#v", b)
			}
			if err := c.Delete(ctx, b); err != nil {
				t.Fatal(err)
			}
			reconcile()
			err := c.Get(ctx, client.ObjectKeyFromObject(b), b)
			if test.otherFinalizer {
				if err != nil || len(b.Finalizers) != 1 || b.Finalizers[0] != "example.org/other-owner" {
					t.Fatalf("unrelated finalizer must survive: binding=%#v error=%v", b, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("unallocated binding should finish deletion, got %v; finalizers=%v", err, b.Finalizers)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				t.Fatal(err)
			}
			if _, allocated := p.Status.Allocations["rejected-uid"]; allocated || cloudCalls != 0 {
				t.Fatalf("rejected binding must not allocate or mutate OCI: allocated=%v calls=%d", allocated, cloudCalls)
			}
		})
	}
}

func TestReconcileRepairsResurrectedRetiredListener(t *testing.T) {
	deletes := []string{}
	cloud, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			deletes = append(deletes, r.URL.Path)
			w.Header().Set("opc-work-request-id", "cleanup-work")
			w.WriteHeader(202)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "lb", "compartmentId": "comp", "subnetId": "sub", "lifecycleState": "ACTIVE", "freeformTags": map[string]string{"controller": "independent-shared-nlb", "installation": "test", "pool-uid": "pool", "shard": "0", "project": "test"}, "listeners": map[string]any{"t-binding": map[string]any{"name": "t-binding", "port": 20000, "protocol": "UDP", "defaultBackendSetName": "t-binding"}}})
	})
	p.Name = "pool"
	p.Namespace = "managed"
	p.Finalizers = []string{api.Finalizer}
	p.Spec.Occupancy = 2
	p.Spec.MaxShards = 1
	p.Spec.PortStart = 20000
	frozen := p.Spec
	p.Status.FrozenSpec = &frozen
	a := p.Status.Allocations["binding"]
	a.Tombstone = true
	p.Status.Allocations["binding"] = a
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(p).Build()
	r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Namespace: "managed", Compartment: "comp", Subnet: "sub"}
	key := types.NamespacedName{Namespace: "managed", Name: "pool"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || !strings.HasSuffix(deletes[0], "/listeners/t-binding") {
		t.Fatalf("unexpected deletion(s): %v", deletes)
	}
	saved := &api.NLBPool{}
	if err = c.Get(context.Background(), key, saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status.PendingWork != "cleanup-work" || !saved.Status.Allocations["binding"].Tombstone {
		t.Fatal("lost cleanup intent or tombstone")
	}
	if saved.UID != "pool" {
		t.Fatal("pool unexpectedly replaced")
	}
}

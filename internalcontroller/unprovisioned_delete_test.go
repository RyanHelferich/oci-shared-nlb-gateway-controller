package internalcontroller

import (
	"context"
	"net/http"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A binding whose lease was persisted but whose shard never received an NLB
// must be tombstoned on deletion without provisioning a retained NLB.
func TestDeletingBindingOnUnprovisionedShardNeverCreatesNLB(t *testing.T) {
	ctx := context.Background()
	creates := 0
	cloud, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates++
		}
		http.Error(w, "unexpected cloud call", http.StatusBadRequest)
	})
	p.Name, p.Namespace = "pool", "managed"
	p.Finalizers = []string{api.Finalizer}
	p.Spec.Occupancy, p.Spec.MaxShards, p.Spec.PortStart = 2, 1, 20000
	frozen := p.Spec
	p.Status = api.PoolStatus{FrozenSpec: &frozen, Allocations: map[string]api.Allocation{
		"leased-uid": {Shard: 0, Port: 20000, Name: "t-leased", ServiceName: "tunnel", ServiceUID: "service-uid", PortName: "wireguard", Mode: "NodePort"},
	}}
	now := metav1.Now()
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "leased", Namespace: "managed", UID: "leased-uid", DeletionTimestamp: &now, Finalizers: []string{api.Finalizer}}, Spec: api.BindingSpec{Pool: "pool", Service: "tunnel", Mode: "NodePort", PortName: "wireguard", HealthPort: 31001}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = core.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(p, b).Build()
	r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 0 {
		t.Fatalf("deleting an unprovisioned binding created %d NLB(s)", creates)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if a := p.Status.Allocations["leased-uid"]; !a.Tombstone {
		t.Fatalf("allocation must be tombstoned, got %#v", a)
	}
	if len(p.Status.Shards) != 0 {
		t.Fatalf("no shard may be recorded, got %#v", p.Status.Shards)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(b), b); !apierrors.IsNotFound(err) {
		t.Fatalf("binding should finish deletion, got %v finalizers=%v", err, b.Finalizers)
	}
}

func TestTruncateUTF8NeverSplitsRune(t *testing.T) {
	s := "ééééé" // each é is two bytes
	if got := truncateUTF8(s, 5); got != "éé" {
		t.Fatalf("expected two runes, got %q", got)
	}
	if got := truncateUTF8("abc", 10); got != "abc" {
		t.Fatalf("short strings pass through, got %q", got)
	}
}

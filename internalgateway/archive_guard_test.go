package internalgateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestPoolArchiveGuardProtectsForegroundCascade(t *testing.T) {
	ctx := context.Background()
	f := setup(t, func(f *fixture) {
		f.route.Spec.ParentRefs = nil
		f.route.Finalizers = nil
		f.pool.Status.Allocations = map[string]api.Allocation{"retired": {BindingName: "gone", Port: 51820, Tombstone: true}}
	})
	// Simulate the API garbage collector reaching the child before the Gateway
	// controller observes its deleting parent. A parent finalizer cannot stop this.
	if err := f.c.Delete(ctx, f.g); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	archive := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retired-" + string(f.g.UID), Namespace: "managed"}}
	if err := f.c.Get(ctx, client.ObjectKeyFromObject(archive), archive); !errors.IsNotFound(err) {
		t.Fatalf("archived before core completed: %v", err)
	}
	readObject(t, f.c, f.pool)
	if !controllerutil.ContainsFinalizer(f.pool, PoolArchiveFinalizer) {
		t.Fatal("guard removed before cloud cleanup")
	}
	// Core completes its cloud journal and tombstone checks, removing only its
	// own finalizer. The pool must survive this exact point in the live race.
	controllerutil.RemoveFinalizer(f.pool, api.Finalizer)
	if err := f.c.Update(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	readObject(t, f.c, f.pool)
	f.reconcile(t)
	readObject(t, f.c, archive)
	var p api.NLBPool
	if err := json.Unmarshal([]byte(archive.Data["pool.json"]), &p); err != nil {
		t.Fatal(err)
	}
	if p.UID != f.pool.UID || p.Kind != "NLBPool" || !p.Status.Allocations["retired"].Tombstone || p.Status.Shards[0].ID != "owned-nlb" || len(archive.OwnerReferences) != 0 || archive.Immutable == nil || !*archive.Immutable {
		t.Fatal("incomplete durable archive")
	}
	if err := f.c.Get(ctx, client.ObjectKeyFromObject(f.pool), f.pool); !errors.IsNotFound(err) {
		t.Fatalf("pool guard not released after archive: %v", err)
	}
	f.reconcile(t)
	if err := f.c.Get(ctx, client.ObjectKeyFromObject(f.g), f.g); !errors.IsNotFound(err) {
		t.Fatalf("Gateway did not finish: %v", err)
	}
}

func TestPoolArchiveGuardUpgradeAndCollision(t *testing.T) {
	ctx := context.Background()
	f := setup(t, func(f *fixture) { f.pool.Finalizers = []string{api.Finalizer} })
	if _, err := f.r.ensurePool(ctx, f.g); err != nil {
		t.Fatal(err)
	}
	readObject(t, f.c, f.pool)
	if !controllerutil.ContainsFinalizer(f.pool, PoolArchiveFinalizer) {
		t.Fatal("existing owned pool not protected")
	}
	// An immutable foreign archive must never authorize guard removal.
	if err := f.c.Delete(ctx, f.g); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	readObject(t, f.c, f.pool)
	controllerutil.RemoveFinalizer(f.pool, api.Finalizer)
	if err := f.c.Update(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	immutable := true
	if err := f.c.Create(ctx, &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retired-" + string(f.g.UID), Namespace: "managed"}, Immutable: &immutable, Data: map[string]string{"pool.json": "{}"}}); err != nil {
		t.Fatal(err)
	}
	readObject(t, f.c, f.g)
	if err := f.r.retireGateway(ctx, f.g, nil); err == nil {
		t.Fatal("archive collision accepted")
	}
	readObject(t, f.c, f.pool)
	if !controllerutil.ContainsFinalizer(f.pool, PoolArchiveFinalizer) {
		t.Fatal("collision released ledger")
	}
}

func TestGeneratedPoolStartsProtected(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	f.pool.Finalizers = nil
	if err := f.c.Update(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	f.g.Annotations = nil
	if err := f.c.Update(ctx, f.g); err != nil {
		t.Fatal(err)
	}
	p, err := f.r.ensurePool(ctx, f.g)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(p, api.Finalizer) || !controllerutil.ContainsFinalizer(p, PoolArchiveFinalizer) {
		t.Fatal("pool initially exposed to garbage collection")
	}
}

package internalgatewaypool

import (
	"context"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// A pool must stay deletable after the allocator's class was renamed or after
// a rejected spec edit; otherwise the finalizer wedges it in Terminating and
// generated Gateways (and their NLBs) are never retired.
func TestPoolDeletionProceedsDespiteClassRenameOrSpecDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *fixture)
	}{
		{name: "class renamed", mutate: func(f *fixture) { f.r.ClassName = "renamed-class" }},
		{name: "spec drifted from frozen", mutate: func(f *fixture) {
			p := f.pool()
			p.Spec.MaxGateways++
			if err := f.c.Update(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1)
			f.steps(20)
			if len(f.gateways()) != 1 {
				t.Fatalf("expected one generated Gateway, got %d", len(f.gateways()))
			}
			tc.mutate(f)
			if err := f.step(); err == nil {
				t.Fatal("live pool outside scope must still be rejected")
			}
			p := f.pool()
			if err := f.c.Delete(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			f.steps(6)
			if len(f.gateways()) != 0 {
				t.Fatalf("generated Gateway must be deleted during cleanup, still have %d", len(f.gateways()))
			}
			err := f.c.Get(context.Background(), client.ObjectKeyFromObject(p), &api.GatewayPool{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("pool deletion did not finish: %v", err)
			}
		})
	}
}

// A second installation sharing the namespace must never archive or finalize a
// pool it did not freeze, even though the finalizer name is shared.
func TestForeignInstallationNeverFinalizesPool(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	if got := f.pool().Status.Installation; got != "test-install" {
		t.Fatalf("expected recorded installation, got %q", got)
	}
	foreign := &Reconciler{Client: f.c, Reader: f.c.Client, Namespace: "managed", Installation: "other-install", ClassName: "native-udp"}
	if err := f.c.Delete(context.Background(), f.pool()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := foreign.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.p)}); err == nil {
			t.Fatal("foreign installation must refuse to finalize")
		}
	}
	archives := &core.ConfigMapList{}
	_ = f.c.List(context.Background(), archives)
	if len(archives.Items) != 0 || len(f.gateways()) != 1 || !controllerutil.ContainsFinalizer(f.pool(), Finalizer) {
		t.Fatalf("foreign installation mutated pool state: archives=%d gateways=%d", len(archives.Items), len(f.gateways()))
	}
	f.steps(6) // the owning installation still completes deletion
	if err := f.c.Get(context.Background(), client.ObjectKeyFromObject(f.p), &api.GatewayPool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owning installation could not finish deletion: %v", err)
	}
}

// Pools frozen before installation scoping existed are finalized by class match.
func TestLegacyPoolWithoutRecordedInstallationFinalizesByClass(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	p := f.pool()
	p.Status.Installation = ""
	if err := f.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.steps(2)
	if got := f.pool().Status.Installation; got != "test-install" {
		t.Fatalf("expected backfilled installation, got %q", got)
	}
}

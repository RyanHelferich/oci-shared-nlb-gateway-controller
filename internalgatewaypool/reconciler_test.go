package internalgatewaypool

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gw "sigs.k8s.io/gateway-api/apis/v1"
)

// fake.Client does not allocate server UIDs. This wrapper does, and can simulate
// a committed write whose reply was lost without hiding the persisted object.
type writes struct {
	client.Client
	lostCreate, lostParent bool
}

func (w *writes) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("server-" + obj.GetName()))
	}
	err := w.Client.Create(ctx, obj, opts...)
	if _, ok := obj.(*gw.Gateway); ok && err == nil && w.lostCreate {
		w.lostCreate = false
		return errors.New("lost Gateway create reply")
	}
	return err
}
func (w *writes) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	err := w.Client.Update(ctx, obj, opts...)
	if route, ok := obj.(*gw.UDPRoute); ok && err == nil && len(route.Spec.ParentRefs) > 0 && w.lostParent {
		w.lostParent = false
		return errors.New("lost parent update reply")
	}
	return err
}

type fixture struct {
	r *Reconciler
	c *writes
	p *api.GatewayPool
	t *testing.T
}

func serviceAndRoute(name string) (*core.Service, *gw.UDPRoute) {
	port := gw.PortNumber(51820)
	svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "managed", UID: types.UID("service-" + name)}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, Selector: map[string]string{"app": name}, Ports: []core.ServicePort{
		{Name: "wg", Protocol: core.ProtocolUDP, Port: 51820, TargetPort: intstr.FromInt(51820)},
		{Name: "health", Protocol: core.ProtocolTCP, Port: 9901, TargetPort: intstr.FromInt(9901)},
	}}}
	route := &gw.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "managed", UID: types.UID("route-" + name), Annotations: map[string]string{PoolAnnotation: "pool", HealthAnnotation: "9901"}},
		Spec: gw.UDPRouteSpec{Rules: []gw.UDPRouteRule{{BackendRefs: []gw.BackendRef{{BackendObjectReference: gw.BackendObjectReference{Name: gw.ObjectName(name), Port: &port}}}}}}}
	return svc, route
}
func newFixture(t *testing.T, count int) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = api.AddGatewayPoolToScheme(scheme)
	_ = core.AddToScheme(scheme)
	_ = gw.Install(scheme)
	p := &api.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "managed", UID: "pool-uid", Generation: 1}, Spec: api.GatewayPoolSpec{GatewayClassName: "native-udp", Occupancy: 2, MaxGateways: 2, PortStart: 20000}}
	objects := []client.Object{p}
	for i := 0; i < count; i++ {
		svc, route := serviceAndRoute(fmt.Sprintf("t%d", i))
		objects = append(objects, svc, route)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.GatewayPool{}, &gw.Gateway{}, &gw.UDPRoute{}).WithObjects(objects...).Build()
	w := &writes{Client: base}
	return &fixture{r: &Reconciler{Client: w, Reader: base, Namespace: "managed", Installation: "test-install", ClassName: "native-udp"}, c: w, p: p, t: t}
}
func (f *fixture) step() error {
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.p)})
	return err
}
func (f *fixture) steps(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if err := f.step(); err != nil {
			f.t.Fatal(err)
		}
	}
}
func (f *fixture) pool() *api.GatewayPool {
	f.t.Helper()
	p := &api.GatewayPool{}
	if err := f.c.Get(context.Background(), client.ObjectKeyFromObject(f.p), p); err != nil {
		f.t.Fatal(err)
	}
	return p
}
func (f *fixture) route(name string) *gw.UDPRoute {
	f.t.Helper()
	r := &gw.UDPRoute{}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "managed", Name: name}, r); err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) gateways() []gw.Gateway {
	f.t.Helper()
	list := &gw.GatewayList{}
	if err := f.c.List(context.Background(), list); err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

func TestLeaseIsDurableBeforeGatewayOrParentMutation(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(3)
	if len(f.pool().Status.Allocations) != 1 || len(f.gateways()) != 0 || len(f.route("t0").Spec.ParentRefs) != 0 {
		t.Fatal("allocation must precede all generated-resource changes")
	}
}

func TestRolloverTwoPlusOneAndRestartStableParents(t *testing.T) {
	f := newFixture(t, 3)
	f.steps(30)
	p := f.pool()
	gateways := f.gateways()
	if len(gateways) != 2 || len(p.Status.Allocations) != 3 {
		t.Fatalf("want two shards and three allocations: %#v", p.Status)
	}
	counts := map[int]int{}
	for uid, a := range p.Status.Allocations {
		counts[a.Shard]++
		route := f.route(a.RouteName)
		if string(route.UID) != uid || !a.Attached || !matchesParent(route, a) || !controllerutil.ContainsFinalizer(route, RouteFinalizer) {
			t.Fatal("route not durably attached", a)
		}
	}
	if counts[0] != 2 || counts[1] != 1 {
		t.Fatal(counts)
	}
	before := p.Status.Allocations
	parents := f.route("t0").Spec.ParentRefs
	f.r = &Reconciler{Client: f.c, Reader: f.c, Namespace: "managed", Installation: "test-install", ClassName: "native-udp"}
	f.steps(5)
	if !reflect.DeepEqual(before, f.pool().Status.Allocations) || !reflect.DeepEqual(parents, f.route("t0").Spec.ParentRefs) {
		t.Fatal("restart changed stable leases")
	}
}

func TestLostWriteRepliesDoNotDuplicateLeasesOrGateways(t *testing.T) {
	for _, kind := range []string{"create", "parent"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, 1)
			f.c.lostCreate = kind == "create"
			f.c.lostParent = kind == "parent"
			failures := 0
			for i := 0; i < 24; i++ {
				if err := f.step(); err != nil {
					failures++
				}
			}
			if failures != 1 || len(f.pool().Status.Allocations) != 1 || len(f.gateways()) != 1 || !f.pool().Status.Allocations["route-t0"].Attached {
				t.Fatal("ambiguous reply was not recovered idempotently")
			}
		})
	}
}

func TestDeletedRouteWaitsForAdapterThenNewUIDNeverReusesPort(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	route := f.route("t0")
	old := f.pool().Status.Allocations[string(route.UID)]
	if err := f.c.Delete(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(3)
	if f.pool().Status.Allocations[string(route.UID)].Tombstone {
		t.Fatal("retired before adapter cleanup completed")
	}
	route = f.route("t0")
	controllerutil.RemoveFinalizer(route, RouteFinalizer)
	if err := f.c.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(3)
	if !f.pool().Status.Allocations["route-t0"].Tombstone {
		t.Fatal("missing tombstone")
	}
	_, replacement := serviceAndRoute("t0")
	replacement.UID = "replacement-route"
	if err := f.c.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	f.steps(20)
	a := f.pool().Status.Allocations[string(replacement.UID)]
	if a.Port == old.Port && a.GatewayName == old.GatewayName {
		t.Fatal("retired public endpoint reused")
	}
	if len(f.gateways()[0].Spec.Listeners) != 1 || f.gateways()[0].Spec.Listeners[0].Port != a.Port {
		t.Fatal("inert retired placeholder must disappear when an active lease exists")
	}
}

func TestRetiredListenerRemovedWithoutBlockingLiveSibling(t *testing.T) {
	f := newFixture(t, 2)
	f.steps(25)
	route := f.route("t0")
	old := f.pool().Status.Allocations["route-t0"]
	if err := f.c.Delete(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(3)
	if len(f.gateways()[0].Spec.Listeners) != 2 {
		t.Fatal("listener removed before adapter finalized deleted route")
	}
	route = f.route("t0")
	controllerutil.RemoveFinalizer(route, RouteFinalizer)
	if err := f.c.Update(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(8)
	g := f.gateways()[0]
	live := f.pool().Status.Allocations["route-t1"]
	if len(g.Spec.Listeners) != 1 || string(g.Spec.Listeners[0].Name) != live.ListenerName {
		t.Fatal("retired listener would leave live Gateway permanently unprogrammed")
	}
	if !f.pool().Status.Allocations["route-t0"].Tombstone {
		t.Fatal("removing spec listener lost durable lease")
	}
	// A forged/retargeted retired listener is not eligible for automatic removal.
	p := f.pool()
	want := desiredGateway(p, g.Name, "test-install")
	retired := retiredGatewayListeners(p, g.Name, "test-install")
	if len(retired) != 1 || retired[0].Port != old.Port {
		t.Fatal("retired whitelist differs from ledger")
	}
	g.Spec.Listeners = append(g.Spec.Listeners, retired[0])
	g.Spec.Listeners[1].Port++
	if compatibleGateway(&g, want, retired...) {
		t.Fatal("retired whitelist accepted changed endpoint")
	}
}

func TestInvalidReferencesConsumeNoSlotsAndDoNotBlockValidSibling(t *testing.T) {
	f := newFixture(t, 2)
	route := f.route("t0")
	other := gw.Namespace("foreign")
	route.Spec.Rules[0].BackendRefs[0].Namespace = &other
	if err := f.c.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	f.steps(24)
	p := f.pool()
	if len(p.Status.Allocations) != 1 || p.Status.RejectedRoutes["route-t0"] == "" || !p.Status.Allocations["route-t1"].Attached {
		t.Fatal("invalid route either consumed a slot or blocked sibling", p.Status)
	}
}

func TestConflictingParentAndChangedServiceUIDRefused(t *testing.T) {
	for _, change := range []string{"parent", "remove-parent", "service-uid", "class"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, 1)
			f.steps(20)
			original := f.pool().Status.Allocations["route-t0"]
			switch change {
			case "parent":
				route := f.route("t0")
				route.Spec.ParentRefs[0].Name = "foreign-gateway"
				if err := f.c.Update(context.Background(), route); err != nil {
					t.Fatal(err)
				}
			case "remove-parent":
				route := f.route("t0")
				route.Spec.ParentRefs = nil
				if err := f.c.Update(context.Background(), route); err != nil {
					t.Fatal(err)
				}
			case "service-uid":
				svc := &core.Service{}
				_ = f.c.Get(context.Background(), types.NamespacedName{Namespace: "managed", Name: "t0"}, svc)
				_ = f.c.Delete(context.Background(), svc)
				svc.ResourceVersion = ""
				svc.UID = "replacement-service"
				if err := f.c.Create(context.Background(), svc); err != nil {
					t.Fatal(err)
				}
			case "class":
				p := f.pool()
				p.Spec.GatewayClassName = "foreign-class"
				if err := f.c.Update(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			}
			if change == "class" {
				if err := f.step(); err == nil {
					t.Fatal("class drift accepted")
				}
			} else {
				f.steps(3)
				if f.pool().Status.RejectedRoutes["route-t0"] == "" {
					t.Fatal("retarget not rejected")
				}
			}
			if !reflect.DeepEqual(original, f.pool().Status.Allocations["route-t0"]) {
				t.Fatal("durable allocation changed")
			}
		})
	}
}

func TestForeignGatewayAndReplacementUIDAreNeverAdopted(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			f := newFixture(t, 1)
			f.steps(3)
			p := f.pool()
			a := p.Status.Allocations["route-t0"]
			if replacement {
				f.steps(15)
				g := f.gateways()[0]
				_ = f.c.Delete(context.Background(), &g)
				g.ResourceVersion = ""
				g.UID = "new-gateway-uid"
				if err := f.c.Create(context.Background(), &g); err != nil {
					t.Fatal(err)
				}
			} else {
				g := desiredGateway(p, a.GatewayName, "foreign-installation")
				if err := f.c.Create(context.Background(), g); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.step(); err == nil {
				t.Fatal("foreign Gateway was accepted")
			}
		})
	}
}

func TestResourceVersionConflictPreventsSecondReservationCommit(t *testing.T) {
	f := newFixture(t, 2)
	f.steps(2)
	a, b := f.pool(), f.pool()
	s0, r0 := serviceAndRoute("t0")
	s1, r1 := serviceAndRoute("t1")
	if _, err := reserve(a, r0, s0, 51820, 9901); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(b, r1, s1, 51820, 9901); err != nil {
		t.Fatal(err)
	}
	if err := f.r.save(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.r.save(context.Background(), b, nil); !apierrors.IsConflict(err) {
		t.Fatalf("stale allocation commit should conflict: %v", err)
	}
	if len(f.gateways()) != 0 || len(f.pool().Status.Allocations) != 1 {
		t.Fatal("failed commit mutated generated resources")
	}
	f.steps(25)
	p := f.pool()
	if p.Status.Allocations["route-t0"].Port == p.Status.Allocations["route-t1"].Port {
		t.Fatal("retry duplicated occupied port")
	}
}

func TestCleanupArchivesLedgerAndWaitsForGatewayFinalizer(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	g := f.gateways()[0]
	g.Finalizers = []string{"nlb.independent.dev/gateway-cleanup"}
	_ = f.c.Update(context.Background(), &g)
	p := f.pool()
	if err := f.c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.steps(4)
	if !controllerutil.ContainsFinalizer(f.pool(), Finalizer) {
		t.Fatal("pool finalized before Gateway native cleanup")
	}
	archives := &core.ConfigMapList{}
	_ = f.c.List(context.Background(), archives)
	if len(archives.Items) != 1 || archives.Items[0].Immutable == nil || !*archives.Items[0].Immutable || len(archives.Items[0].OwnerReferences) > 0 {
		t.Fatal("durable archive missing")
	}
	_ = f.c.Get(context.Background(), client.ObjectKeyFromObject(&g), &g)
	g.Finalizers = nil
	_ = f.c.Update(context.Background(), &g)
	f.steps(3)
	err := f.c.Get(context.Background(), client.ObjectKeyFromObject(p), &api.GatewayPool{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("pool deletion did not finish", err)
	}
	if err = f.c.Get(context.Background(), client.ObjectKeyFromObject(&archives.Items[0]), &core.ConfigMap{}); err != nil {
		t.Fatal("archive was deleted")
	}
}

type failedReads struct{ client.Reader }

func (r failedReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*core.Service); ok {
		return errors.New("temporary transport failure")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func TestServiceReadErrorDoesNotAllocateOrGenerateGateway(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(2)
	f.r.Reader = failedReads{f.c}
	if err := f.step(); err == nil {
		t.Fatal("transport error swallowed")
	}
	if len(f.pool().Status.Allocations) != 0 || len(f.gateways()) != 0 {
		t.Fatal("read failure consumed capacity")
	}
}

func TestInvalidHealthAndPreassignedParentConsumeNoCapacity(t *testing.T) {
	for _, kind := range []string{"missing-health", "wrong-health", "preassigned-parent", "missing-Service"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, 1)
			route := f.route("t0")
			switch kind {
			case "missing-health":
				delete(route.Annotations, HealthAnnotation)
			case "wrong-health":
				route.Annotations[HealthAnnotation] = "9902"
			case "preassigned-parent":
				route.Spec.ParentRefs = []gw.ParentReference{{Name: "existing-gateway"}}
			case "missing-Service":
				route.Spec.Rules[0].BackendRefs[0].Name = "absent"
			}
			if err := f.c.Update(context.Background(), route); err != nil {
				t.Fatal(err)
			}
			f.steps(6)
			if len(f.pool().Status.Allocations) != 0 || len(f.gateways()) != 0 || f.pool().Status.RejectedRoutes["route-t0"] == "" {
				t.Fatal("invalid route consumed capacity or was not reported")
			}
		})
	}
}

func TestGatewayListenerDriftRejectedAndMissingListenerRestored(t *testing.T) {
	for _, kind := range []string{"foreign-listener", "wrong-port", "missing-listener"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, 2)
			f.steps(25)
			g := f.gateways()[0]
			switch kind {
			case "foreign-listener":
				g.Spec.Listeners[0].Name = "unowned"
			case "wrong-port":
				g.Spec.Listeners[0].Port = 29999
			case "missing-listener":
				g.Spec.Listeners = g.Spec.Listeners[1:]
			}
			if err := f.c.Update(context.Background(), &g); err != nil {
				t.Fatal(err)
			}
			if kind == "missing-listener" {
				f.steps(3)
				if len(f.gateways()[0].Spec.Listeners) != 2 {
					t.Fatal("missing reserved listener not repaired")
				}
			} else if err := f.step(); err == nil {
				t.Fatal("foreign listener drift overwritten")
			}
		})
	}
}

func TestExistingRouteUIDCannotMoveToAnotherPool(t *testing.T) {
	f := newFixture(t, 1)
	f.steps(20)
	route := f.route("t0")
	route.Annotations[PoolAnnotation] = "other"
	route.Spec.ParentRefs = nil
	if err := f.c.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	p := &api.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "managed", UID: "other-pool"}, Spec: f.p.Spec}
	if err := f.c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.p = p
	f.steps(8)
	if len(f.pool().Status.Allocations) != 0 || f.pool().Status.RejectedRoutes["route-t0"] == "" {
		t.Fatal("route UID allocated by two pools")
	}
}

package internalgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	r     *Reconciler
	g     *gateway.Gateway
	route *gateway.UDPRoute
	pool  *api.NLBPool
	svc   *core.Service
	c     client.Client
}

func setup(t *testing.T, mutate func(*fixture)) *fixture {
	t.Helper()
	f := &fixture{}
	f.r = &Reconciler{Namespace: "managed", GatewayClassName: "native", ControllerName: "nlb.independent.dev/native-udp", Installation: "test", Compartment: "c", Subnet: "s"}
	f.g = &gateway.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "managed", UID: "gateway-uid", Generation: 2, Finalizers: []string{GatewayFinalizer}, Annotations: map[string]string{poolUIDAnnotation: "pool-uid"}}, Spec: gateway.GatewaySpec{GatewayClassName: "native", Listeners: []gateway.Listener{{Name: "udp", Port: 51820, Protocol: gateway.UDPProtocolType}}}}
	f.pool = &api.NLBPool{ObjectMeta: metav1.ObjectMeta{Name: PoolName(f.g.UID), Namespace: "managed", UID: "pool-uid", Labels: f.r.labels(f.g.UID, ""), OwnerReferences: []metav1.OwnerReference{gatewayOwner(f.g)}, Finalizers: []string{api.Finalizer, PoolArchiveFinalizer}}, Spec: api.PoolSpec{CompartmentID: "c", SubnetID: "s", Occupancy: 50, MaxShards: 1, PortStart: 1, AllocationMode: "Explicit", ProvisionEmpty: true}, Status: api.PoolStatus{Shards: []api.Shard{{ID: "owned-nlb", PublicIP: "192.0.2.1"}}}}
	f.route = &gateway.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "managed", UID: "route-uid", Generation: 3, Finalizers: []string{RouteFinalizer}, CreationTimestamp: metav1.NewTime(time.Unix(100, 0)), Annotations: map[string]string{HealthPortAnnotation: "9901"}}, Spec: gateway.UDPRouteSpec{CommonRouteSpec: gateway.CommonRouteSpec{ParentRefs: []gateway.ParentReference{{Name: "edge", SectionName: ptr(gateway.SectionName("udp"))}}}, Rules: []gateway.UDPRouteRule{{BackendRefs: []gateway.BackendRef{{BackendObjectReference: gateway.BackendObjectReference{Name: "tunnel", Port: ptr(gateway.PortNumber(51820))}}}}}}}
	f.svc = &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "managed", UID: "service-uid"}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, Selector: map[string]string{"tunnel": "one"}, Ports: []core.ServicePort{{Name: "wireguard", Protocol: core.ProtocolUDP, Port: 51820, TargetPort: intstr.FromInt32(51820)}, {Name: "health", Protocol: core.ProtocolTCP, Port: 9901, TargetPort: intstr.FromInt32(9901)}}}}
	if mutate != nil {
		mutate(f)
	}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = core.AddToScheme(scheme)
	_ = gateway.Install(scheme)
	class := &gateway.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "native", UID: "class-uid", Generation: 1}, Spec: gateway.GatewayClassSpec{ControllerName: gateway.GatewayController(f.r.ControllerName)}}
	f.c = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}, &gateway.Gateway{}, &gateway.GatewayClass{}, &gateway.UDPRoute{}).WithObjects(f.g, f.pool, f.route, f.svc, class).Build()
	f.r.Client = f.c
	f.r.Reader = f.c
	return f
}
func (f *fixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.g)}); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) binding(t *testing.T) *api.TunnelBinding {
	t.Helper()
	b := &api.TunnelBinding{}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "managed", Name: BindingName(f.g.UID, f.route.UID)}, b); err != nil {
		t.Fatal(err)
	}
	return b
}
func checkCondition(t *testing.T, conditions []metav1.Condition, kind string, status metav1.ConditionStatus, generation int64) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(conditions, kind)
	if c == nil || c.Status != status || c.ObservedGeneration != generation {
		t.Fatalf("condition %s: %#v", kind, c)
	}
	return *c
}
func readObject(t *testing.T, c client.Client, o client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayCreatesExactPodIPBindingAndReportsOnlyCurrentConfiguration(t *testing.T) {
	f := setup(t, nil)
	f.reconcile(t)
	b := f.binding(t)
	if b.Spec.RequestedPort != 51820 || b.Spec.Mode != "PodIP" || b.Spec.Service != "tunnel" || b.Spec.PortName != "wireguard" || b.Spec.HealthPort != 9901 || !f.r.ownsBinding(b) {
		t.Fatal(b)
	}
	readObject(t, f.c, f.g)
	if len(f.g.Status.Addresses) != 1 || f.g.Status.Addresses[0].Value != "192.0.2.1" {
		t.Fatal(f.g.Status)
	}
	checkCondition(t, f.g.Status.Conditions, "Programmed", metav1.ConditionFalse, 2)
	b.Generation = 4
	if err := f.c.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	b.Status = api.BindingStatus{NLBID: "owned-nlb", ExternalIP: "192.0.2.1", ExternalPort: 51820, Conditions: []metav1.Condition{condition("Configured", true, "Configured", "stale", 3)}}
	if err := f.c.Status().Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, f.g)
	checkCondition(t, f.g.Status.Conditions, "Programmed", metav1.ConditionFalse, 2)
	b.Status.Conditions[0].ObservedGeneration = 4
	if err := f.c.Status().Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, f.g)
	checkCondition(t, f.g.Status.Conditions, "Programmed", metav1.ConditionTrue, 2)
	readObject(t, f.c, f.route)
	checkCondition(t, f.route.Status.Parents[0].Conditions, "Programmed", metav1.ConditionTrue, 3)
	f.pool.Status.PendingWork = "oci-pending"
	if err := f.c.Status().Update(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, f.g)
	checkCondition(t, f.g.Status.Conditions, "Programmed", metav1.ConditionFalse, 2)
}

func TestUnboundListenerHasAddressButWaitsForRoute(t *testing.T) {
	f := setup(t, func(f *fixture) { f.route.Spec.ParentRefs = nil; f.route.Finalizers = nil })
	f.reconcile(t)
	readObject(t, f.c, f.g)
	if len(f.g.Status.Addresses) != 1 {
		t.Fatal("eager address missing")
	}
	c := checkCondition(t, f.g.Status.Listeners[0].Conditions, "Programmed", metav1.ConditionFalse, 2)
	if c.Reason != "WaitingForRoute" || f.g.Status.Listeners[0].AttachedRoutes != 0 {
		t.Fatal(f.g.Status)
	}
	var bs api.TunnelBindingList
	_ = f.c.List(context.Background(), &bs)
	if len(bs.Items) != 0 {
		t.Fatal("unattached route created binding")
	}
}

func TestRestrictedProfileRejectsUnsupportedFeatures(t *testing.T) {
	tests := map[string]func(*fixture){
		"cross namespace":  func(f *fixture) { f.route.Spec.Rules[0].BackendRefs[0].Namespace = ptr(gateway.Namespace("foreign")) },
		"missing port":     func(f *fixture) { f.route.Spec.Rules[0].BackendRefs[0].Port = nil },
		"weighted backend": func(f *fixture) { f.route.Spec.Rules[0].BackendRefs[0].Weight = ptr(int32(2)) },
		"multiple backends": func(f *fixture) {
			f.route.Spec.Rules[0].BackendRefs = append(f.route.Spec.Rules[0].BackendRefs, f.route.Spec.Rules[0].BackendRefs[0])
		},
		"missing health":       func(f *fixture) { f.route.Annotations = nil },
		"missing listener":     func(f *fixture) { f.route.Spec.ParentRefs[0].SectionName = ptr(gateway.SectionName("absent")) },
		"all listeners parent": func(f *fixture) { f.route.Spec.ParentRefs[0].SectionName = nil },
		"parent port":          func(f *fixture) { f.route.Spec.ParentRefs[0].Port = ptr(gateway.PortNumber(51820)) },
		"wrong Service type":   func(f *fixture) { f.svc.Spec.Type = core.ServiceTypeLoadBalancer },
		"unnamed UDP":          func(f *fixture) { f.svc.Spec.Ports[0].Name = "" },
		"no TCP health":        func(f *fixture) { f.svc.Spec.Ports = f.svc.Spec.Ports[:1] },
		"publish unready":      func(f *fixture) { f.svc.Spec.PublishNotReadyAddresses = true },
		"default gateways":     func(f *fixture) { f.route.Spec.UseDefaultGateways = "All" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := setup(t, mutate)
			f.reconcile(t)
			var bs api.TunnelBindingList
			_ = f.c.List(context.Background(), &bs)
			if len(bs.Items) != 0 {
				t.Fatal("invalid route created binding")
			}
			readObject(t, f.c, f.route)
			if len(f.route.Status.Parents) != 1 {
				t.Fatal("missing rejection status")
			}
			checkCondition(t, f.route.Status.Parents[0].Conditions, "Programmed", metav1.ConditionFalse, f.route.Generation)
		})
	}
}

func TestGatewayInvalidationSuspendsWithoutDestroyingLeaseAndCorrectionResumes(t *testing.T) {
	f := setup(t, nil)
	f.reconcile(t)
	readObject(t, f.c, f.route)
	f.route.Annotations[HealthPortAnnotation] = "invalid"
	f.route.Generation++
	if err := f.c.Update(context.Background(), f.route); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	b := f.binding(t)
	if !b.Spec.Suspended || !b.DeletionTimestamp.IsZero() {
		t.Fatal("rejection must suspend, preserving binding", b)
	}
	readObject(t, f.c, f.route)
	f.route.Annotations[HealthPortAnnotation] = "9901"
	f.route.Generation++
	_ = f.c.Update(context.Background(), f.route)
	f.reconcile(t)
	if f.binding(t).Spec.Suspended {
		t.Fatal("corrected route remained suspended")
	}
}

func TestConflictPrecedenceAndPermanentLease(t *testing.T) {
	f := setup(t, nil)
	newer := f.route.DeepCopy()
	newer.ResourceVersion = ""
	newer.Name = "aaa-newer"
	newer.UID = "newer"
	newer.CreationTimestamp = metav1.NewTime(time.Unix(101, 0))
	if err := f.c.Create(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, newer)
	if c := checkCondition(t, newer.Status.Parents[0].Conditions, "Accepted", metav1.ConditionFalse, newer.Generation); c.Reason != "Conflicted" {
		t.Fatal(c)
	}
	// A retained port lease defeats a different Route UID even when it now wins
	// normal precedence: stale clients may not be silently retargeted.
	f.pool.Status.Allocations = map[string]api.Allocation{"retired-binding-uid": {BindingName: "retired-owner", Port: 51820, Tombstone: true}}
	if err := f.c.Status().Update(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, f.route)
	if c := checkCondition(t, f.route.Status.Parents[0].Conditions, "Accepted", metav1.ConditionFalse, f.route.Generation); c.Reason != "PortAlreadyLeased" {
		t.Fatal(c)
	}
	if !f.binding(t).Spec.Suspended {
		t.Fatal("old forwarding not suspended")
	}
}

func TestForeignGeneratedObjectsNeverAdoptedOrChanged(t *testing.T) {
	for _, kind := range []string{"pool", "binding"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, nil)
			if kind == "pool" {
				f.pool.Labels[InstallationLabel] = "foreign"
				_ = f.c.Update(context.Background(), f.pool)
			} else {
				foreign := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: BindingName(f.g.UID, f.route.UID), Namespace: "managed", UID: "foreign"}, Spec: api.BindingSpec{Pool: "foreign", HealthPort: 9901}}
				if err := f.c.Create(context.Background(), foreign); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.g)}); err == nil {
				t.Fatal("foreign object adopted")
			}
		})
	}
}

func TestRouteDeletionWaitsForBindingAndTombstone(t *testing.T) {
	f := setup(t, nil)
	f.reconcile(t)
	b := f.binding(t)
	b.UID = "binding-uid"
	b.Finalizers = []string{api.Finalizer}
	_ = f.c.Update(context.Background(), b)
	f.pool.Status.Allocations = map[string]api.Allocation{string(b.UID): {BindingName: b.Name, Port: 51820}}
	_ = f.c.Status().Update(context.Background(), f.pool)
	if err := f.c.Delete(context.Background(), f.route); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	readObject(t, f.c, b)
	if b.DeletionTimestamp.IsZero() {
		t.Fatal("binding not deleted through normal API")
	}
	readObject(t, f.c, f.route)
	if !controllerutil.ContainsFinalizer(f.route, RouteFinalizer) {
		t.Fatal("route finalized before native cleanup")
	}
	b.Finalizers = nil
	_ = f.c.Update(context.Background(), b)
	f.reconcile(t)
	readObject(t, f.c, f.route) // Still blocked by an active orphan lease.
	a := f.pool.Status.Allocations["binding-uid"]
	a.Tombstone = true
	f.pool.Status.Allocations["binding-uid"] = a
	_ = f.c.Status().Update(context.Background(), f.pool)
	f.reconcile(t)
	if err := f.c.Get(context.Background(), client.ObjectKeyFromObject(f.route), f.route); !apierrors.IsNotFound(err) {
		t.Fatal("route deletion hung", err)
	}
}

func TestGatewayRetirementArchivesBeforePoolDeletionAndRetainsNLBIdentity(t *testing.T) {
	f := setup(t, func(f *fixture) {
		f.route.Spec.ParentRefs = nil
		f.route.Finalizers = nil
		f.pool.Status.Allocations = map[string]api.Allocation{"gone": {BindingName: "retired", Port: 53, Tombstone: true}}
	})
	if err := f.c.Delete(context.Background(), f.g); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retired-" + string(f.g.UID), Namespace: "managed"}}
	readObject(t, f.c, cm)
	var archived api.NLBPool
	if err := json.Unmarshal([]byte(cm.Data["pool.json"]), &archived); err != nil {
		t.Fatal(err)
	}
	if archived.Kind != "NLBPool" || archived.APIVersion != api.GroupVersion.String() || archived.UID != "pool-uid" || archived.Status.Shards[0].ID != "owned-nlb" || !archived.Status.Allocations["gone"].Tombstone || len(cm.OwnerReferences) != 0 || !*cm.Immutable {
		t.Fatal(cm)
	}
	readObject(t, f.c, f.pool)
	if f.pool.DeletionTimestamp.IsZero() {
		t.Fatal("empty archived pool not deleted")
	}
	readObject(t, f.c, f.g)
	if !controllerutil.ContainsFinalizer(f.g, GatewayFinalizer) {
		t.Fatal("Gateway finalized before pool cleanup")
	}
	controllerutil.RemoveFinalizer(f.pool, api.Finalizer)
	_ = f.c.Update(context.Background(), f.pool)
	f.reconcile(t)
	f.reconcile(t)
	if err := f.c.Get(context.Background(), client.ObjectKeyFromObject(f.g), f.g); !apierrors.IsNotFound(err) {
		t.Fatal("Gateway cleanup hung", err)
	}
}

type failingReader struct{ client.Reader }

func (r failingReader) Get(ctx context.Context, k client.ObjectKey, obj client.Object, options ...client.GetOption) error {
	if _, ok := obj.(*core.Service); ok {
		return errors.New("transient API timeout")
	}
	return r.Reader.Get(ctx, k, obj, options...)
}
func (r failingReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*core.ServiceList); ok {
		return errors.New("transient API timeout")
	}
	return r.Reader.List(ctx, list, options...)
}
func TestTransientReadDoesNotSuspendExistingForwarding(t *testing.T) {
	f := setup(t, nil)
	f.reconcile(t)
	f.r.Reader = failingReader{f.c}
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.g)}); err == nil {
		t.Fatal("read failure hidden")
	}
	if f.binding(t).Spec.Suspended {
		t.Fatal("transient read suspended forwarding")
	}
}

func TestForeignClassAndUnsupportedGatewayNeverProvision(t *testing.T) {
	for _, kind := range []string{"class", "duplicate-port", "TCP", "TLS", "address", "ListenerSets"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, nil)
			switch kind {
			case "class":
				class := &gateway.GatewayClass{}
				_ = f.c.Get(context.Background(), types.NamespacedName{Name: "native"}, class)
				class.Spec.ControllerName = "other.example/controller"
				_ = f.c.Update(context.Background(), class)
			case "duplicate-port":
				f.g.Spec.Listeners = append(f.g.Spec.Listeners, gateway.Listener{Name: "second", Protocol: gateway.UDPProtocolType, Port: 51820})
			case "TCP":
				f.g.Spec.Listeners[0].Protocol = gateway.TCPProtocolType
			case "TLS":
				f.g.Spec.TLS = &gateway.GatewayTLSConfig{}
			case "address":
				f.g.Spec.Addresses = []gateway.GatewaySpecAddress{{Value: "192.0.2.50"}}
			case "ListenerSets":
				f.g.Spec.AllowedListeners = &gateway.AllowedListeners{}
			}
			if err := f.c.Update(context.Background(), f.g); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			readObject(t, f.c, f.g)
			checkCondition(t, f.g.Status.Conditions, "Accepted", metav1.ConditionFalse, f.g.Generation)
			var bs api.TunnelBindingList
			_ = f.c.List(context.Background(), &bs)
			if len(bs.Items) != 0 {
				t.Fatal(fmt.Sprint("unsupported Gateway produced bindings ", kind))
			}
		})
	}
}

func TestStableReconciliationDoesNotWriteStatusOrTriggerWatchLoop(t *testing.T) {
	f := setup(t, nil)
	f.reconcile(t)
	f.reconcile(t)
	objects := []client.Object{f.g, f.route, f.pool, f.binding(t), &gateway.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "native"}}}
	versions := map[string]string{}
	for _, o := range objects {
		readObject(t, f.c, o)
		versions[fmt.Sprintf("%T/%s", o, o.GetName())] = o.GetResourceVersion()
	}
	for i := 0; i < 3; i++ {
		f.reconcile(t)
	}
	for _, o := range objects {
		readObject(t, f.c, o)
		if o.GetResourceVersion() != versions[fmt.Sprintf("%T/%s", o, o.GetName())] {
			t.Fatalf("stable reconcile wrote %T/%s", o, o.GetName())
		}
	}
}

type immutableBindingClient struct {
	client.Client
	forbidden int
}

func (c *immutableBindingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, options ...client.PatchOption) error {
	if binding, ok := obj.(*api.TunnelBinding); ok {
		before := &api.TunnelBinding{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(binding), before); err != nil {
			return err
		}
		if !sameBindingTarget(before.Spec, binding.Spec) {
			c.forbidden++
			return fmt.Errorf("CEL: immutable target fields")
		}
	}
	return c.Client.Patch(ctx, obj, patch, options...)
}
func TestImmutableRouteRetargetSuspendsWithoutInvalidCRDPatch(t *testing.T) {
	for _, kind := range []string{"service", "listener port", "Service port name"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, nil)
			f.reconcile(t)
			before := f.binding(t).Spec
			guarded := &immutableBindingClient{Client: f.c}
			f.r.Client = guarded
			readObject(t, f.c, f.route)
			switch kind {
			case "service":
				other := f.svc.DeepCopy()
				other.ResourceVersion = ""
				other.UID = "other-service"
				other.Name = "other"
				if err := f.c.Create(context.Background(), other); err != nil {
					t.Fatal(err)
				}
				f.route.Spec.Rules[0].BackendRefs[0].Name = "other"
			case "listener port":
				readObject(t, f.c, f.g)
				f.g.Spec.Listeners[0].Port++
				_ = f.c.Update(context.Background(), f.g)
			case "Service port name":
				readObject(t, f.c, f.svc)
				f.svc.Spec.Ports[0].Name = "renamed"
				_ = f.c.Update(context.Background(), f.svc)
			}
			f.route.Generation++
			_ = f.c.Update(context.Background(), f.route)
			f.reconcile(t)
			after := f.binding(t).Spec
			if guarded.forbidden != 0 || !after.Suspended || !sameBindingTarget(before, after) {
				t.Fatal("immutable retarget was not safely suspended", guarded.forbidden, after)
			}
			readObject(t, f.c, f.route)
			checkCondition(t, f.route.Status.Parents[0].Conditions, "Accepted", metav1.ConditionFalse, f.route.Generation)
			checkCondition(t, f.route.Status.Parents[0].Conditions, "Programmed", metav1.ConditionFalse, f.route.Generation)
		})
	}
}

func TestRetirementRefusesForeignArchiveAndActiveLease(t *testing.T) {
	for _, scenario := range []string{"active", "foreign archive"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t, func(f *fixture) { f.route.Spec.ParentRefs = nil; f.route.Finalizers = nil })
			if scenario == "active" {
				f.pool.Status.Allocations = map[string]api.Allocation{"active": {BindingName: "active", Port: 51820}}
				_ = f.c.Status().Update(context.Background(), f.pool)
			} else {
				_ = f.c.Create(context.Background(), &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retired-" + string(f.g.UID), Namespace: "managed"}, Data: map[string]string{"foreign": "keep"}})
			}
			_ = f.c.Delete(context.Background(), f.g)
			_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.g)})
			if scenario == "foreign archive" && err == nil {
				t.Fatal("archive collision ignored")
			}
			readObject(t, f.c, f.pool)
			if !f.pool.DeletionTimestamp.IsZero() {
				t.Fatal("unsafe pool deletion")
			}
			readObject(t, f.c, f.g)
			if !controllerutil.ContainsFinalizer(f.g, GatewayFinalizer) {
				t.Fatal("unsafe Gateway finalization")
			}
		})
	}
}

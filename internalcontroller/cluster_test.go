package internalcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"net/http"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func clusterFixture() (*api.TunnelBinding, *core.Service, *discovery.EndpointSlice, *core.Pod, *core.Node, *core.Node) {
	b, s, e, p, one := podIPFixture()
	b.Spec.Mode, b.Spec.HealthPort = "NodePortCluster", 10256
	s.Spec.Type, s.Spec.ExternalTrafficPolicy = core.ServiceTypeNodePort, core.ServiceExternalTrafficPolicyCluster
	s.Spec.Ports = s.Spec.Ports[:1]
	s.Spec.Ports[0].NodePort = 30001
	e.Ports = e.Ports[:1]
	one.UID = "worker-one"
	one.Spec.ProviderID = "oci://ocid1.instance.one"
	one.Status.Addresses = []core.NodeAddress{{Type: core.NodeInternalIP, Address: "10.0.1.2"}}
	two := one.DeepCopy()
	two.Name, two.UID = "second", "worker-two"
	two.Spec.ProviderID = "oci://ocid1.instance.two"
	two.Status.Addresses[0].Address = "10.0.1.3"
	return b, s, e, p, one, two
}

func TestClusterWorkerIdentityInventory(t *testing.T) {
	for _, scenario := range []string{"ready", "cordon", "notready", "new-unready", "replaced-uid", "replaced-instance", "replaced-ip", "deleted", "duplicate-ip", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, _, _, one, two := clusterFixture()
			pool := &api.NLBPool{Spec: api.PoolSpec{MaxWorkers: 2}, Status: api.PoolStatus{Workers: map[string]api.WorkerIdentity{"node": {UID: string(one.UID), InstanceID: "ocid1.instance.one", IP: "10.0.1.2"}}}}
			switch scenario {
			case "cordon":
				one.Spec.Unschedulable = true
			case "notready", "new-unready", "replaced-uid", "replaced-instance", "replaced-ip":
				one.Status.Conditions[0].Status = core.ConditionFalse
				if scenario == "new-unready" {
					pool.Status.Workers = nil
				}
				if scenario == "replaced-uid" {
					one.UID = "other"
				}
				if scenario == "replaced-instance" {
					one.Spec.ProviderID = "oci://ocid1.instance.other"
				}
				if scenario == "replaced-ip" {
					one.Status.Addresses[0].Address = "10.0.1.99"
				}
			case "deleted":
				one.DeletionTimestamp = ptr(metav1.Now())
				one.Finalizers = []string{"test"}
			case "duplicate-ip":
				two.Status.Addresses[0].Address = one.Status.Addresses[0].Address
			case "overflow":
				pool.Spec.MaxWorkers = 1
			}
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithObjects(one, two).Build()
			workers, err := clusterWorkers(context.Background(), c, pool)
			if scenario == "duplicate-ip" || scenario == "overflow" {
				if err == nil {
					t.Fatal("unsafe inventory accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if scenario == "new-unready" || scenario == "replaced-uid" || scenario == "replaced-instance" || scenario == "replaced-ip" || scenario == "deleted" {
				want = 1
			}
			if len(workers) != want {
				t.Fatalf("workers=%v", workers)
			}
		})
	}
}

func TestClusterReconciliationStableAndFailClosed(t *testing.T) {
	for _, scenario := range []string{"ready", "gap", "pod-notready", "pod-moved", "node-notready", "cordon", "foreign-slice", "stale-pod", "overlap", "unknown-ready", "serving-terminating", "stale-serving-terminating", "ready-replacement-stale-terminating", "timeout", "throttle", "cancel", "overflow", "overflow-foreign-slice", "deleted-worker"} {
		t.Run(scenario, func(t *testing.T) {
			b, s, e, p, one, two := clusterFixture()
			var lb n.NetworkLoadBalancer
			var updates []n.UpdateBackendSetDetails
			cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(lb)
					return
				}
				if r.Method != http.MethodPut {
					t.Fatalf("unexpected %s", r.Method)
				}
				var update n.UpdateBackendSetDetails
				if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
					t.Fatal(err)
				}
				updates = append(updates, update)
				w.Header().Set("opc-work-request-id", "update")
				w.WriteHeader(http.StatusAccepted)
			})
			pool.Name, pool.Namespace = "pool", "managed"
			pool.Finalizers = []string{api.Finalizer}
			pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart, pool.Spec.MaxWorkers = 2, 1, 20000, 2
			frozen := pool.Spec
			pool.Status.FrozenSpec = &frozen
			a := pool.Status.Allocations["binding"]
			a.NodePort = 30001
			a.BindingName, a.ServiceName, a.ServiceUID, a.PortName, a.Mode = b.Name, s.Name, string(s.UID), "wg", "NodePortCluster"
			pool.Status.Allocations["binding"] = a
			pool.Status.Workers = map[string]api.WorkerIdentity{"node": {UID: string(one.UID), InstanceID: "ocid1.instance.one", IP: "10.0.1.2"}, "second": {UID: string(two.UID), InstanceID: "ocid1.instance.two", IP: "10.0.1.3"}}
			backends := []Backend{{IP: "10.0.1.2", TargetID: "ocid1.instance.one", Port: 30001}, {IP: "10.0.1.3", TargetID: "ocid1.instance.two", Port: 30001}}
			lb = cloudModel(cloud, pool, 0)
			bs := convergedSet(&backends[0])
			bs.Backends = append(bs.Backends, convergedSet(&backends[1]).Backends...)
			bs.HealthChecker = &n.HealthChecker{Protocol: n.HealthCheckProtocolsHttp, Port: ptr(10256), UrlPath: ptr("/healthz"), ReturnCode: ptr(200), IntervalInMillis: ptr(10000), TimeoutInMillis: ptr(3000), Retries: ptr(3)}
			lb.BackendSets[a.Name], lb.Listeners[a.Name] = bs, convergedListener(a)
			objects := []client.Object{pool, b, s, e, p, one, two}
			switch scenario {
			case "gap":
				e.Endpoints = nil
			case "pod-notready":
				p.Status.Conditions[0].Status = core.ConditionFalse
			case "pod-moved":
				p.Spec.NodeName = two.Name
				p.UID = "replacement"
				p.Status.PodIP = "10.0.8.3"
				e.Endpoints[0].TargetRef.UID = p.UID
				e.Endpoints[0].NodeName = &two.Name
				e.Endpoints[0].Addresses = []string{p.Status.PodIP}
			case "node-notready":
				one.Status.Conditions[0].Status = core.ConditionFalse
			case "cordon":
				one.Spec.Unschedulable = true
			case "foreign-slice":
				e.OwnerReferences = nil
			case "stale-pod":
				p.UID = "wrong"
			case "unknown-ready":
				e.Endpoints[0].Conditions.Ready = nil
			case "serving-terminating", "stale-serving-terminating":
				e.Endpoints[0].Conditions.Terminating = ptr(true)
				e.Endpoints[0].Conditions.Serving = ptr(true)
				if scenario == "stale-serving-terminating" {
					p.UID = "wrong"
				}
			case "ready-replacement-stale-terminating":
				old := *e.Endpoints[0].DeepCopy()
				old.TargetRef.Name, old.TargetRef.UID = "missing-old", "missing-old"
				old.Addresses = []string{"10.0.8.99"}
				old.Conditions.Terminating = ptr(true)
				old.Conditions.Serving = ptr(true)
				e.Endpoints = append(e.Endpoints, old)
			case "overlap":
				other := p.DeepCopy()
				other.Name, other.UID = "other", "other"
				other.Status.PodIP = "10.0.8.3"
				objects = append(objects, other)
				ep := *e.Endpoints[0].DeepCopy()
				ep.TargetRef.Name, ep.TargetRef.UID = other.Name, other.UID
				ep.Addresses = []string{other.Status.PodIP}
				e.Endpoints = append(e.Endpoints, ep)
			case "overflow", "overflow-foreign-slice":
				third := two.DeepCopy()
				third.Name, third.UID = "third", "third"
				third.Spec.ProviderID = "oci://ocid1.instance.three"
				third.Status.Addresses[0].Address = "10.0.1.4"
				objects = append(objects, third)
				if scenario == "overflow-foreign-slice" {
					e.OwnerReferences = nil
				}
			case "deleted-worker":
				two.DeletionTimestamp = ptr(metav1.Now())
				two.Finalizers = []string{"test"}
			}
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(objects...).Build()
			var reader client.Reader = c
			if scenario == "timeout" {
				reader = endpointErrorReader{Reader: c, getKind: "Pod", err: context.DeadlineExceeded}
			}
			if scenario == "throttle" {
				reader = endpointErrorReader{Reader: c, listError: true, err: apierrors.NewTooManyRequests("test", 1)}
			}
			r := &Reconciler{Client: c, Reader: reader, Cloud: cloud, Recorder: record.NewFakeRecorder(30), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			ctx := context.Background()
			if scenario == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			if scenario == "timeout" || scenario == "throttle" || scenario == "cancel" {
				if err == nil || len(updates) != 0 {
					t.Fatalf("err=%v mutations=%d", err, len(updates))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "deleted-worker" { // First reconcile persists changed identities before mutating OCI.
				if len(updates) != 0 {
					t.Fatal("cloud mutation before identity journal")
				}
				_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
				if err != nil {
					t.Fatal(err)
				}
				if len(updates) != 1 || len(updates[0].Backends) != 1 || value(updates[0].Backends[0].TargetId) != "ocid1.instance.one" {
					t.Fatalf("deleted identity retained: %+v", updates)
				}
				return
			}
			unsafe := scenario == "foreign-slice" || scenario == "stale-pod" || scenario == "overlap" || scenario == "overflow-foreign-slice" || scenario == "unknown-ready" || scenario == "stale-serving-terminating"
			if unsafe {
				if len(updates) != 1 || len(updates[0].Backends) != 0 {
					t.Fatalf("unsafe route not cleared: %+v", updates)
				}
			} else if len(updates) != 0 {
				t.Fatalf("stable workers mutated: %+v", updates)
			}
		})
	}
}

func TestClusterCloudBudgetAndOrder(t *testing.T) {
	desired := []Backend{{IP: "10.0.1.1", TargetID: "ocid1.instance.one", Port: 30001}, {IP: "10.0.1.2", TargetID: "ocid1.instance.two", Port: 30001}}
	current := append(convergedSet(&desired[1]).Backends, convergedSet(&desired[0]).Backends...)
	if !sameBackendList(current, desired) {
		t.Fatal("order-only change would write")
	}
	details := backendListDetails(desired, "binding", current)
	if reflect.DeepEqual(details[0].Name, details[1].Name) {
		t.Fatal("worker names collide")
	}
	lb := n.NetworkLoadBalancer{BackendSets: map[string]n.BackendSet{"other": {Backends: make([]n.Backend, 1023)}}}
	var capacity *BackendCapacityError
	if !errors.As(validateBackendBudget(lb, "own", desired), &capacity) {
		t.Fatal("1024 total limit not enforced")
	}
	h := modeHealth("NodePortCluster", 9901)
	if h.Protocol != n.HealthCheckProtocolsHttp || value(h.Port) != 10256 || value(h.UrlPath) != "/healthz" || value(h.ReturnCode) != 200 || value(h.IntervalInMillis) != 10000 || value(h.TimeoutInMillis) != 3000 {
		t.Fatalf("wrong worker proxy health %+v", h)
	}
}

func TestClusterRejectsCapacityBeforeLeaseAndCloud(t *testing.T) {
	b, svc, _, _, one, two := clusterFixture()
	calls := 0
	cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected cloud call", 500) })
	pool.Name, pool.Namespace = "pool", "managed"
	pool.Finalizers = []string{api.Finalizer}
	pool.Spec.Occupancy, pool.Spec.MaxShards, pool.Spec.PortStart, pool.Spec.MaxWorkers = 2, 1, 20000, 1
	frozen := pool.Spec
	pool.Status = api.PoolStatus{FrozenSpec: &frozen}
	c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(pool, b, svc, one, two).Build()
	r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(10), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(pool.Status.Allocations) != 0 {
		t.Fatalf("rejected allocation leaked: calls=%d allocations=%v", calls, pool.Status.Allocations)
	}
	if err := validateBindingService(b, svc, true); err == nil {
		t.Fatal("source preservation accepted")
	}
	pool.Spec.Occupancy, pool.Spec.MaxWorkers = 50, 20
	if err := ValidatePool(pool.Spec); err != nil {
		t.Fatal(err)
	}
	pool.Spec.MaxWorkers = 21
	if ValidatePool(pool.Spec) == nil {
		t.Fatal("1050 worst-case backend budget accepted")
	}
	pool.Spec.Occupancy, pool.Spec.MaxWorkers = 20, 50
	if err := ValidatePool(pool.Spec); err != nil {
		t.Fatal("larger worker fleet with lower occupancy rejected", err)
	}
}

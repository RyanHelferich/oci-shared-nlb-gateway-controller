package internalcontroller

import (
	"context"
	"encoding/json"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	"k8s.io/client-go/tools/record"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestClusterNodePortPinAndUpgrade(t *testing.T) {
	for _, scenario := range []string{"new-allocation", "unchanged", "drift-ready", "drift-gap", "upgrade-match", "upgrade-empty", "upgrade-different", "upgrade-ambiguous", "upgrade-service-uid", "upgrade-read-failure"} {
		t.Run(scenario, func(t *testing.T) {
			b, svc, slice, pod, one, two := clusterFixture()
			var lb n.NetworkLoadBalancer
			calls := 0
			var updates []n.UpdateBackendSetDetails
			cloud, pool := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if scenario == "upgrade-read-failure" {
						w.WriteHeader(403)
						json.NewEncoder(w).Encode(map[string]string{"code": "NotAuthorized", "message": "injected uncertain read"})
						return
					}
					json.NewEncoder(w).Encode(lb)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected mutation %s", r.Method)
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
			a.BindingName, a.ServiceName, a.ServiceUID, a.PortName, a.Mode, a.NodePort = b.Name, svc.Name, string(svc.UID), "wg", "NodePortCluster", 30001
			if len(scenario) >= 8 && scenario[:8] == "upgrade-" {
				a.NodePort = 0
			}
			pool.Status.Allocations["binding"] = a
			pool.Status.Workers = map[string]api.WorkerIdentity{"node": {UID: string(one.UID), InstanceID: "ocid1.instance.one", IP: "10.0.1.2"}, "second": {UID: string(two.UID), InstanceID: "ocid1.instance.two", IP: "10.0.1.3"}}
			lb = cloudModel(cloud, pool, 0)
			bs := convergedSet(&Backend{IP: "10.0.1.2", TargetID: "ocid1.instance.one", Port: 30001})
			bs.Backends = append(bs.Backends, convergedSet(&Backend{IP: "10.0.1.3", TargetID: "ocid1.instance.two", Port: 30001}).Backends...)
			bs.HealthChecker = &n.HealthChecker{Protocol: n.HealthCheckProtocolsHttp, Port: ptr(10256), UrlPath: ptr("/healthz"), ReturnCode: ptr(200), IntervalInMillis: ptr(10000), TimeoutInMillis: ptr(3000), Retries: ptr(3)}
			switch scenario {
			case "new-allocation":
				pool.Status.Allocations = nil
				pool.Status.Shards = nil
			case "drift-ready", "drift-gap":
				svc.Spec.Ports[0].NodePort = 30002
				if scenario == "drift-gap" {
					slice.Endpoints = nil
				}
			case "upgrade-empty":
				bs.Backends = nil
			case "upgrade-different":
				for i := range bs.Backends {
					bs.Backends[i].Port = ptr(30009)
				}
			case "upgrade-ambiguous":
				bs.Backends[1].Port = ptr(30009)
			case "upgrade-service-uid":
				svc.UID = "replacement-service"
			}
			lb.BackendSets[a.Name], lb.Listeners[a.Name] = bs, convergedListener(a)
			c := fake.NewClientBuilder().WithScheme(podIPScheme()).WithStatusSubresource(&api.NLBPool{}, &api.TunnelBinding{}).WithObjects(pool, b, svc, slice, pod, one, two).Build()
			r := &Reconciler{Client: c, Reader: c, Cloud: cloud, Recorder: record.NewFakeRecorder(20), Namespace: "managed", Compartment: "comp", Subnet: "sub"}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			read := &api.NLBPool{}
			if getErr := c.Get(context.Background(), client.ObjectKeyFromObject(pool), read); getErr != nil {
				t.Fatal(getErr)
			}
			saved := read.Status.Allocations[string(b.UID)]
			if scenario == "upgrade-read-failure" {
				if err == nil || len(updates) != 0 || saved.NodePort != 0 {
					t.Fatalf("uncertain read changed state: err=%v updates=%v pin=%d", err, updates, saved.NodePort)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "new-allocation":
				if saved.NodePort != 30001 || calls != 0 {
					t.Fatalf("port not atomically persisted before cloud: pin=%d calls=%d", saved.NodePort, calls)
				}
			case "unchanged":
				if len(updates) != 0 || saved.NodePort != 30001 {
					t.Fatal("unchanged binding mutated")
				}
			case "upgrade-match":
				if len(updates) != 0 || saved.NodePort != 30001 {
					t.Fatalf("matching legacy port did not pin without cloud write: %+v", saved)
				}
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
					t.Fatal(err)
				}
				if len(updates) != 0 {
					t.Fatal("upgraded unchanged native set was rewritten")
				}
			case "upgrade-empty":
				if len(updates) != 0 || saved.NodePort != 0 {
					t.Fatal("empty legacy set was guessed or rewritten")
				}
			default:
				if len(updates) != 1 || len(updates[0].Backends) != 0 {
					t.Fatalf("unverified/changed port was not cleared: %+v", updates)
				}
				if scenario == "drift-ready" || scenario == "drift-gap" {
					if saved.NodePort != 30001 {
						t.Fatal("immutable port overwritten")
					}
				} else if saved.NodePort != 0 {
					t.Fatal("unverified legacy port guessed")
				}
			}
		})
	}
}

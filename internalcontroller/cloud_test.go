package internalcontroller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"github.com/oracle/oci-go-sdk/v65/common"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cloudFixture(t *testing.T, h http.HandlerFunc) (*Cloud, *api.NLBPool) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	provider := common.NewRawConfigurationProvider("tenancy", "user", "us-ashburn-1", "fingerprint", string(private), nil)
	sdk, e := n.NewNetworkLoadBalancerClientWithConfigurationProvider(provider)
	if e != nil {
		t.Fatal(e)
	}
	sdk.Host = server.URL
	// Injected responses exercise SDK serialization and real HTTP calls, not cloud availability.
	p := &api.NLBPool{ObjectMeta: metav1.ObjectMeta{UID: "pool"}, Spec: api.PoolSpec{CompartmentID: "comp", SubnetID: "sub", MaxShards: 27}, Status: api.PoolStatus{Shards: []api.Shard{{ID: "lb"}}, Allocations: map[string]api.Allocation{"binding": {Name: "t-binding", Shard: 0, Port: 20000}}}}
	return &Cloud{Client: sdk, Installation: "test"}, p
}

func TestSDKTransientRetries(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{429, "TooManyRequests"}, {409, "IncorrectState"}, {500, "InternalServerError"}} {
		t.Run(tc.code, func(t *testing.T) {
			attempts := 0
			c, _ := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.Header().Set("Content-Type", "application/json")
				if attempts < 3 {
					w.WriteHeader(tc.status)
					json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "message": "injected transient error"})
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"id": "work", "status": "SUCCEEDED"})
			})
			policy := common.NewRetryPolicyWithOptions(common.WithMaximumNumberAttempts(4), common.WithFixedBackoff(time.Millisecond))
			c.Client.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &policy})
			done, err := c.Work(context.Background(), "work")
			if err != nil || !done || attempts != 3 {
				t.Fatalf("done=%v attempts=%d err=%v", done, attempts, err)
			}
		})
	}
}
func TestCloudOwnershipAndInterruptedCreation(t *testing.T) {
	posts := 0
	c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts++
			t.Errorf("unexpected mutation %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "networkLoadBalancers") {
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "lb", "freeformTags": map[string]string{"installation": "test", "pool-uid": "pool", "shard": "0"}, "lifecycleState": "ACTIVE"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "lb", "compartmentId": "comp", "subnetId": "sub", "freeformTags": map[string]string{"controller": "independent-shared-nlb", "installation": "test", "project": "test", "pool-uid": "pool", "shard": "0"}, "lifecycleState": "ACTIVE", "ipAddresses": []any{map[string]any{"isPublic": true, "ipAddress": "192.0.2.10"}}})
	})
	p.Status.Shards = nil
	s, work, e := c.EnsureShard(context.Background(), p, 0)
	if e != nil || work != "" || s.ID != "lb" || posts != 0 {
		t.Fatal(s, work, e)
	}
	p.Status.Shards = []api.Shard{s}
	c.Installation = "another-installation"
	if _, e := c.Step(context.Background(), p, p.Status.Allocations["binding"], nil, 9901, true); e == nil {
		t.Fatal("unowned deletion accepted")
	}
}
func TestWorkRequestStates(t *testing.T) {
	status := "IN_PROGRESS"
	c, _ := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "work", "status": status})
	})
	done, e := c.Work(context.Background(), "work")
	if done || e != nil {
		t.Fatal(done, e)
	}
	status = "FAILED"
	done, e = c.Work(context.Background(), "work")
	if !done || e == nil {
		t.Fatal(done, e)
	}
	status = "SUCCEEDED"
	done, e = c.Work(context.Background(), "work")
	if !done || e != nil {
		t.Fatal(done, e)
	}
}

func cloudModel(c *Cloud, p *api.NLBPool, shard int) n.NetworkLoadBalancer {
	return n.NetworkLoadBalancer{Id: &p.Status.Shards[shard].ID, CompartmentId: &p.Spec.CompartmentID, SubnetId: &p.Spec.SubnetID, FreeformTags: c.tags(p, shard), LifecycleState: n.LifecycleStateActive, Listeners: map[string]n.Listener{}, BackendSets: map[string]n.BackendSet{}}
}

func convergedSet(b *Backend) n.BackendSet {
	return n.BackendSet{Policy: n.NetworkLoadBalancingPolicyFiveTuple, HealthChecker: &n.HealthChecker{Protocol: n.HealthCheckProtocolsTcp, Port: ptr(30901), IntervalInMillis: ptr(10000), TimeoutInMillis: ptr(3000), Retries: ptr(3)}, IsPreserveSource: ptr(false), IsFailOpen: ptr(false), IsInstantFailoverEnabled: ptr(true), Backends: []n.Backend{{IpAddress: &b.IP, TargetId: &b.TargetID, Port: &b.Port, Weight: ptr(1)}}}
}

func convergedListener(a api.Allocation) n.Listener {
	return n.Listener{Port: &a.Port, DefaultBackendSetName: &a.Name, Protocol: n.ListenerProtocolsUdp, UdpIdleTimeout: ptr(120)}
}

func TestBackendDriftAndMissingHealthRepair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*n.BackendSet)
	}{
		{"nil-health", func(bs *n.BackendSet) { bs.HealthChecker = nil }},
		{"health-protocol", func(bs *n.BackendSet) { bs.HealthChecker.Protocol = n.HealthCheckProtocolsHttp }},
		{"aggressive-health-interval", func(bs *n.BackendSet) { bs.HealthChecker.IntervalInMillis = ptr(1000) }},
		{"aggressive-health-timeout", func(bs *n.BackendSet) { bs.HealthChecker.TimeoutInMillis = ptr(1000) }},
		{"wrong-instance", func(bs *n.BackendSet) { bs.Backends[0].TargetId = ptr("old-instance") }},
		{"backup", func(bs *n.BackendSet) { bs.Backends[0].IsBackup = ptr(true) }},
		{"weight", func(bs *n.BackendSet) { bs.Backends[0].Weight = ptr(0) }},
		{"policy", func(bs *n.BackendSet) { bs.Policy = n.NetworkLoadBalancingPolicyThreeTuple }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lb n.NetworkLoadBalancer
			gets, writes := 0, 0
			c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					gets++
					json.NewEncoder(w).Encode(lb)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected method %s", r.Method)
				}
				writes++
				var body n.UpdateBackendSetDetails
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.HealthChecker.Protocol != n.HealthCheckProtocolsTcp || value(body.HealthChecker.IntervalInMillis) != 10000 || value(body.HealthChecker.TimeoutInMillis) != 3000 || value(body.HealthChecker.Retries) != 3 || value(body.Backends[0].TargetId) != "instance" || value(body.Backends[0].IsBackup) || value(body.Backends[0].Weight) != 1 {
					t.Errorf("incorrect repair: %+v", body)
				}
				w.Header().Set("opc-work-request-id", "work")
				json.NewEncoder(w).Encode(map[string]any{})
			})
			a := p.Status.Allocations["binding"]
			b := &Backend{IP: "10.0.0.2", TargetID: "instance", Port: 30001}
			lb = cloudModel(c, p, 0)
			bs := convergedSet(b)
			tc.mutate(&bs)
			lb.BackendSets[a.Name] = bs
			lb.Listeners[a.Name] = convergedListener(a)
			c.ResetSnapshot()
			if work, err := c.Step(context.Background(), p, a, b, 30901, false); err != nil || work != "work" {
				t.Fatal(work, err)
			}
			// An attempted write must invalidate the snapshot even before the
			// next reconciliation, including when its response is lost.
			if _, err := c.get(context.Background(), "lb"); err != nil {
				t.Fatal(err)
			}
			if gets != 2 || writes != 1 {
				t.Fatal(gets, writes)
			}
		})
	}
}

func TestScaleSnapshotBoundedAndReset(t *testing.T) {
	lbs := map[string]n.NetworkLoadBalancer{}
	gets := 0
	c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		gets++
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lbs[id])
	})
	p.Status.Shards = nil
	p.Status.Allocations = map[string]api.Allocation{}
	b := &Backend{IP: "10.0.0.2", TargetID: "instance", Port: 30001}
	for shard := 0; shard < 27; shard++ {
		p.Status.Shards = append(p.Status.Shards, api.Shard{ID: fmt.Sprintf("lb-%d", shard)})
		lbs[p.Status.Shards[shard].ID] = cloudModel(c, p, shard)
	}
	for i := 0; i < 1200; i++ {
		a := api.Allocation{Name: fmt.Sprintf("binding-%d", i), Shard: i / 45, Port: 20000 + i%45}
		p.Status.Allocations[a.Name] = a
		lb := lbs[p.Status.Shards[a.Shard].ID]
		lb.BackendSets[a.Name] = convergedSet(b)
		lb.Listeners[a.Name] = convergedListener(a)
	}
	c.ResetSnapshot()
	for _, a := range p.Status.Allocations {
		if _, _, err := c.EnsureShard(context.Background(), p, a.Shard); err != nil {
			t.Fatal(err)
		}
		if work, err := c.Step(context.Background(), p, a, b, 30901, false); err != nil || work != "" {
			t.Fatal(work, err)
		}
	}
	if gets != 27 {
		t.Fatalf("1200 bindings caused %d reads; want 27", gets)
	}
	c.ResetSnapshot()
	if _, _, err := c.EnsureShard(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if gets != 28 {
		t.Fatal("stale snapshot survived reset", gets)
	}
}

func TestBackendNamesUniqueAcrossBackendSets(t *testing.T) {
	var lb n.NetworkLoadBalancer
	names := map[string]bool{}
	c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(lb)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/backendSets") {
			t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
		}
		var body n.CreateBackendSetDetails
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Backends) != 1 {
			t.Error("expected one backend")
			return
		}
		if body.HealthChecker == nil || value(body.HealthChecker.IntervalInMillis) != 10000 || value(body.HealthChecker.TimeoutInMillis) != 3000 || value(body.HealthChecker.Retries) != 3 {
			t.Error("new backend set must use native CCM health timing, not aggressive per-binding probes")
		}
		name := value(body.Backends[0].Name)
		if !strings.HasPrefix(name, value(body.Name)+"-") || names[name] {
			t.Errorf("backend name collision: %s", name)
		}
		names[name] = true
		w.Header().Set("opc-work-request-id", "work")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	lb = cloudModel(c, p, 0)
	p.Status.Allocations["second"] = api.Allocation{Name: "t-second", Shard: 0, Port: 20001}
	for _, a := range p.Status.Allocations {
		if work, err := c.Step(context.Background(), p, a, &Backend{IP: "10.0.0.2", TargetID: "instance", Port: 30001}, 30901, false); err != nil || work != "work" {
			t.Fatal(work, err)
		}
	}
	if len(names) != 2 {
		t.Fatal(names)
	}
}

package internalcontroller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
)

func TestDestinationChangeReplacesBackendIdentity(t *testing.T) {
	original := Backend{IP: "10.0.0.2", TargetID: "ocid1.instance.original", Port: 30001}
	seen := map[string]bool{}
	for _, tc := range []struct {
		name string
		next Backend
	}{
		{"reused worker IP with new instance", Backend{IP: original.IP, TargetID: "ocid1.instance.replacement", Port: original.Port}},
		{"changed worker IP", Backend{IP: "10.0.0.3", TargetID: original.TargetID, Port: original.Port}},
		{"changed NodePort", Backend{IP: original.IP, TargetID: original.TargetID, Port: 30002}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var model n.NetworkLoadBalancer
			var written n.UpdateBackendSetDetails
			puts := 0
			c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(model)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected mutation %s", r.Method)
				}
				puts++
				if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
					t.Error(err)
				}
				w.Header().Set("opc-work-request-id", "replacement-work")
				json.NewEncoder(w).Encode(map[string]any{})
			})
			a := p.Status.Allocations["binding"]
			model = cloudModel(c, p, 0)
			set := convergedSet(&original)
			oldName := value(backendDetails(&original, a.Name)[0].Name)
			set.Backends[0].Name = &oldName
			model.BackendSets[a.Name], model.Listeners[a.Name] = set, convergedListener(a)
			work, err := c.Step(context.Background(), p, a, &tc.next, 30901, false)
			if err != nil || work != "replacement-work" || puts != 1 || len(written.Backends) != 1 {
				t.Fatalf("work=%q err=%v puts=%d backends=%d", work, err, puts, len(written.Backends))
			}
			name := value(written.Backends[0].Name)
			if name == "" || name == oldName || seen[name] {
				t.Fatalf("destination change reused backend identity %q", name)
			}
			seen[name] = true
			if value(written.Backends[0].IpAddress) != tc.next.IP || value(written.Backends[0].TargetId) != tc.next.TargetID || value(written.Backends[0].Port) != tc.next.Port {
				t.Fatal("replacement lost verified destination fields")
			}
		})
	}
}

func TestExistingBackendNameSurvivesConvergenceAndHealthRepair(t *testing.T) {
	backend := &Backend{IP: "10.0.0.2", TargetID: "ocid1.instance.original", Port: 30001}
	var model n.NetworkLoadBalancer
	puts := 0
	c, p := cloudFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(model)
			return
		}
		puts++
		var written n.UpdateBackendSetDetails
		if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
			t.Error(err)
		}
		if len(written.Backends) != 1 || value(written.Backends[0].Name) != "legacy-active" {
			t.Error("health repair replaced an unchanged destination identity")
		}
		w.Header().Set("opc-work-request-id", "health-repair")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	a := p.Status.Allocations["binding"]
	model = cloudModel(c, p, 0)
	set := convergedSet(backend)
	set.Backends[0].Name = ptr("legacy-active")
	model.BackendSets[a.Name], model.Listeners[a.Name] = set, convergedListener(a)
	if work, err := c.Step(context.Background(), p, a, backend, 30901, false); err != nil || work != "" || puts != 0 {
		t.Fatalf("unchanged destination churned: work=%q err=%v puts=%d", work, err, puts)
	}
	set.HealthChecker.Port = ptr(30902)
	model.BackendSets[a.Name] = set
	if work, err := c.Step(context.Background(), p, a, backend, 30901, false); err != nil || work != "health-repair" || puts != 1 {
		t.Fatalf("health repair: work=%q err=%v puts=%d", work, err, puts)
	}
}

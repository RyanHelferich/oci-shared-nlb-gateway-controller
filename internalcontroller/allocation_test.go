package internalcontroller

import (
	"encoding/json"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"testing"
)

func TestScaleRestartAndTombstones(t *testing.T) {
	p := &api.NLBPool{Spec: api.PoolSpec{CompartmentID: "test", SubnetID: "test", Occupancy: 45, MaxShards: 27, PortStart: 20000}}
	seen := map[string]bool{}
	for i := 0; i < 1200; i++ {
		b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprint(i), UID: types.UID(fmt.Sprint(i))}}
		a, e := Allocate(p, b, fmt.Sprintf("svc-%d", i))
		if e != nil {
			t.Fatal(e)
		}
		slot := fmt.Sprintf("%d/%d", a.Shard, a.Port)
		if seen[slot] {
			t.Fatalf("collision %s", slot)
		}
		seen[slot] = true
	}
	raw, _ := json.Marshal(p)
	restored := &api.NLBPool{}
	_ = json.Unmarshal(raw, restored)
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: "44"}}
	a, e := Allocate(restored, b, "svc-44")
	if e != nil || a.Shard != 0 || a.Port != 20044 {
		t.Fatal("restart changed allocation", a, e)
	}
	a.Tombstone = true
	restored.Status.Allocations["44"] = a
	next, e := Allocate(restored, &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: "recreated-44"}}, "svc-44")
	if e != nil {
		t.Fatal(e)
	}
	if next.Shard == a.Shard && next.Port == a.Port {
		t.Fatal("reused tombstoned port")
	}
	if len(raw) > 900000 {
		t.Fatal("pool status unexpectedly close to API object size bound")
	}
}
func TestCapacityExhaustionAndBounds(t *testing.T) {
	p := &api.NLBPool{Spec: api.PoolSpec{CompartmentID: "test", SubnetID: "test", Occupancy: 2, MaxShards: 2, PortStart: 20000}}
	for i := 0; i < 4; i++ {
		a, e := Allocate(p, &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: types.UID(fmt.Sprint(i))}}, fmt.Sprintf("s-%d", i))
		if e != nil || a.Shard != i/2 {
			t.Fatal(a, e)
		}
	}
	if _, e := Allocate(p, &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{UID: "5"}}, "s-5"); e == nil {
		t.Fatal("expected exhaustion")
	}
	p.Spec.Occupancy = 51
	if ValidatePool(p.Spec) == nil {
		t.Fatal("exceeded documented cap")
	}
	p.Spec.Occupancy = 2
	p.Spec.PortStart = 65535
	if ValidatePool(p.Spec) == nil {
		t.Fatal("port overflow")
	}
}

func TestDuplicateActiveServicePortRejectedBeforeAllocation(t *testing.T) {
	p := &api.NLBPool{Spec: api.PoolSpec{CompartmentID: "test", SubnetID: "test", Occupancy: 2, MaxShards: 2, PortStart: 20000}}
	b := &api.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Name: "first", UID: "first"}, Spec: api.BindingSpec{PortName: "wireguard"}}
	first, err := Allocate(p, b, "service-uid")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Allocate(p, b, "service-uid"); err != nil || again != first {
		t.Fatalf("existing allocation must remain stable: %v %v", again, err)
	}
	b.Name, b.UID = "duplicate", "duplicate"
	if _, err := Allocate(p, b, "service-uid"); err == nil {
		t.Fatal("duplicate active Service port accepted")
	}
	if len(p.Status.Allocations) != 1 || p.Status.Allocations["first"] != first {
		t.Fatal("rejected duplicate changed allocations or original binding")
	}
	b.Spec.PortName = "other-wireguard"
	if _, err := Allocate(p, b, "service-uid"); err != nil {
		t.Fatalf("distinct port on same Service must remain valid: %v", err)
	}
	first.Tombstone = true
	p.Status.Allocations["first"] = first
	b.Name, b.UID, b.Spec.PortName = "replacement", "replacement", "wireguard"
	replacement, err := Allocate(p, b, "service-uid")
	if err != nil || (replacement.Shard == first.Shard && replacement.Port == first.Port) {
		t.Fatalf("retired binding must allow replacement on a fresh endpoint: %v %v", replacement, err)
	}
}

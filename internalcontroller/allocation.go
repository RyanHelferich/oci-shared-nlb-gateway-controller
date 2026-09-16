package internalcontroller

import (
	"crypto/sha256"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
)

func ValidatePool(s api.PoolSpec) error {
	if s.MaxWorkers < 0 || s.MaxWorkers > 512 || s.Occupancy*workerLimit(s) > 1024 {
		return fmt.Errorf("worker capacity requires occupancy * maxWorkers <= 1024; maxWorkers defaults to 20 and includes surge")
	}
	if s.Occupancy < 1 || s.Occupancy > 50 || s.MaxShards < 1 || s.MaxShards > 100 {
		return fmt.Errorf("invalid pool capacity/port bounds")
	}
	switch s.AllocationMode {
	case "", "Dynamic":
		if s.PortStart < 1024 || s.PortStart+s.Occupancy-1 > 65535 {
			return fmt.Errorf("invalid dynamic port bounds")
		}
	case "Explicit":
		if s.MaxShards != 1 || s.PortStart != 1 {
			return fmt.Errorf("Explicit pools require maxShards=1 and portStart=1")
		}
	default:
		return fmt.Errorf("unsupported allocationMode")
	}
	if s.CompartmentID == "" || s.SubnetID == "" {
		return fmt.Errorf("compartment and subnet required")
	}
	return nil
}

func workerLimit(s api.PoolSpec) int {
	if s.MaxWorkers == 0 {
		return 20
	}
	return s.MaxWorkers
}

// Allocate includes tombstones: stale clients must never silently route to a new tenant.
func Allocate(p *api.NLBPool, b *api.TunnelBinding, serviceUID string) (api.Allocation, error) {
	if b.Spec.Suspended {
		return api.Allocation{}, fmt.Errorf("binding suspended")
	}
	if err := ValidatePool(p.Spec); err != nil {
		return api.Allocation{}, err
	}
	if b.UID == "" || serviceUID == "" {
		return api.Allocation{}, fmt.Errorf("binding and Service UIDs required")
	}
	if p.Spec.AllocationMode == "Explicit" {
		if b.Spec.RequestedPort < 1 || b.Spec.RequestedPort > 65535 {
			return api.Allocation{}, fmt.Errorf("Explicit allocation requires requestedPort in 1..65535")
		}
	} else if b.Spec.RequestedPort != 0 {
		return api.Allocation{}, fmt.Errorf("requestedPort requires an Explicit pool")
	}
	if a, ok := p.Status.Allocations[string(b.UID)]; ok {
		if p.Spec.AllocationMode == "Explicit" && a.Port != b.Spec.RequestedPort {
			return api.Allocation{}, fmt.Errorf("requestedPort immutable after allocation")
		}
		return a, nil
	}
	occupied := map[[2]int]bool{}
	for _, a := range p.Status.Allocations {
		// OCI rejects the same backend address/port in two sets on one NLB.
		// One active binding per Service port also avoids ambiguous ownership
		// when allocation would otherwise happen to choose different shards.
		if !a.Tombstone && a.ServiceUID == serviceUID && a.PortName == b.Spec.PortName {
			return api.Allocation{}, fmt.Errorf("Service port already has an active binding in this pool: %s", a.BindingName)
		}
		occupied[[2]int{a.Shard, a.Port}] = true
	}
	if p.Spec.AllocationMode == "Explicit" {
		if occupied[[2]int{0, b.Spec.RequestedPort}] {
			return api.Allocation{}, fmt.Errorf("requested port already leased, including permanent tombstones")
		}
		if len(p.Status.Allocations) >= p.Spec.Occupancy {
			return api.Allocation{}, fmt.Errorf("pool exhausted (including unreusable tombstones)")
		}
		h := sha256.Sum256([]byte(b.UID))
		a := api.Allocation{BindingName: b.Name, ServiceName: b.Spec.Service, ServiceUID: serviceUID, PortName: b.Spec.PortName, Mode: b.Spec.Mode, Shard: 0, Port: b.Spec.RequestedPort, Name: fmt.Sprintf("t-%x", h[:12])}
		if p.Status.Allocations == nil {
			p.Status.Allocations = map[string]api.Allocation{}
		}
		p.Status.Allocations[string(b.UID)] = a
		return a, nil
	}
	for shard := 0; shard < p.Spec.MaxShards; shard++ {
		for offset := 0; offset < p.Spec.Occupancy; offset++ {
			port := p.Spec.PortStart + offset
			if !occupied[[2]int{shard, port}] {
				h := sha256.Sum256([]byte(b.UID))
				a := api.Allocation{BindingName: b.Name, ServiceName: b.Spec.Service, ServiceUID: serviceUID, PortName: b.Spec.PortName, Mode: b.Spec.Mode, Shard: shard, Port: port, Name: fmt.Sprintf("t-%x", h[:12])}
				if p.Status.Allocations == nil {
					p.Status.Allocations = map[string]api.Allocation{}
				}
				p.Status.Allocations[string(b.UID)] = a
				return a, nil
			}
		}
	}
	return api.Allocation{}, fmt.Errorf("pool exhausted (including unreusable tombstones)")
}

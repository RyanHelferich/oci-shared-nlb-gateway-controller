package internalcontroller

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	"github.com/oracle/oci-go-sdk/v65/common"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
)

// Existing releases did not journal NodePort. Upgrade only from a nonempty,
// owned native set whose destinations unanimously prove the current port.
// False is a validation failure; an error is an uncertain read/ownership state
// and must not cause a ledger write or cloud mutation.
func (c *Cloud) verifyLegacyNodePort(ctx context.Context, p *api.NLBPool, a api.Allocation, port int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a.Shard < 0 || a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "" {
		return false, nil
	}
	response, err := c.get(ctx, p.Status.Shards[a.Shard].ID)
	if err != nil {
		return false, err
	}
	lb := response.NetworkLoadBalancer
	if !c.owns(p, a.Shard, lb) || string(lb.LifecycleState) != "ACTIVE" {
		return false, fmt.Errorf("cannot verify legacy NodePort on unowned or inactive NLB")
	}
	owned := map[string]bool{}
	for _, allocation := range p.Status.Allocations {
		if allocation.Shard == a.Shard {
			owned[allocation.Name] = true
		}
	}
	for name := range lb.Listeners {
		if !owned[name] {
			return false, fmt.Errorf("unowned listener prevents NodePort migration")
		}
	}
	for name := range lb.BackendSets {
		if !owned[name] {
			return false, fmt.Errorf("unowned backend set prevents NodePort migration")
		}
	}
	set, found := lb.BackendSets[a.Name]
	if !found || len(set.Backends) == 0 {
		return false, nil
	}
	for _, backend := range set.Backends {
		if value(backend.Port) != port || value(backend.IpAddress) == "" || value(backend.TargetId) == "" {
			return false, nil
		}
	}
	return true, nil
}

type BackendCapacityError struct{ Message string }

func (e *BackendCapacityError) Error() string { return e.Message }

func modeHealth(mode string, port int) *n.HealthCheckerDetails {
	h := health(port)
	if mode == "NodePortCluster" {
		h.Protocol = n.HealthCheckProtocolsHttp
		h.Port = common.Int(10256)
		h.UrlPath = common.String("/healthz")
		h.ReturnCode = common.Int(200)
	}
	return h
}

func sameModeHealth(h *n.HealthChecker, mode string, port int) bool {
	if mode != "NodePortCluster" {
		return sameHealth(h, port)
	}
	return h != nil && h.Protocol == n.HealthCheckProtocolsHttp && value(h.Port) == 10256 && value(h.UrlPath) == "/healthz" && value(h.ReturnCode) == 200 && value(h.IntervalInMillis) == healthIntervalMillis && value(h.TimeoutInMillis) == healthTimeoutMillis && value(h.Retries) == healthRetries && len(h.RequestData) == 0 && len(h.ResponseData) == 0 && value(h.ResponseBodyRegex) == ""
}

func backendListDetails(backends []Backend, name string, current []n.Backend) []n.BackendDetails {
	out := make([]n.BackendDetails, 0, len(backends))
	for i := range backends {
		out = append(out, backendDetailsForUpdate(&backends[i], name, current)...)
	}
	return out
}

func sameBackendList(current []n.Backend, desired []Backend) bool {
	if len(current) != len(desired) {
		return false
	}
	seen := map[Backend]bool{}
	for _, b := range desired {
		if seen[b] {
			return false
		}
		seen[b] = true
		found := false
		for _, existing := range current {
			if sameBackends([]n.Backend{existing}, &b) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validateBackendBudget(lb n.NetworkLoadBalancer, own string, desired []Backend) error {
	if len(desired) > 512 {
		return &BackendCapacityError{"backend set exceeds 512 destinations"}
	}
	total := len(desired)
	seen := map[string]bool{}
	for _, b := range desired {
		key := fmt.Sprintf("%s:%d", b.IP, b.Port)
		if seen[key] {
			return fmt.Errorf("duplicate desired backend destination")
		}
		seen[key] = true
	}
	for name, set := range lb.BackendSets {
		if name == own {
			continue
		}
		total += len(set.Backends)
	}
	if total > 1024 {
		return &BackendCapacityError{"NLB backend capacity exceeded: maximum 1024 including all backend sets"}
	}
	return nil
}

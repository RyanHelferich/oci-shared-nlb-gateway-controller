package internalcontroller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"github.com/oracle/oci-go-sdk/v65/common"
	n "github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
)

type Backend struct {
	IP       string
	TargetID string
	Port     int
}
type Cloud struct {
	Client       n.NetworkLoadBalancerClient
	Installation string
	snapshot     map[string]n.GetNetworkLoadBalancerResponse
}

// ResetSnapshot begins a single serialized reconciliation. Never retain a cloud
// snapshot across reconciliations: external changes and completed work must be seen.
func (c *Cloud) ResetSnapshot() {
	c.snapshot = make(map[string]n.GetNetworkLoadBalancerResponse)
}

func (c *Cloud) get(ctx context.Context, id string) (n.GetNetworkLoadBalancerResponse, error) {
	if response, ok := c.snapshot[id]; ok {
		return response, nil
	}
	response, err := timedOCI("GetNetworkLoadBalancer", func() (n.GetNetworkLoadBalancerResponse, error) {
		return c.Client.GetNetworkLoadBalancer(ctx, n.GetNetworkLoadBalancerRequest{NetworkLoadBalancerId: &id})
	})
	if err == nil && c.snapshot != nil {
		c.snapshot[id] = response
	}
	return response, err
}

func value[T any](p *T) (z T) {
	if p != nil {
		return *p
	}
	return
}
func (c *Cloud) tags(p *api.NLBPool, shard int) map[string]string {
	return map[string]string{"controller": "independent-shared-nlb", "installation": c.Installation, "project": c.Installation, "pool-uid": string(p.UID), "shard": strconv.Itoa(shard)}
}
func (c *Cloud) owns(p *api.NLBPool, shard int, lb n.NetworkLoadBalancer) bool {
	if value(lb.CompartmentId) != p.Spec.CompartmentID || value(lb.SubnetId) != p.Spec.SubnetID {
		return false
	}
	for k, v := range c.tags(p, shard) {
		// Cost/inventory tagging is not an ownership credential. Older
		// installations did not set it; the four identity tags remain required.
		if k == "project" {
			continue
		}
		if lb.FreeformTags[k] != v {
			return false
		}
	}
	return true
}
func (c *Cloud) Work(ctx context.Context, id string) (bool, error) {
	r, e := timedOCI("GetWorkRequest", func() (n.GetWorkRequestResponse, error) {
		return c.Client.GetWorkRequest(ctx, n.GetWorkRequestRequest{WorkRequestId: &id})
	})
	if e != nil {
		return false, e
	}
	switch string(r.Status) {
	case "SUCCEEDED":
		return true, nil
	case "FAILED", "CANCELED":
		return true, fmt.Errorf("OCI work request %s ended %s", id, r.Status)
	}
	return false, nil
}

// EnsureShard discovers owned resources before creating, including after lost responses.
func (c *Cloud) EnsureShard(ctx context.Context, p *api.NLBPool, index int) (api.Shard, string, error) {
	if index < 0 || index >= p.Spec.MaxShards {
		return api.Shard{}, "", fmt.Errorf("invalid shard index %d", index)
	}
	var id string
	if len(p.Status.Shards) > index {
		id = p.Status.Shards[index].ID
	}
	if id == "" {
		var page *string
		for {
			r, e := timedOCI("ListNetworkLoadBalancers", func() (n.ListNetworkLoadBalancersResponse, error) {
				return c.Client.ListNetworkLoadBalancers(ctx, n.ListNetworkLoadBalancersRequest{CompartmentId: &p.Spec.CompartmentID, Page: page})
			})
			if e != nil {
				return api.Shard{}, "", e
			}
			for _, s := range r.Items {
				if s.FreeformTags["installation"] == c.Installation && s.FreeformTags["pool-uid"] == string(p.UID) && s.FreeformTags["shard"] == strconv.Itoa(index) && string(s.LifecycleState) != "DELETED" {
					if id != "" {
						return api.Shard{}, "", fmt.Errorf("duplicate owned shard; manual repair required")
					}
					id = value(s.Id)
				}
			}
			if r.OpcNextPage == nil {
				break
			}
			page = r.OpcNextPage
		}
	}
	if id == "" {
		token := fmt.Sprintf("%s-%d", p.UID, index)
		r, e := timedOCI("CreateNetworkLoadBalancer", func() (n.CreateNetworkLoadBalancerResponse, error) {
			return c.Client.CreateNetworkLoadBalancer(ctx, n.CreateNetworkLoadBalancerRequest{OpcRetryToken: &token, CreateNetworkLoadBalancerDetails: n.CreateNetworkLoadBalancerDetails{CompartmentId: &p.Spec.CompartmentID, SubnetId: &p.Spec.SubnetID, DisplayName: common.String("shared-nlb-" + token), IsPrivate: common.Bool(false), IsPreserveSourceDestination: common.Bool(false), FreeformTags: c.tags(p, index)}})
		})
		if e != nil {
			return api.Shard{}, "", e
		}
		return api.Shard{ID: value(r.Id)}, value(r.OpcWorkRequestId), nil
	}
	r, e := c.get(ctx, id)
	if e != nil {
		return api.Shard{}, "", e
	}
	if !c.owns(p, index, r.NetworkLoadBalancer) {
		return api.Shard{}, "", fmt.Errorf("refusing NLB ownership/scope mismatch")
	}
	if string(r.LifecycleState) != "ACTIVE" {
		return api.Shard{ID: id}, "", fmt.Errorf("owned NLB not ACTIVE: %s", r.LifecycleState)
	}
	s := api.Shard{ID: id}
	for _, ip := range r.IpAddresses {
		if value(ip.IsPublic) {
			s.PublicIP = value(ip.IpAddress)
		}
	}
	return s, "", nil
}

// Match the reviewed Oracle CCM defaults. Aggressive per-binding probes multiply
// across shared shards and can exhaust worker VNIC connection-tracking capacity.
const healthIntervalMillis, healthTimeoutMillis, healthRetries = 10000, 3000, 3

func health(port int) *n.HealthCheckerDetails {
	return &n.HealthCheckerDetails{Protocol: n.HealthCheckProtocolsTcp, Port: &port, IntervalInMillis: common.Int(healthIntervalMillis), TimeoutInMillis: common.Int(healthTimeoutMillis), Retries: common.Int(healthRetries)}
}
func backendDetails(b *Backend, bindingName string) []n.BackendDetails {
	if b == nil {
		return []n.BackendDetails{}
	}
	// Backend APIs identify destinations by name. Include all verified target
	// fields instead of reusing one binding slot for different destinations.
	// This avoids identity ambiguity; flow recovery still requires live tests.
	// The binding prefix avoids collisions across backend sets on one NLB.
	destination := sha256.Sum256([]byte(b.TargetID + "\x00" + b.IP + "\x00" + strconv.Itoa(b.Port)))
	name := fmt.Sprintf("%s-%x", bindingName, destination[:12])
	d := n.BackendDetails{Name: &name, IpAddress: &b.IP, Port: &b.Port, Weight: common.Int(1), IsOffline: common.Bool(false), IsDrain: common.Bool(false), IsBackup: common.Bool(false)}
	if b.TargetID != "" {
		d.TargetId = &b.TargetID
	}
	return []n.BackendDetails{d}
}

func backendDetailsForUpdate(b *Backend, bindingName string, current []n.Backend) []n.BackendDetails {
	details := backendDetails(b, bindingName)
	if b == nil {
		return details
	}
	for _, existing := range current {
		if value(existing.Name) != "" && value(existing.IpAddress) == b.IP && value(existing.TargetId) == b.TargetID && value(existing.Port) == b.Port {
			// Repairing health/flags must not replace a correctly identified
			// destination merely because an older release used another name.
			details[0].Name = existing.Name
			break
		}
	}
	return details
}
func sameBackends(current []n.Backend, b *Backend) bool {
	if b == nil {
		return len(current) == 0
	}
	if len(current) != 1 {
		return false
	}
	x := current[0]
	return value(x.IpAddress) == b.IP && value(x.TargetId) == b.TargetID && value(x.Port) == b.Port && value(x.Weight) == 1 && !value(x.IsOffline) && !value(x.IsDrain) && !value(x.IsBackup)
}

func sameHealth(h *n.HealthChecker, port int) bool {
	return h != nil && h.Protocol == n.HealthCheckProtocolsTcp && value(h.Port) == port && value(h.IntervalInMillis) == healthIntervalMillis && value(h.TimeoutInMillis) == healthTimeoutMillis && value(h.Retries) == healthRetries && len(h.RequestData) == 0 && len(h.ResponseData) == 0
}

// destinationConflict uses the same per-reconcile snapshot as EnsureShard and
// Step. OCI rejects identical destination IP/port pairs across backend sets on
// one NLB, even when different Services/bindings name the same Pod.
func (c *Cloud) destinationConflict(ctx context.Context, p *api.NLBPool, a api.Allocation, b *Backend) (bool, error) {
	if a.Shard < 0 || a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "" {
		return false, fmt.Errorf("allocation has no provisioned shard")
	}
	response, err := c.get(ctx, p.Status.Shards[a.Shard].ID)
	if err != nil {
		return false, err
	}
	if !c.owns(p, a.Shard, response.NetworkLoadBalancer) {
		return false, fmt.Errorf("refusing NLB ownership mismatch")
	}
	for name, set := range response.BackendSets {
		if name == a.Name {
			continue
		}
		for _, existing := range set.Backends {
			if value(existing.IpAddress) == b.IP && value(existing.Port) == b.Port {
				return true, nil
			}
		}
	}
	return false, nil
}

// Step performs at most one asynchronous mutation; caller persists its work ID.
func (c *Cloud) Step(ctx context.Context, p *api.NLBPool, a api.Allocation, b *Backend, healthPort int, remove bool) (string, error) {
	var backends []Backend
	if b != nil {
		backends = []Backend{*b}
	}
	return c.StepBackends(ctx, p, a, backends, healthPort, remove)
}

func (c *Cloud) StepBackends(ctx context.Context, p *api.NLBPool, a api.Allocation, backends []Backend, healthPort int, remove bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.Shard < 0 || a.Shard >= len(p.Status.Shards) || p.Status.Shards[a.Shard].ID == "" {
		return "", fmt.Errorf("allocation has no provisioned shard")
	}
	id := p.Status.Shards[a.Shard].ID
	r, e := c.get(ctx, id)
	if e != nil {
		return "", e
	}
	lb := r.NetworkLoadBalancer
	if !c.owns(p, a.Shard, lb) {
		return "", fmt.Errorf("refusing NLB ownership mismatch")
	}
	if string(lb.LifecycleState) != "ACTIVE" {
		return "", fmt.Errorf("NLB update in progress")
	}
	// Detect foreign additions instead of overwriting them.
	ownedNames := map[string]bool{}
	for _, v := range p.Status.Allocations {
		if v.Shard == a.Shard {
			ownedNames[v.Name] = true
		}
	}
	for name := range lb.Listeners {
		if !ownedNames[name] {
			return "", fmt.Errorf("unowned listener on shard")
		}
	}
	for name := range lb.BackendSets {
		if !ownedNames[name] {
			return "", fmt.Errorf("unowned backend set on shard")
		}
	}
	listener, hasListener := lb.Listeners[a.Name]
	bs, hasSet := lb.BackendSets[a.Name]
	if remove {
		if hasListener {
			delete(c.snapshot, id)
			x, e := timedOCI("DeleteListener", func() (n.DeleteListenerResponse, error) {
				return c.Client.DeleteListener(ctx, n.DeleteListenerRequest{NetworkLoadBalancerId: &id, ListenerName: &a.Name})
			})
			return value(x.OpcWorkRequestId), e
		}
		if hasSet {
			delete(c.snapshot, id)
			x, e := timedOCI("DeleteBackendSet", func() (n.DeleteBackendSetResponse, error) {
				return c.Client.DeleteBackendSet(ctx, n.DeleteBackendSetRequest{NetworkLoadBalancerId: &id, BackendSetName: &a.Name})
			})
			return value(x.OpcWorkRequestId), e
		}
		return "", nil
	}
	for _, b := range backends {
		if p.Spec.PreserveSource && (b.TargetID == "" || a.Mode == "NodePortCluster") {
			return "", fmt.Errorf("source preservation requires verified instance target and is unsupported in NodePortCluster")
		}
	}
	if a.Mode == "NodePortCluster" && len(backends) > workerLimit(p.Spec) {
		return "", &BackendCapacityError{"desired workers exceed configured maxWorkers including surge"}
	}
	if err := validateBackendBudget(lb, a.Name, backends); err != nil {
		return "", err
	}
	if !hasSet {
		delete(c.snapshot, id)
		x, e := timedOCI("CreateBackendSet", func() (n.CreateBackendSetResponse, error) {
			return c.Client.CreateBackendSet(ctx, n.CreateBackendSetRequest{NetworkLoadBalancerId: &id, CreateBackendSetDetails: n.CreateBackendSetDetails{Name: &a.Name, Policy: n.NetworkLoadBalancingPolicyFiveTuple, HealthChecker: modeHealth(a.Mode, healthPort), Backends: backendListDetails(backends, a.Name, nil), IsPreserveSource: &p.Spec.PreserveSource, IsFailOpen: common.Bool(false), IsInstantFailoverEnabled: common.Bool(true)}})
		})
		return value(x.OpcWorkRequestId), e
	}
	if !sameBackendList(bs.Backends, backends) || !sameModeHealth(bs.HealthChecker, a.Mode, healthPort) || bs.Policy != n.NetworkLoadBalancingPolicyFiveTuple || value(bs.IsPreserveSource) != p.Spec.PreserveSource || value(bs.IsFailOpen) || !value(bs.IsInstantFailoverEnabled) {
		delete(c.snapshot, id)
		x, e := timedOCI("UpdateBackendSet", func() (n.UpdateBackendSetResponse, error) {
			return c.Client.UpdateBackendSet(ctx, n.UpdateBackendSetRequest{NetworkLoadBalancerId: &id, BackendSetName: &a.Name, UpdateBackendSetDetails: n.UpdateBackendSetDetails{Policy: common.String("FIVE_TUPLE"), HealthChecker: modeHealth(a.Mode, healthPort), Backends: backendListDetails(backends, a.Name, bs.Backends), IsPreserveSource: &p.Spec.PreserveSource, IsFailOpen: common.Bool(false), IsInstantFailoverEnabled: common.Bool(true)}})
		})
		return value(x.OpcWorkRequestId), e
	}
	if !hasListener {
		delete(c.snapshot, id)
		x, e := timedOCI("CreateListener", func() (n.CreateListenerResponse, error) {
			return c.Client.CreateListener(ctx, n.CreateListenerRequest{NetworkLoadBalancerId: &id, CreateListenerDetails: n.CreateListenerDetails{Name: &a.Name, DefaultBackendSetName: &a.Name, Port: &a.Port, Protocol: n.ListenerProtocolsUdp, UdpIdleTimeout: common.Int(120)}})
		})
		return value(x.OpcWorkRequestId), e
	}
	if value(listener.Port) != a.Port || value(listener.DefaultBackendSetName) != a.Name || string(listener.Protocol) != "UDP" || value(listener.IsPpv2Enabled) || value(listener.UdpIdleTimeout) != 120 {
		return "", fmt.Errorf("listener drift requires operator repair")
	}
	return "", nil
}

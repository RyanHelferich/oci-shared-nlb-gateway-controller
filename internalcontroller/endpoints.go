package internalcontroller

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

// EndpointReadError means the API observation failed, not that the observed
// routing state was unsafe. Retrying must not remove an existing backend.
type EndpointReadError struct{ Err error }

func (e *EndpointReadError) Error() string {
	return "unable to observe tunnel endpoint: " + e.Err.Error()
}
func (e *EndpointReadError) Unwrap() error { return e.Err }

func endpointGetError(err error) error {
	// A successful API response proving that a required object is absent is
	// different from a timeout, authorization failure or unavailable API.
	if apierrors.IsNotFound(err) {
		return err
	}
	return &EndpointReadError{Err: err}
}

// BackendUnavailable is a verified gap in ready endpoints, not an ownership
// error. A previously configured, still-verified NodePort may be left in place
// under native health checking until an eligible endpoint appears.
type BackendUnavailable struct {
	NodePort   int
	HealthPort int
}

func (e *BackendUnavailable) Error() string {
	return "waiting for one ready endpoint; any retained NodePort remains subject to native health checks"
}

func Resolve(ctx context.Context, c client.Reader, b *api.TunnelBinding, a api.Allocation) (*Backend, error) {
	svc := &core.Service{}
	if e := c.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.Service}, svc); e != nil {
		return nil, endpointGetError(e)
	}
	if string(svc.UID) != a.ServiceUID || !svc.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("Service UID changed; create a new binding")
	}
	if b.Spec.Mode == "PodIP" {
		return resolvePodIP(ctx, c, b, svc)
	}
	if b.Spec.Mode != "NodePort" || svc.Spec.Type != core.ServiceTypeNodePort || svc.Spec.ExternalTrafficPolicy != core.ServiceExternalTrafficPolicyLocal {
		return nil, fmt.Errorf("requires NodePort Service with externalTrafficPolicy Local")
	}
	if svc.Spec.PublishNotReadyAddresses {
		return nil, fmt.Errorf("publishNotReadyAddresses is unsafe")
	}
	// The health check must belong to the referenced Service, not an arbitrary
	// port on the selected node.
	healthFound := false
	for _, p := range svc.Spec.Ports {
		if p.Protocol == core.ProtocolTCP && int(p.NodePort) == b.Spec.HealthPort {
			healthFound = true
		}
	}
	if !healthFound {
		return nil, fmt.Errorf("healthPort must match a TCP NodePort on the referenced Service")
	}
	var port core.ServicePort
	for _, v := range svc.Spec.Ports {
		if v.Name == b.Spec.PortName && v.Protocol == core.ProtocolUDP {
			port = v
		}
	}
	if port.NodePort == 0 {
		return nil, fmt.Errorf("named UDP NodePort missing")
	}
	var slices discovery.EndpointSliceList
	if e := c.List(ctx, &slices, client.InNamespace(b.Namespace), client.MatchingLabels{discovery.LabelServiceName: svc.Name}); e != nil {
		// An empty successful list establishes no endpoints. A failed list,
		// including a missing API resource, establishes no such fact.
		return nil, &EndpointReadError{Err: e}
	}
	endpoints := map[types.UID]discovery.Endpoint{}
	var terminating []discovery.Endpoint
	for _, s := range slices.Items {
		validOwner := false
		for _, o := range s.OwnerReferences {
			if o.UID == svc.UID && o.Kind == "Service" {
				validOwner = true
			}
		}
		// kube-proxy consumes labelled EndpointSlices without checking their
		// owner reference. Ignoring a foreign slice could therefore route a
		// supposedly verified NodePort to an unverified Pod on that same node.
		if !validOwner {
			return nil, fmt.Errorf("Service has an EndpointSlice with unverified ownership")
		}
		if s.AddressType != discovery.AddressTypeIPv4 {
			continue
		}
		validPort := false
		for _, p := range s.Ports {
			if value(p.Name) == b.Spec.PortName && p.Protocol != nil && *p.Protocol == core.ProtocolUDP && p.Port != nil && *p.Port > 0 {
				validPort = true
			}
		}
		if !validPort {
			continue
		}
		for _, ep := range s.Endpoints {
			if value(ep.Conditions.Terminating) {
				// A ready replacement is the preferred Local destination. Validate
				// draining Pods only if retention of a gap is actually needed.
				if ep.Conditions.Serving == nil || value(ep.Conditions.Serving) {
					terminating = append(terminating, ep)
				}
				continue
			}
			if ep.Conditions.Ready == nil {
				return nil, fmt.Errorf("endpoint readiness is unknown")
			}
			if !value(ep.Conditions.Ready) {
				continue
			}
			if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" || ep.TargetRef.Namespace != b.Namespace || ep.TargetRef.UID == "" {
				return nil, fmt.Errorf("endpoint lacks same-namespace Pod ownership")
			}
			if len(ep.Addresses) != 1 || net.ParseIP(ep.Addresses[0]).To4() == nil {
				return nil, fmt.Errorf("endpoint must contain exactly one IPv4 address")
			}
			if old, ok := endpoints[ep.TargetRef.UID]; ok && (old.TargetRef.Name != ep.TargetRef.Name || old.Addresses[0] != ep.Addresses[0] || value(old.NodeName) != value(ep.NodeName)) {
				return nil, fmt.Errorf("conflicting EndpointSlices for the same Pod")
			}
			endpoints[ep.TargetRef.UID] = ep
		}
	}
	gap := func() (*Backend, error) {
		owners := map[types.UID]bool{}
		for _, ep := range terminating {
			if _, err := readEndpointPod(ctx, c, b, svc, ep); err != nil {
				return nil, err
			}
			owners[ep.TargetRef.UID] = true
		}
		if len(owners) > 1 {
			return nil, fmt.Errorf("multiple serving terminating endpoints are ambiguous")
		}
		return nil, &BackendUnavailable{NodePort: int(port.NodePort), HealthPort: b.Spec.HealthPort}
	}
	if len(endpoints) == 0 {
		return gap()
	}
	if len(endpoints) != 1 {
		return nil, fmt.Errorf("requires exactly one ready nonterminating endpoint, found %d", len(endpoints))
	}
	var ep discovery.Endpoint
	for _, v := range endpoints {
		ep = v
	}
	pod, e := readEndpointPod(ctx, c, b, svc, ep)
	if e != nil {
		return nil, e
	}
	ready := false
	for _, v := range pod.Status.Conditions {
		if v.Type == core.PodReady && v.Status == core.ConditionTrue {
			ready = true
		}
	}
	if !ready || !pod.DeletionTimestamp.IsZero() {
		// Pod state can change before its EndpointSlice readiness catches up.
		// Its identity/address/selector were verified above; this is the same
		// availability gap as observing no ready endpoint in the slice.
		return gap()
	}
	node := &core.Node{}
	if e := c.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); e != nil {
		return nil, endpointGetError(e)
	}
	ready = false
	for _, v := range node.Status.Conditions {
		if v.Type == core.NodeReady && v.Status == core.ConditionTrue {
			ready = true
		}
	}
	if !ready || !node.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("active node not ready")
	}
	instance := strings.TrimPrefix(node.Spec.ProviderID, "oci://")
	if !strings.HasPrefix(instance, "ocid1.instance.") {
		return nil, fmt.Errorf("missing OCI node instance identity")
	}
	for _, v := range node.Status.Addresses {
		if v.Type == core.NodeInternalIP && net.ParseIP(v.Address).To4() != nil {
			return &Backend{IP: v.Address, TargetID: instance, Port: int(port.NodePort)}, nil
		}
	}
	return nil, fmt.Errorf("node has no IPv4 internal address")
}

func readEndpointPod(ctx context.Context, c client.Reader, b *api.TunnelBinding, svc *core.Service, ep discovery.Endpoint) (*core.Pod, error) {
	if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" || ep.TargetRef.Namespace != b.Namespace || ep.TargetRef.UID == "" {
		return nil, fmt.Errorf("endpoint lacks same-namespace Pod ownership")
	}
	if len(ep.Addresses) != 1 || net.ParseIP(ep.Addresses[0]).To4() == nil {
		return nil, fmt.Errorf("endpoint must contain exactly one IPv4 address")
	}
	pod := &core.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: ep.TargetRef.Name}, pod); err != nil {
		return nil, endpointGetError(err)
	}
	if pod.UID != ep.TargetRef.UID {
		return nil, fmt.Errorf("stale Pod identity")
	}
	if len(svc.Spec.Selector) > 0 && !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)) {
		return nil, fmt.Errorf("Pod no longer matches Service selector")
	}
	if ep.NodeName != nil && *ep.NodeName != pod.Spec.NodeName {
		return nil, fmt.Errorf("endpoint node does not match Pod node")
	}
	if ep.Addresses[0] != pod.Status.PodIP {
		return nil, fmt.Errorf("Pod address does not match endpoint")
	}
	return pod, nil
}

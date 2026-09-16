// Package internalgateway translates a deliberately restricted Gateway API UDP
// profile into the existing durable NLB engine. It never calls OCI itself.
package internalgateway

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	GatewayFinalizer      = "nlb.independent.dev/gateway-cleanup"
	RouteFinalizer        = "nlb.independent.dev/gateway-route-cleanup"
	GatewayUIDLabel       = "nlb.independent.dev/gateway-uid"
	RouteUIDLabel         = "nlb.independent.dev/route-uid"
	InstallationLabel     = "nlb.independent.dev/installation"
	HealthPortAnnotation  = "nlb.independent.dev/health-port"
	BackendModeAnnotation = "nlb.independent.dev/backend-mode"
	MaxWorkersAnnotation  = "nlb.independent.dev/max-workers"
)

func gatewayWorkers(g *gateway.Gateway) (int, int, error) {
	configured := 0
	if text := g.Annotations[MaxWorkersAnnotation]; text != "" {
		var err error
		configured, err = strconv.Atoi(text)
		if err != nil || configured < 1 || configured > 512 {
			return 0, 0, fmt.Errorf("max-workers must be 1..512 including surge")
		}
	}
	effective := configured
	if effective == 0 {
		effective = 20
	}
	return configured, min(50, 1024/effective), nil
}

func PoolName(uid types.UID) string {
	h := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("gw-%x", h[:12])
}
func BindingName(gatewayUID, routeUID types.UID) string {
	h := sha256.Sum256([]byte(string(gatewayUID) + "\x00" + string(routeUID)))
	return fmt.Sprintf("route-%x", h[:12])
}
func condition(kind string, ok bool, reason, message string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation}
}

func parentMatches(parent gateway.ParentReference, g *gateway.Gateway, namespace string) bool {
	return (parent.Group == nil || *parent.Group == gateway.GroupName) && (parent.Kind == nil || *parent.Kind == "Gateway") && (parent.Namespace == nil || string(*parent.Namespace) == namespace) && string(parent.Name) == g.Name
}

func validateGateway(g *gateway.Gateway) error {
	_, capacity, err := gatewayWorkers(g)
	if err != nil {
		return err
	}
	if len(g.Spec.Listeners) > capacity {
		return fmt.Errorf("listener count times maxWorkers exceeds 1024 backend budget")
	}
	if len(g.Spec.Addresses) != 0 || g.Spec.Infrastructure != nil || g.Spec.AllowedListeners != nil || g.Spec.TLS != nil || (g.Spec.DefaultScope != "" && g.Spec.DefaultScope != "None") {
		return fmt.Errorf("requested addresses, infrastructure, ListenerSets, TLS and default Gateway attachment are unsupported")
	}
	if len(g.Spec.Listeners) < 1 || len(g.Spec.Listeners) > 50 {
		return fmt.Errorf("one to 50 UDP listeners required")
	}
	ports := map[gateway.PortNumber]bool{}
	names := map[gateway.SectionName]bool{}
	for _, l := range g.Spec.Listeners {
		if l.Name == "" || l.Protocol != gateway.UDPProtocolType || l.Port < 1 || l.Port > 65535 || l.TLS != nil || l.Hostname != nil {
			return fmt.Errorf("only named UDP listeners without TLS or hostname are supported")
		}
		if ports[l.Port] || names[l.Name] {
			return fmt.Errorf("listener names and UDP ports must be distinct")
		}
		ports[l.Port] = true
		names[l.Name] = true
		if l.AllowedRoutes != nil {
			a := l.AllowedRoutes
			if a.Namespaces != nil && ((a.Namespaces.From != nil && *a.Namespaces.From != gateway.NamespacesFromSame) || a.Namespaces.Selector != nil) {
				return fmt.Errorf("only same-namespace routes are supported")
			}
			for _, k := range a.Kinds {
				if k.Kind != "UDPRoute" || (k.Group != nil && *k.Group != gateway.GroupName) {
					return fmt.Errorf("only UDPRoute is supported")
				}
			}
		}
	}
	return nil
}

func listenerFor(route *gateway.UDPRoute, g *gateway.Gateway) (*gateway.Listener, error) {
	if route.Spec.UseDefaultGateways != "" && route.Spec.UseDefaultGateways != "None" {
		return nil, fmt.Errorf("default Gateway attachment is unsupported")
	}
	if len(route.Spec.ParentRefs) != 1 {
		return nil, fmt.Errorf("exactly one Gateway parent required")
	}
	p := route.Spec.ParentRefs[0]
	if !parentMatches(p, g, route.Namespace) || p.SectionName == nil || *p.SectionName == "" || p.Port != nil {
		return nil, fmt.Errorf("same-namespace Gateway parent with sectionName and no port selector required")
	}
	for i := range g.Spec.Listeners {
		if g.Spec.Listeners[i].Name == *p.SectionName {
			return &g.Spec.Listeners[i], nil
		}
	}
	return nil, fmt.Errorf("parent listener does not exist")
}

func backendService(route *gateway.UDPRoute) (string, int, error) {
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		return "", 0, fmt.Errorf("exactly one rule and one Service backend required")
	}
	ref := route.Spec.Rules[0].BackendRefs[0]
	if (ref.Group != nil && *ref.Group != "") || (ref.Kind != nil && *ref.Kind != "Service") || (ref.Namespace != nil && string(*ref.Namespace) != route.Namespace) || ref.Port == nil || *ref.Port < 1 || *ref.Port > 65535 || ref.Name == "" || (ref.Weight != nil && *ref.Weight != 1) {
		return "", 0, fmt.Errorf("one same-namespace core Service with numeric port and weight one required")
	}
	return string(ref.Name), int(*ref.Port), nil
}

func desiredBinding(route *gateway.UDPRoute, g *gateway.Gateway, listener *gateway.Listener, svc *core.Service) (api.BindingSpec, error) {
	name, port, err := backendService(route)
	if err != nil {
		return api.BindingSpec{}, err
	}
	mode := route.Annotations[BackendModeAnnotation]
	if mode == "" {
		mode = "PodIP"
	}
	if mode != "PodIP" && mode != "NodePortCluster" {
		return api.BindingSpec{}, fmt.Errorf("unsupported backend-mode")
	}
	serviceTypeOK := (svc.Spec.Type == "" || svc.Spec.Type == core.ServiceTypeClusterIP)
	if mode == "NodePortCluster" {
		serviceTypeOK = svc.Spec.Type == core.ServiceTypeNodePort && svc.Spec.ExternalTrafficPolicy == core.ServiceExternalTrafficPolicyCluster
	}
	if svc.Name != name || svc.Namespace != route.Namespace || svc.UID == "" || !svc.DeletionTimestamp.IsZero() || !serviceTypeOK || len(svc.Spec.Selector) == 0 || svc.Spec.PublishNotReadyAddresses {
		return api.BindingSpec{}, fmt.Errorf("backend must be a live selector-backed ClusterIP Service with Ready endpoints")
	}
	health, err := strconv.Atoi(route.Annotations[HealthPortAnnotation])
	if mode == "NodePortCluster" {
		if annotation := route.Annotations[HealthPortAnnotation]; annotation != "" && annotation != "10256" {
			return api.BindingSpec{}, fmt.Errorf("NodePortCluster uses fixed worker health port 10256; omit workload health annotation")
		}
		health, err = 10256, nil
	}
	if err != nil || health < 1 || health > 65535 {
		return api.BindingSpec{}, fmt.Errorf("annotation %s must specify TCP pod health port 1..65535", HealthPortAnnotation)
	}
	selected := ""
	matches := 0
	healthFound := false
	for _, p := range svc.Spec.Ports {
		if p.Protocol == core.ProtocolUDP && int(p.Port) == port {
			if mode == "NodePortCluster" && (p.NodePort < 1 || p.NodePort > 65535) {
				return api.BindingSpec{}, fmt.Errorf("NodePortCluster requires an allocated UDP NodePort")
			}
			selected = p.Name
			matches++
		}
		// Named target ports are resolved and checked against the actual owned Pod
		// and EndpointSlice by the sole cloud-writing core reconciler.
		if p.Protocol == core.ProtocolTCP && (p.TargetPort.StrVal != "" || int(p.TargetPort.IntVal) == health || (p.TargetPort.IntVal == 0 && int(p.Port) == health)) {
			healthFound = true
		}
	}
	if matches != 1 || selected == "" {
		return api.BindingSpec{}, fmt.Errorf("backend port must identify one named UDP Service port")
	}
	if !healthFound && mode != "NodePortCluster" {
		return api.BindingSpec{}, fmt.Errorf("Service must expose a TCP target for the pod health port")
	}
	return api.BindingSpec{Pool: PoolName(g.UID), Service: name, PortName: selected, Mode: mode, HealthPort: health, RequestedPort: int(listener.Port)}, nil
}

package internalcontroller

import (
	"context"
	"fmt"

	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Validate the explicit transport choice before consuming an allocation. PodIP
// requires operator-provisioned VCN reachability; Kubernetes ownership checks
// alone cannot establish the cluster's CNI or OCI network rules.
func validateBindingService(b *api.TunnelBinding, svc *core.Service, preserveSource bool) error {
	if b.Spec.HealthPort < 1 || b.Spec.HealthPort > 65535 {
		return fmt.Errorf("valid healthPort required")
	}
	switch b.Spec.Mode {
	case "NodePortCluster":
		if preserveSource || b.Spec.HealthPort != 10256 || svc.Spec.Type != core.ServiceTypeNodePort || svc.Spec.ExternalTrafficPolicy != core.ServiceExternalTrafficPolicyCluster || len(svc.Spec.Selector) == 0 || svc.Spec.PublishNotReadyAddresses {
			return fmt.Errorf("NodePortCluster requires selector-backed NodePort Service, externalTrafficPolicy Cluster, healthPort 10256, publishNotReadyAddresses false and preserveSource false")
		}
	case "NodePort":
		if svc.Spec.Type != core.ServiceTypeNodePort {
			return fmt.Errorf("NodePort mode requires a NodePort Service")
		}
	case "PodIP":
		if svc.Spec.Type != core.ServiceTypeClusterIP || len(svc.Spec.Selector) == 0 {
			return fmt.Errorf("PodIP mode requires a selector-backed ClusterIP Service")
		}
		if preserveSource {
			return fmt.Errorf("PodIP mode requires preserveSource false")
		}
	default:
		return fmt.Errorf("unsupported binding mode")
	}
	return nil
}

// No BackendUnavailable is returned here: retaining a removed Pod IP could
// direct traffic to a different Pod after address reuse. API observation errors
// remain operational errors and never establish that an endpoint is absent.
func resolvePodIP(ctx context.Context, c client.Reader, b *api.TunnelBinding, svc *core.Service) (*Backend, error) {
	if err := validateBindingService(b, svc, false); err != nil {
		return nil, err
	}
	if svc.Spec.PublishNotReadyAddresses {
		return nil, fmt.Errorf("publishNotReadyAddresses is unsafe")
	}
	var udp *core.ServicePort
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if p.Name == b.Spec.PortName && p.Protocol == core.ProtocolUDP {
			if udp != nil {
				return nil, fmt.Errorf("ambiguous UDP Service port")
			}
			udp = p
		}
	}
	if udp == nil {
		return nil, fmt.Errorf("named UDP Service port missing")
	}
	var slices discovery.EndpointSliceList
	if err := c.List(ctx, &slices, client.InNamespace(b.Namespace), client.MatchingLabels{discovery.LabelServiceName: svc.Name}); err != nil {
		return nil, &EndpointReadError{Err: err}
	}
	var chosen *Backend
	var chosenUID types.UID
	for _, slice := range slices.Items {
		owned := false
		for _, owner := range slice.OwnerReferences {
			if owner.Kind == "Service" && owner.UID == svc.UID {
				owned = true
			}
		}
		if !owned {
			return nil, fmt.Errorf("Service has an EndpointSlice with unverified ownership")
		}
		if slice.AddressType != discovery.AddressTypeIPv4 {
			continue
		}
		udpPort, err := slicePort(slice.Ports, udp.Name, core.ProtocolUDP)
		if err != nil {
			return nil, err
		}
		if udpPort == 0 {
			continue
		}
		for _, ep := range slice.Endpoints {
			if value(ep.Conditions.Terminating) {
				continue
			}
			if ep.Conditions.Ready == nil {
				return nil, fmt.Errorf("endpoint readiness is unknown")
			}
			if !*ep.Conditions.Ready {
				continue
			}
			pod, err := readEndpointPod(ctx, c, b, svc, ep)
			if err != nil {
				return nil, err
			}
			if pod.Spec.HostNetwork || !pod.DeletionTimestamp.IsZero() || !podReady(pod) {
				return nil, fmt.Errorf("PodIP requires a ready nonterminating Pod without hostNetwork")
			}
			resolved, err := podTargetPort(*udp, pod)
			if err != nil || resolved != udpPort {
				return nil, fmt.Errorf("UDP EndpointSlice port does not match the Pod's Service targetPort")
			}
			healthFound := false
			for _, health := range svc.Spec.Ports {
				if health.Protocol != core.ProtocolTCP {
					continue
				}
				target, err := podTargetPort(health, pod)
				if err != nil || target != b.Spec.HealthPort {
					continue
				}
				sliceHealth, err := slicePort(slice.Ports, health.Name, core.ProtocolTCP)
				if err != nil {
					return nil, err
				}
				if sliceHealth == target {
					healthFound = true
				}
			}
			if !healthFound {
				return nil, fmt.Errorf("healthPort must match a TCP Service targetPort and EndpointSlice port for the same Pod")
			}
			node := &core.Node{}
			if err := c.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
				return nil, endpointGetError(err)
			}
			nodeReady := false
			for _, condition := range node.Status.Conditions {
				if condition.Type == core.NodeReady && condition.Status == core.ConditionTrue {
					nodeReady = true
				}
			}
			if !nodeReady || !node.DeletionTimestamp.IsZero() {
				return nil, fmt.Errorf("active node not ready")
			}
			candidate := &Backend{IP: pod.Status.PodIP, Port: udpPort}
			if chosen != nil && (chosenUID != pod.UID || *chosen != *candidate) {
				return nil, fmt.Errorf("requires exactly one unambiguous ready Pod endpoint")
			}
			chosen, chosenUID = candidate, pod.UID
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("PodIP requires exactly one ready nonterminating endpoint")
	}
	return chosen, nil
}

func podReady(pod *core.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

func slicePort(ports []discovery.EndpointPort, name string, protocol core.Protocol) (int, error) {
	found := 0
	for _, port := range ports {
		if value(port.Name) != name || port.Protocol == nil || *port.Protocol != protocol {
			continue
		}
		if port.Port == nil || *port.Port < 1 || *port.Port > 65535 || found != 0 {
			return 0, fmt.Errorf("invalid or ambiguous EndpointSlice port")
		}
		found = int(*port.Port)
	}
	return found, nil
}

func podTargetPort(port core.ServicePort, pod *core.Pod) (int, error) {
	if port.TargetPort.Type == intstr.Int {
		target := int(port.TargetPort.IntVal)
		if target == 0 {
			target = int(port.Port)
		}
		if target > 0 && target <= 65535 {
			return target, nil
		}
		return 0, fmt.Errorf("invalid numeric targetPort")
	}
	found := 0
	for _, container := range pod.Spec.Containers {
		for _, candidate := range container.Ports {
			if candidate.Name == port.TargetPort.StrVal && candidate.Protocol == port.Protocol {
				if found != 0 || candidate.ContainerPort < 1 || candidate.ContainerPort > 65535 {
					return 0, fmt.Errorf("ambiguous named targetPort")
				}
				found = int(candidate.ContainerPort)
			}
		}
	}
	if found == 0 {
		return 0, fmt.Errorf("named targetPort not found in Pod containers")
	}
	return found, nil
}

package internalcontroller

import (
	"context"
	"github.com/RyanHelferich/oci-shared-nlb-gateway-controller/api"
	"fmt"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strings"
)

// Worker inventory is pinned before cloud writes. NotReady and cordon never
// remove a previously verified identity; disappearance/deletion does. Kubernetes
// Node registration and OCI provider IDs are the trusted cluster boundary.
func clusterWorkers(ctx context.Context, reader client.Reader, p *api.NLBPool) (map[string]api.WorkerIdentity, error) {
	var nodes core.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, &EndpointReadError{err}
	}
	result := map[string]api.WorkerIdentity{}
	ips, instances := map[string]bool{}, map[string]bool{}
	for _, node := range nodes.Items {
		if !node.DeletionTimestamp.IsZero() {
			continue
		}
		if _, excluded := node.Labels["node.kubernetes.io/exclude-from-external-load-balancers"]; excluded {
			continue
		}
		if _, control := node.Labels["node-role.kubernetes.io/control-plane"]; control {
			continue
		}
		if _, control := node.Labels["node-role.kubernetes.io/master"]; control {
			continue
		}
		instance := strings.TrimPrefix(node.Spec.ProviderID, "oci://")
		if node.UID == "" || !strings.HasPrefix(instance, "ocid1.instance.") {
			continue
		}
		ip := ""
		for _, address := range node.Status.Addresses {
			if address.Type == core.NodeInternalIP && net.ParseIP(address.Address).To4() != nil {
				if ip != "" && ip != address.Address {
					return nil, fmt.Errorf("worker has ambiguous internal IPv4 addresses")
				}
				ip = address.Address
			}
		}
		if ip == "" {
			continue
		}
		identity := api.WorkerIdentity{UID: string(node.UID), InstanceID: instance, IP: ip}
		if !nodeReady(&node) && p.Status.Workers[node.Name] != identity {
			continue
		}
		if ips[ip] || instances[instance] {
			return nil, fmt.Errorf("workers have duplicate IP or instance identity")
		}
		ips[ip], instances[instance] = true, true
		result[node.Name] = identity
	}
	if len(result) > workerLimit(p.Spec) {
		return result, &BackendCapacityError{fmt.Sprintf("observed %d workers exceeds maxWorkers %d including surge; new worker admission paused", len(result), workerLimit(p.Spec))}
	}
	return result, nil
}

func nodeReady(node *core.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == core.NodeReady && condition.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

// A Cluster gap still resolves to stable worker destinations. The returned
// availability error changes status only, never the desired backend membership.
type ClusterEndpointGap struct{}

func (*ClusterEndpointGap) Error() string {
	return "waiting for one ready endpoint; stable worker NodePorts retained under kube-proxy health checks"
}

func clusterServicePort(b *api.TunnelBinding, svc *core.Service) (*core.ServicePort, error) {
	if err := validateBindingService(b, svc, false); err != nil {
		return nil, err
	}
	var udp *core.ServicePort
	for i := range svc.Spec.Ports {
		port := &svc.Spec.Ports[i]
		if port.Name == b.Spec.PortName && port.Protocol == core.ProtocolUDP {
			if udp != nil {
				return nil, fmt.Errorf("ambiguous UDP Service port")
			}
			udp = port
		}
	}
	if udp == nil || udp.NodePort < 1 || udp.NodePort > 65535 {
		return nil, fmt.Errorf("named UDP NodePort missing")
	}
	return udp, nil
}

func resolveCluster(ctx context.Context, reader client.Reader, b *api.TunnelBinding, a api.Allocation, workers map[string]api.WorkerIdentity) ([]Backend, error) {
	svc := &core.Service{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.Service}, svc); err != nil {
		return nil, endpointGetError(err)
	}
	if string(svc.UID) != a.ServiceUID || !svc.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("Service UID changed; create a new binding")
	}
	udp, err := clusterServicePort(b, svc)
	if err != nil {
		return nil, err
	}
	if a.NodePort == 0 || a.NodePort != int(udp.NodePort) {
		return nil, fmt.Errorf("allocated UDP NodePort changed or unverified; restore the original Service NodePort or retire the Route before migration")
	}
	var slices discovery.EndpointSliceList
	if err := reader.List(ctx, &slices, client.InNamespace(b.Namespace), client.MatchingLabels{discovery.LabelServiceName: svc.Name}); err != nil {
		return nil, &EndpointReadError{err}
	}
	readyUID := types.UID("")
	ready := false
	type drainingEndpoint struct {
		endpoint discovery.Endpoint
		port     int
	}
	var draining []drainingEndpoint
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
		target, err := slicePort(slice.Ports, udp.Name, core.ProtocolUDP)
		if err != nil {
			return nil, err
		}
		if target == 0 {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			if value(endpoint.Conditions.Terminating) {
				if endpoint.Conditions.Serving == nil || value(endpoint.Conditions.Serving) {
					draining = append(draining, drainingEndpoint{endpoint, target})
				}
				continue
			}
			if endpoint.Conditions.Ready == nil {
				return nil, fmt.Errorf("endpoint readiness is unknown")
			}
			if !*endpoint.Conditions.Ready {
				continue
			}
			pod, err := readEndpointPod(ctx, reader, b, svc, endpoint)
			if err != nil {
				return nil, err
			}
			if pod.Spec.HostNetwork {
				return nil, fmt.Errorf("NodePortCluster requires a Pod without hostNetwork")
			}
			resolved, err := podTargetPort(*udp, pod)
			if err != nil || resolved != target {
				return nil, fmt.Errorf("UDP EndpointSlice port does not match Service targetPort")
			}
			if readyUID != "" && readyUID != pod.UID {
				return nil, fmt.Errorf("requires exactly one unambiguous active Pod endpoint")
			}
			readyUID = pod.UID
			node := &core.Node{}
			if err := reader.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
				return nil, endpointGetError(err)
			}
			if node.UID == "" || !strings.HasPrefix(strings.TrimPrefix(node.Spec.ProviderID, "oci://"), "ocid1.instance.") {
				return nil, fmt.Errorf("active Pod node lacks verified OCI identity")
			}
			if podReady(pod) && pod.DeletionTimestamp.IsZero() && nodeReady(node) && node.DeletionTimestamp.IsZero() {
				ready = true
			}
		}
	}
	if !ready {
		drainingUID := types.UID("")
		for _, observed := range draining {
			pod, err := readEndpointPod(ctx, reader, b, svc, observed.endpoint)
			if err != nil {
				return nil, err
			}
			if pod.Spec.HostNetwork {
				return nil, fmt.Errorf("hostNetwork draining endpoint unsafe")
			}
			resolved, err := podTargetPort(*udp, pod)
			if err != nil || resolved != observed.port {
				return nil, fmt.Errorf("draining UDP endpoint does not match Service targetPort")
			}
			if drainingUID != "" && drainingUID != pod.UID {
				return nil, fmt.Errorf("ambiguous serving terminating endpoints")
			}
			drainingUID = pod.UID
		}
	}
	names := make([]string, 0, len(workers))
	for name := range workers {
		names = append(names, name)
	}
	sort.Strings(names)
	backends := make([]Backend, 0, len(names))
	for _, name := range names {
		worker := workers[name]
		backends = append(backends, Backend{IP: worker.IP, TargetID: worker.InstanceID, Port: int(udp.NodePort)})
	}
	if len(backends) == 0 {
		return nil, fmt.Errorf("no verified worker destinations")
	}
	if !ready {
		return backends, &ClusterEndpointGap{}
	}
	return backends, nil
}

# Compatibility and limits

## Required platform capabilities

- An OKE cluster whose Kubernetes release supports Gateway API v1 resources.
- The pinned GatewayClass, Gateway, and UDPRoute v1 CRDs, or a compatible newer v1 Standard-channel installation.
- A trusted namespace for the controller and managed workloads.
- OCI workload identity or a deliberately constrained instance principal.
- An existing public NLB subnet and security policy. The controller does not create VCNs, subnets, gateways, route tables, or security rules.
- A registry path that every current and replacement worker can pull by immutable image digest.

## Backend modes

| Property | `NodePortLocal` | `NodePortCluster` | `PodIP` |
| --- | --- | --- | --- |
| Intended network | Preferred worker path for overlay or native | All-worker compatibility path | Routable native pod addressing |
| Service type / policy | `NodePort` / `Local` | `NodePort` / `Cluster` | `ClusterIP` |
| NLB backends | Worker hosting the one ready pod | Every admitted worker | Selected pod IP and UDP target port |
| Health | TCP health NodePort forwarded to the workload | Worker HTTP `:10256/healthz` | TCP target on the same pod |
| Pod movement | Controller moves the single worker backend | Kubernetes/CNI updates cross-node forwarding | Controller changes pod-IP backend |
| Source IP | Depends on CNI behavior; validate | Depends on CNI forwarding; not assumed | Disabled in this release |

## Deliberately bounded Gateway API subset

- UDP listeners and `UDPRoute` only.
- One Service backend reference per route rule.
- Same-namespace Service references.
- No backend weights, cross-namespace `ReferenceGrant`, TLS, HTTP, TCPRoute, or arbitrary filters.
- Automatic allocation uses the `nlb.independent.dev/pool` annotation and controller-owned route `parentRefs`.
- A manually authored Gateway/listener/route is supported for fixed public ports.

## Service and workload constraints

- A Service must use a selector and controller-owned EndpointSlices.
- PodIP mode requires exactly one eligible ready endpoint.
- NodePortLocal requires exactly one eligible ready endpoint, `externalTrafficPolicy: Local`, one UDP NodePort, and one TCP health NodePort.
- A multi-port Service uses one selector for every port. It works when the selected pod set owns all declared ports; it cannot choose a different pod set for each port.
- NodePortCluster requires `externalTrafficPolicy: Cluster` and one allocated UDP NodePort per route.
- `hostNetwork` pods are rejected for PodIP mode.
- Workload keys, fencing, readiness, and peer endpoint publication remain application responsibilities.

## Capacity checks

Validate these independent limits before rollout:

1. OCI NLB listener and backend-set limits.
2. NLB backend count: active listeners multiplied by maximum simultaneous admitted workers.
3. Kubernetes NodePort range and free ports.
4. NLB, public IPv4, VNIC, connection-tracking, pod, and worker quotas.
5. Expected packet rate, bandwidth, health-check traffic, and update rate.

The CRD's 45-listener default is configuration headroom below OCI's 50-listener maximum. The optional Helm pilot defaults to 8. Neither setting is a performance result. See [capacity planning](capacity-planning.md).

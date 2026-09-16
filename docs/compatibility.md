# Compatibility and limits

## Required platform capabilities

- An OKE cluster whose Kubernetes release supports Gateway API v1 resources.
- The pinned GatewayClass, Gateway, and UDPRoute v1 CRDs, or a compatible newer v1 Standard-channel installation.
- A trusted namespace for the controller and managed workloads.
- OCI workload identity or a deliberately constrained instance principal.
- An existing public NLB subnet and security policy. The controller does not create VCNs, subnets, gateways, route tables, or security rules.
- A registry path that every current and replacement worker can pull by immutable image digest.

## Backend modes

| Property | `NodePortCluster` | `PodIP` |
| --- | --- | --- |
| Intended network | Overlay or native | Routable native pod addressing |
| Service type | `NodePort` | `ClusterIP` |
| NLB backends | Worker VNIC IP and allocated UDP NodePort | Selected pod IP and UDP target port |
| Health | Worker HTTP `:10256/healthz` | TCP target on the same pod |
| Pod movement | Kubernetes/CNI updates forwarding | OCI backend address must change |
| Source IP | Depends on CNI forwarding mode; not assumed | Source preservation is disabled in this release |

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

The project's 45-listener default is an operational recommendation below OCI's 50-listener maximum. It is not a performance result.

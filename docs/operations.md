# Operations

## Observe the system

Monitor these layers independently:

- controller availability, leader changes, reconcile errors, and pending OCI work;
- Gateway and UDPRoute conditions with `observedGeneration` matching current objects;
- OCI NLB lifecycle, listener/backend counts, and backend health;
- NodePort allocation and Cilium/kube-proxy service entries;
- authenticated workload probes in both directions; and
- pool occupancy including retired tombstones.

An OCI backend marked healthy proves its health endpoint. It does not prove UDP application delivery or a valid workload key.

## Common failure checks

| Symptom | Check |
| --- | --- |
| No UDP handshake | Published Gateway IP and listener port, NLB ingress rules, key/peer configuration, route conditions |
| NLB backend unhealthy | Workload TCP health Service and Local NodePort path for NodePortLocal; worker TCP 10256 for NodePortCluster; same-pod TCP health for PodIP |
| Healthy backend but no traffic | Service NodePort, Cilium service table, ready EndpointSlices, cross-node forwarding, NetworkPolicy, MTU |
| Route waiting | Service selector, port/protocol, endpoint readiness, single-backend restriction, pool capacity |
| Pool exhausted | Active plus retired allocations, occupancy, maximum Gateways, OCI quota |
| Worker transition blocked | Stable provider IDs, maximum worker budget, NLB backend limit, new worker image/network readiness |
| Ownership error | Installation/pool/shard tags and Kubernetes UIDs; do not force-adopt or delete foreign resources |

## Upgrades

1. Export GatewayPool, Gateway, UDPRoute, NLBPool, and TunnelBinding objects including status.
2. Export retirement archive ConfigMaps and current CRD definitions.
3. Record the controller version and image manifest digest.
4. Review schema, dependency, Gateway API, OCI SDK, OKE, and Cilium changes.
5. Deploy the replacement by digest with the same namespace, installation ID, and GatewayClass.
6. Confirm one active leader, stable route endpoints, OCI convergence, and continuous application traffic.

Rollback the Deployment image while retaining compatible CRDs and allocation state. Recreating pools is not an image rollback.

The Helm chart is optional. A Helm-managed Deployment and a renderer-managed Deployment must not manage the same installation at the same time. Adopting Helm requires preserving the namespace, ServiceAccount/IAM identity, installation ID, GatewayClass, CRDs, and custom-resource state.

## Route migration

WireGuard-like UDP transports can learn endpoints from authenticated traffic. Two active paths may cause endpoint roaming. Use a deliberate sequence:

1. Create the shared route without publishing it.
2. Verify health and reachability.
3. Quiesce old-path initiation if the application requires it.
4. Publish the new IP and port.
5. Verify bidirectional traffic and retain the old path through the rollback window.
6. Remove the old path only after acceptance.

## Retirement

Delete routes while the controller is running and wait for finalizers. Deleting a route retires its listener port permanently within that pool. Deleting the GatewayPool archives allocation state and retains empty owned NLBs.

Use `scripts/cleanup.py` only with the exact exported retired ledger, compartment, subnet, installation ID, and pool UID. The tool refuses foreign, non-empty, mismatched, or active NLBs. Never remove finalizers merely to clear an error.

## Backup

Back up custom resources with status and immutable retirement ConfigMaps. Allocation status is operational data: losing it can prevent safe ownership decisions and endpoint continuity.

# Annotation reference

The controller's user-facing configuration surface is four annotations. Three are set on a `UDPRoute`; one is set on a `Gateway`. All are read from the object at reconcile time; the values a route was first programmed with are then frozen in its generated `TunnelBinding`, so changing `backend-mode` or the selected Service port on a live route is rejected as a spec drift rather than applied.

## UDPRoute annotations

| Annotation | Required | Values | Effect |
| --- | --- | --- | --- |
| `nlb.independent.dev/pool` | Yes, for automatic allocation | Name of a `GatewayPool` in the same namespace | The allocator reserves a listener port on one of the pool's Gateways, writes the route's `parentRefs`, and adds the route cleanup finalizer. Omit it only for a manually authored Gateway with fixed listener ports. |
| `nlb.independent.dev/backend-mode` | No (defaults to `PodIP`) | `NodePortLocal`, `PodIP`, `NodePortCluster` | Selects which addresses are programmed into the OCI backend set. `NodePortLocal` is the preferred pilot mode. See the mode comparison in [compatibility](compatibility.md). |
| `nlb.independent.dev/health-port` | Yes for `NodePortLocal` and `PodIP`; must be omitted or `10256` for `NodePortCluster` | TCP port `1..65535` | Identifies the TCP health target the OCI health checker probes. Its meaning depends on the mode; see the table below. |

### `health-port` per mode

| Mode | What the annotation names | What the NLB health checker actually probes | Service requirements |
| --- | --- | --- | --- |
| `NodePortLocal` | A **TCP Service port** (`spec.ports[].port`) on the same Service that carries the UDP backend port | The **NodePort** Kubernetes allocated for that TCP Service port, on the worker hosting the one ready pod | `type: NodePort`, `externalTrafficPolicy: Local`, exactly one TCP port matching the annotation, one UDP port matching the route's `backendRefs[].port` |
| `PodIP` | The pod's **TCP target port** | That port on the selected pod IP | `type: ClusterIP` (or unset), selector-backed, the Service must expose a TCP port matching the annotation |
| `NodePortCluster` | Nothing; the worker health port is fixed | `:10256/healthz` on every admitted worker (kube-proxy or Cilium health) | `type: NodePort`, `externalTrafficPolicy: Cluster`. A value other than `10256` is rejected. |

A route whose annotation does not match its Service consumes no allocation. If the allocator rejects it before assigning a listener, the reason appears in `GatewayPool.status.rejectedRoutes` (keyed by route UID) and the route has no condition of its own yet; once a route has `parentRefs`, the adapter reports `Accepted=False` on the route instead. Fix the annotation or the Service and the route is retried.

## Gateway annotation

| Annotation | Set by | Values | Effect |
| --- | --- | --- | --- |
| `nlb.independent.dev/max-workers` | The allocator (from `GatewayPool.spec.maxWorkers`) or the operator on a manual Gateway | `1..512`; defaults to `20` when absent | Declares the maximum number of simultaneously admitted workers, including surge, for `NodePortCluster` routes. It bounds how many listeners the Gateway may hold: `listeners × max-workers ≤ 1024` OCI backends, capped at 50 listeners. |

`max-workers` is frozen with the Gateway's generated `NLBPool`. Changing it on a live Gateway is a permanent spec-drift error; the only recovery is deleting and recreating the Gateway, which also retires its public IP. Set it deliberately before routes attach.

## Related fixed behaviour

- Every listener is created with a UDP idle timeout of 120 seconds. The value is not configurable in this release, and an externally edited timeout is reported as listener drift requiring operator repair.
- Client source IP preservation is disabled for every Gateway-API-managed pool in this release (`preserveSource: false`); there is no annotation or chart value to enable it.
- Only IPv4 EndpointSlices and addresses are considered, and worker nodes must carry an OCI `providerID` (`ocid1.instance...`).

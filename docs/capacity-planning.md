# NLB and tunnel capacity planning

## Correct service model

This project creates OCI **Network Load Balancers** through the Network Load Balancer API. OCI's current documentation describes NLBs as elastically scaling with no bandwidth-shape configuration and says they can exceed 8 Gbps. The 10 Mbps through 8,000 Mbps flexible-shape controls belong to OCI **Load Balancer**, a different reverse-proxy service.

Sources: [OCI Network Load Balancer overview](https://docs.oracle.com/en-us/iaas/Content/NetworkLoadBalancer/overview.htm), [OCI NLB create API/CLI](https://docs.oracle.com/en-us/iaas/tools/oci-cli/latest/oci_cli_docs/cmdref/nlb/network-load-balancer/create.html), and [NLB metrics](https://docs.oracle.com/en-us/iaas/Content/NetworkLoadBalancer/Metrics/metrics.htm).

The public NLB documentation does not publish a guaranteed 45 Gbps ceiling. “Can exceed 8 Gbps” is not an initial 8 Gbps shape assignment. NLB creation has no public shape or bandwidth parameter, so there is no documented customer workflow that replaces an NLB to move it to a larger bandwidth shape. Do not translate “can exceed 8 Gbps” into “45 Gbps is guaranteed,” and do not plan an NLB resize/recreation workflow based on Load Balancer shapes. Obtain a workload-specific capacity statement from the OCI NLB team and validate it in the target region and tenancy.

## Three independent pool limits

Choose routes per NLB as the smallest result from all three budgets:

```text
routes_per_nlb = min(
  listener_budget,
  backend_budget,
  validated_throughput_budget
)
```

1. **Listeners/backend sets:** OCI documents 50 listeners and 50 backend sets per NLB. The CRD default of 45 leaves five unallocated slots; the optional Helm pilot starts at 8. These are configuration choices, not bandwidth results.
2. **Backends:** `NodePortCluster` uses one backend per admitted worker per route, so `routes × maxWorkers` must remain at or below 1,024. `NodePortLocal` uses the one worker hosting the one ready pod, so its steady-state count is one backend per route.
3. **Traffic:** use measured aggregate bytes per second and packets per second, simultaneous busy-tunnel assumptions, a failure reserve, and OCI-confirmed NLB capacity.

For a conservative first estimate:

```text
throughput_routes = floor(
  validated_sustained_nlb_Gbps
  / (per_tunnel_peak_aggregate_Gbps × simultaneous_peak_fraction × safety_factor)
)
```

Use a safety factor greater than 1. Treat “1 Gbps duplex” explicitly: determine whether it means 1 Gbps total or 1 Gbps in each direction. OCI's `ProcessedBytes` metric includes bytes processed by the NLB, including TCP/IP headers, but the public documentation does not define a contractual throughput formula from that metric.

## Bottlenecks outside the NLB

The backing VMs are one limit, not the only limit. End-to-end capacity is the lowest sustainable rate across the NLB service, public path, VCN, worker VNIC, Kubernetes/Cilium dataplane, WireGuard CPU and packet processing, and the workload itself.

`NodePortLocal` sends one tunnel to the worker that hosts its pod. That worker and pod must sustain the tunnel's entire traffic rate. Validate:

- OKE worker shape aggregate network bandwidth and VNIC limits;
- WireGuard encryption/decryption CPU and interrupt pressure;
- Cilium service, overlay, MTU, and packet-per-second behavior;
- pod CPU/memory limits and scheduling contention;
- public Internet path and OCI VCN security-rule mode; and
- aggregate NLB bytes, packets, new UDP flows, drops, and backend health.

OCI recommends stateless security rules for high-volume NLB traffic. See [NLB backend traffic routing](https://docs.oracle.com/en-us/iaas/Content/NetworkLoadBalancer/BackendServers/backend-server-management.htm).

## Practical 45-tunnel example

If 45 tunnels can each peak at 1 Gbps at the same time, the stated demand is at least 45 Gbps in the named direction. If “full duplex” means 1 Gbps each way, record and test both directions and clarify the NLB team's accounting. The listener limit allows the configuration, but published documentation alone does not prove the performance.

Until a 45-tunnel concurrent load test and OCI capacity review pass, use a smaller `GatewayPool.spec.occupancy` such as 4 or 8, observe real traffic, and scale the number of NLB shards horizontally. The allocator already creates another NLB when the configured occupancy is full.

## Acceptance test

1. Use the same region, public subnet, worker shapes, Cilium mode, `wg-srv` image, MTU, and encryption settings planned for production.
2. Ramp independently keyed tunnels in stages; avoid a single synthetic flow that hides per-tunnel costs.
3. Hold each stage long enough to observe NLB elasticity and worker thermal/CPU behavior.
4. Measure authenticated goodput in each direction, loss, reordering, latency, CPU, drops, and recovery during pod and worker failure.
5. Query `oci_nlb` `ProcessedBytes`, `ProcessedPackets`, `NewConnectionsUDP`, security-list drops, and healthy/unhealthy backend counts.
6. Repeat with one path or worker unavailable so the remaining capacity meets the written recovery target.
7. Set pool occupancy from the passed stage and preserve a documented safety margin.

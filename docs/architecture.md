# Architecture

## Components

| Component | What it is | What it does |
| --- | --- | --- |
| Controller Deployment | Two Kubernetes pods with leader election | Watches Gateway API and Service state and reconciles OCI NLB configuration |
| `GatewayClass` | Standard cluster-scoped Gateway API object | Selects this controller through `nlb.independent.dev/native-udp` |
| `GatewayPool` | Project-specific custom resource | Defines NLB occupancy, shard count, public-port range, and worker budget |
| `Gateway` | Standard namespaced Gateway API object | Represents one OCI NLB and its UDP listeners |
| `UDPRoute` | Standard namespaced Gateway API object | Attaches one UDP Service port to one Gateway listener |
| `Service` | Standard Kubernetes object | Selects workload endpoints or reserves a NodePort |
| `NLBPool` and `TunnelBinding` | Internal custom resources | Persist cloud ownership, allocation, endpoint identity, and pending work |
| OCI NLB | Managed OCI network service | Carries UDP traffic and performs native backend health checks |

The controller is a control-plane component. It never terminates UDP sessions and does not read workload keys.

## Preferred NodePortLocal path

```mermaid
flowchart TB
  subgraph internet[Internet]
    peer[UDP peer]:::external
  end
  subgraph oci[OCI VCN]
    nlb[Shared OCI NLB<br/>public IP<br/>listener per route]:::oci
    subgraph oke[OKE cluster]
      ctl[Controller Deployment<br/>programs OCI only]:::controller
      worker[Selected pod worker VNIC<br/>UDP + TCP health NodePorts]:::oke
      cilium[Cilium local service dataplane<br/>kube-proxy replacement]:::network
      pod[One Ready UDP pod<br/>TCP workload health]:::workload
      api[Gateway + UDPRoute + Service]:::k8s
    end
  end

  peer -->|encrypted UDP| nlb
  nlb -->|UDP or TCP health NodePort| worker
  worker --> cilium
  cilium --> pod
  api -. watch .-> ctl
  ctl -. OCI API .-> nlb

  classDef external fill:#fef3c7,stroke:#d97706,color:#451a03,stroke-width:2px;
  classDef oci fill:#ede9fe,stroke:#7c3aed,color:#2e1065,stroke-width:2px;
  classDef oke fill:#e0f2fe,stroke:#0284c7,color:#0c4a6e,stroke-width:2px;
  classDef network fill:#cffafe,stroke:#0891b2,color:#164e63,stroke-width:2px;
  classDef workload fill:#dbeafe,stroke:#2563eb,color:#1e3a8a,stroke-width:2px;
  classDef k8s fill:#e2e8f0,stroke:#475569,color:#0f172a,stroke-width:2px;
  classDef controller fill:#d1fae5,stroke:#059669,color:#064e3b,stroke-width:2px;
  style internet fill:#fffbeb,stroke:#d97706,stroke-width:2px,color:#451a03
  style oci fill:#f5f3ff,stroke:#7c3aed,stroke-width:2px,color:#4c1d95
  style oke fill:#f0f9ff,stroke:#0284c7,stroke-width:2px,color:#0c4a6e
  linkStyle default stroke:#64748b,stroke-width:2px;
```

The NLB has one listener and one backend set per `UDPRoute`. The controller verifies exactly one ready pod and registers only that pod's ready worker, using the allocated UDP NodePort. OCI TCP health traverses a second Local NodePort to the workload. On pod movement the controller changes the NLB backend worker.

`NodePortCluster` remains available as an all-worker compatibility path. It registers every admitted worker, uses cross-node service forwarding, and health-checks worker HTTP `:10256/healthz`. That signal does not prove the route's workload backend exists.

## Direct PodIP path

`PodIP` mode replaces worker backends with the one eligible pod IP and its UDP target port. A TCP health target must exist on the same pod. This path requires routable pod addresses from the NLB subnet and exactly one ready, non-terminating endpoint. Overlay networks normally do not meet the routability requirement.

## Allocation and capacity

- One generated `Gateway` corresponds to one OCI NLB.
- One active route consumes one NLB listener and one backend set.
- OCI permits 50 listeners/backend sets per NLB. The CRD default occupancy is 45 to retain five configuration slots; the optional Helm pilot starts at 8 until capacity is validated.
- Listener occupancy is not a throughput result. Use [NLB capacity planning](capacity-planning.md) to select a smaller value when traffic demands it.
- NodePortLocal creates one steady-state NLB backend per route because it requires exactly one ready workload endpoint.
- NodePortCluster creates one NLB backend per admitted worker for every active route. The controller requires `occupancy × maxWorkers <= 1024`.
- Retired ports remain tombstoned. They are not automatically reused because a former endpoint may remain cached outside the cluster.
- A newly allocated route receives the next available port from `GatewayPool.spec.portStart`.

## Ownership and failure behavior

OCI resources carry installation, pool UID, and shard tags. Cloud names derive from Kubernetes UIDs. Reconciliation refuses foreign objects and records pending OCI work. Deletion archives allocation state before finalization and retains empty NLBs for an explicit, ownership-checked cleanup step.

Stopping the controller leaves the last programmed NLB data path in place. It also stops adapting backends to worker or pod changes. Run at least two controller replicas so leader replacement does not depend on a single pod.

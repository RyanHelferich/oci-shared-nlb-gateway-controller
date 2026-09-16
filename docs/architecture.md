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

## Overlay-safe NodePortCluster path

```mermaid
flowchart TB
  subgraph internet[Internet]
    peer[UDP peer]:::external
  end
  subgraph oci[OCI VCN]
    nlb[Shared OCI NLB<br/>public IP<br/>listener per route]:::oci
    subgraph oke[OKE cluster]
      ctl[Controller Deployment<br/>programs OCI only]:::controller
      workerA[Worker A VNIC<br/>UDP NodePort<br/>TCP 10256 health]:::oke
      workerB[Worker B VNIC<br/>UDP NodePort<br/>TCP 10256 health]:::oke
      cilium[Cilium service dataplane<br/>kube-proxy replacement]:::network
      pod[Selected UDP pod]:::workload
      api[Gateway + UDPRoute + Service]:::k8s
    end
  end

  peer -->|encrypted UDP| nlb
  nlb -->|same route NodePort| workerA
  nlb -->|same route NodePort| workerB
  workerA --> cilium
  workerB --> cilium
  cilium --> pod
  api -. watch .-> ctl
  ctl -. OCI API .-> nlb

  classDef external fill:#fff2cc,stroke:#b8860b,color:#222;
  classDef oci fill:#e4d7f5,stroke:#6f42c1,color:#222;
  classDef oke fill:#d9eaf7,stroke:#2563a6,color:#222;
  classDef network fill:#d7eef7,stroke:#247a91,color:#222;
  classDef workload fill:#d9eaf7,stroke:#2563a6,color:#222;
  classDef k8s fill:#e8edf3,stroke:#526579,color:#222;
  classDef controller fill:#d9ead3,stroke:#38761d,color:#222;
  style internet fill:#fffdf2,stroke:#b8860b
  style oci fill:#f8f5fc,stroke:#6f42c1
  style oke fill:#f3f8fc,stroke:#2563a6
```

The NLB has one listener and one backend set per `UDPRoute`. Every admitted worker is a backend for that route, using the Service's allocated NodePort. Cilium selects the actual ready pod. Worker health is HTTP `/healthz` on TCP 10256.

## Direct PodIP path

`PodIP` mode replaces worker backends with the one eligible pod IP and its UDP target port. A TCP health target must exist on the same pod. This path requires routable pod addresses from the NLB subnet and exactly one ready, non-terminating endpoint. Overlay networks normally do not meet the routability requirement.

## Allocation and capacity

- One generated `Gateway` corresponds to one OCI NLB.
- One active route consumes one NLB listener and one backend set.
- OCI permits 50 listeners/backend sets per NLB; the default pool occupancy is 45 to retain five operational slots.
- NodePortCluster creates one NLB backend per admitted worker for every active route. The controller requires `occupancy × maxWorkers <= 1024`.
- Retired ports remain tombstoned. They are not automatically reused because a former endpoint may remain cached outside the cluster.
- A newly allocated route receives the next available port from `GatewayPool.spec.portStart`.

## Ownership and failure behavior

OCI resources carry installation, pool UID, and shard tags. Cloud names derive from Kubernetes UIDs. Reconciliation refuses foreign objects and records pending OCI work. Deletion archives allocation state before finalization and retains empty NLBs for an explicit, ownership-checked cleanup step.

Stopping the controller leaves the last programmed NLB data path in place. It also stops adapting backends to worker or pod changes. Run at least two controller replicas so leader replacement does not depend on a single pod.

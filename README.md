# OCI Shared NLB Gateway Controller

An experimental Kubernetes controller that lets many UDP workloads share Oracle Cloud Infrastructure (OCI) Network Load Balancers (NLBs). It implements a bounded Gateway API UDP model for Oracle Kubernetes Engine (OKE): each `UDPRoute` receives a stable public IP and UDP listener port, while multiple routes share one NLB.

> [!IMPORTANT]
> This is an independent community project. It is not an Oracle product, an OKE feature, or an Oracle-supported reference architecture. Review the code, IAM policy, limits, and validation evidence before using it. Production acceptance remains the operator's responsibility.

## What problem it solves

The OCI cloud controller normally gives a Kubernetes `Service` of type `LoadBalancer` its own load balancer. UDP tunnel platforms often need a separate public UDP endpoint for every tenant or tunnel, which can create many NLBs.

This controller changes the mapping:

```text
many UDPRoute objects -> many UDP listeners -> a smaller pool of shared OCI NLBs
```

Each route keeps its own listener, backend set, health state, and public port. The controller is only in the control path; encrypted UDP traffic flows through OCI and Kubernetes networking without passing through the controller pod.

## Data-plane architectures

Choose the backend mode from the cluster's real network path. `PodIP` requires independently proven routing from the NLB subnet to pod addresses. `NodePortCluster` works through stable worker VNIC addresses and is the expected mode for overlay networking.

| OKE networking | Controller mode | OCI NLB backend | Kubernetes forwarding |
| --- | --- | --- | --- |
| VCN-native with proven NLB-to-pod routing | `PodIP` | Ready pod IP and UDP target port | NLB reaches the pod directly |
| Cilium or another overlay | `NodePortCluster` | Worker private IP and Service NodePort | Cilium or kube-proxy forwards to the selected pod |

VCN-native clusters may also use `NodePortCluster` when the operator prefers the worker-backed path. Do not select `PodIP` merely because the cluster is described as VCN-native; prove the route from the NLB subnet first.

### VCN-native OKE: direct PodIP mode

The Service and EndpointSlice identify the ready workload. The controller programs that VCN-native pod address directly into the NLB backend set. UDP packets never traverse the controller.

```mermaid
flowchart LR
  subgraph dataplane["UDP DATA PATH"]
    direction TB
    peer["1 · Internet UDP peer<br/>connects to public IP:port"]:::external

    subgraph vcn["OCI VCN"]
      direction TB
      nlb["2 · Shared OCI NLB<br/>listener + backend set per route"]:::oci

      subgraph oke["VCN-native OKE cluster"]
        direction TB
        pod["3 · UDP workload pod<br/>routable pod IP:target port"]:::workload
      end
    end

    peer ==> nlb
    nlb ==> pod
  end

  subgraph control["CONTROL PLANE · no workload packets"]
    direction TB
    desired["Kubernetes state<br/>UDPRoute · Service · EndpointSlice"]:::k8s
    ctl["Shared NLB controller<br/>validate · allocate · reconcile"]:::controller
    ociapi["OCI NLB API<br/>programs the NLB in the data lane"]:::ociapi

    desired -.-> ctl
    ctl -.-> ociapi
  end

  classDef external fill:#fff7ed,stroke:#ea580c,color:#7c2d12,stroke-width:2px;
  classDef oci fill:#f3e8ff,stroke:#7e22ce,color:#3b0764,stroke-width:2px;
  classDef ociapi fill:#ede9fe,stroke:#6d28d9,color:#2e1065,stroke-width:2px;
  classDef k8s fill:#e2e8f0,stroke:#475569,color:#0f172a,stroke-width:2px;
  classDef controller fill:#dcfce7,stroke:#16a34a,color:#14532d,stroke-width:2px;
  classDef workload fill:#dbeafe,stroke:#2563eb,color:#1e3a8a,stroke-width:2px;
  style dataplane fill:#eff6ff,stroke:#2563eb,stroke-width:2px,color:#1e3a8a
  style vcn fill:#faf5ff,stroke:#9333ea,stroke-width:2px,color:#3b0764
  style oke fill:#eff6ff,stroke:#0284c7,stroke-width:2px,color:#0c4a6e
  style control fill:#f8fafc,stroke:#64748b,stroke-width:2px,color:#0f172a
  linkStyle 0,1 stroke:#2563eb,stroke-width:4px;
  linkStyle 2,3 stroke:#64748b,stroke-width:2px;
```

**Left lane:** customer UDP traffic. **Right lane:** the controller reads Kubernetes state and programs the NLB through the OCI API.

See the measured [VCN-native OKE validation](Test/01-VCN-Native-OKE-Validation.md).

### Cilium or overlay OKE: NodePortCluster mode

The controller registers eligible worker VNIC addresses and the Service's allocated UDP NodePort. Cilium performs the final Service lookup and local or cross-node overlay delivery.

```mermaid
flowchart LR
  subgraph dataplane["UDP DATA PATH"]
    direction TB
    peer["1 · Internet UDP peer<br/>connects to public IP:port"]:::external

    subgraph vcn["OCI VCN"]
      direction TB
      nlb["2 · Shared OCI NLB<br/>listener + backend set per route"]:::oci

      subgraph oke["Cilium overlay OKE cluster"]
        direction TB
        worker["3 · OKE worker VNIC<br/>worker IP:allocated NodePort"]:::worker
        cilium["4 · Cilium service forwarding<br/>local or cross-node overlay"]:::network
        pod["5 · UDP workload pod<br/>overlay pod IP:target port"]:::workload
      end
    end

    peer ==> nlb
    nlb ==> worker
    worker ==> cilium
    cilium ==> pod
  end

  subgraph control["CONTROL PLANE · no workload packets"]
    direction TB
    desired["Kubernetes state<br/>UDPRoute · NodePort Service · Nodes"]:::k8s
    ctl["Shared NLB controller<br/>validate · allocate · reconcile"]:::controller
    ociapi["OCI NLB API<br/>programs the NLB in the data lane"]:::ociapi

    desired -.-> ctl
    ctl -.-> ociapi
  end

  classDef external fill:#fff7ed,stroke:#ea580c,color:#7c2d12,stroke-width:2px;
  classDef oci fill:#f3e8ff,stroke:#7e22ce,color:#3b0764,stroke-width:2px;
  classDef ociapi fill:#ede9fe,stroke:#6d28d9,color:#2e1065,stroke-width:2px;
  classDef k8s fill:#e2e8f0,stroke:#475569,color:#0f172a,stroke-width:2px;
  classDef controller fill:#dcfce7,stroke:#16a34a,color:#14532d,stroke-width:2px;
  classDef worker fill:#cffafe,stroke:#0891b2,color:#164e63,stroke-width:2px;
  classDef network fill:#ccfbf1,stroke:#0f766e,color:#134e4a,stroke-width:2px;
  classDef workload fill:#dbeafe,stroke:#2563eb,color:#1e3a8a,stroke-width:2px;
  style dataplane fill:#eff6ff,stroke:#2563eb,stroke-width:2px,color:#1e3a8a
  style vcn fill:#faf5ff,stroke:#9333ea,stroke-width:2px,color:#3b0764
  style oke fill:#ecfeff,stroke:#0891b2,stroke-width:2px,color:#164e63
  style control fill:#f8fafc,stroke:#64748b,stroke-width:2px,color:#0f172a
  linkStyle 0,1,2,3 stroke:#2563eb,stroke-width:4px;
  linkStyle 4,5 stroke:#64748b,stroke-width:2px;
```

**Left lane:** customer UDP traffic through workers and Cilium. **Right lane:** the controller reads Kubernetes state and programs the NLB through the OCI API.

See the measured [Cilium overlay OKE validation](Test/02-Cilium-Overlay-OKE-Validation.md).

> [!WARNING]
> The isolated Cilium 1.20.2 canary reproduced Oracle's published OKE issue in which deletion of a duplicate EndpointSlice can remove a still-valid Service backend. Worker `/healthz` remained a separate node-level signal and did not prove that backend existed. Treat [the recorded result](Test/02-Cilium-Overlay-OKE-Validation.md#endpointslice-backend-loss-failure) and Oracle's monitor/restart workaround as a production acceptance gate for the exact OKE and Cilium versions you operate.

The controller uses 45 active listener slots per NLB by default. OCI's service limit is 50; the five unused slots provide operational headroom. The value is configurable up to 50.

## Controller architecture and reconciliation

```mermaid
flowchart TB
  subgraph kubein["1 · KUBERNETES DESIRED STATE"]
    direction TB
    desired["GatewayClass + GatewayPool<br/>Gateway + UDPRoute<br/>Service + EndpointSlice + Nodes"]:::k8s
  end

  subgraph deployment["2 · CONTROLLER DEPLOYMENT"]
    direction TB
    ha["Leader election<br/>one active replica · one standby"]:::controller
    validate["Validate<br/>supported UDP model + safe references"]:::controller
    allocate["Allocate<br/>NLB shard + public listener port"]:::controller
    durable["Persist ownership<br/>NLBPool + TunnelBinding CRs"]:::state
    reconcile["Reconcile OCI<br/>listener + backend set + health"]:::controller
  end

  subgraph oracle["3 · OCI CONTROL PLANE"]
    direction TB
    api["OCI Network Load Balancer API"]:::oci
    cloud["Owned shared NLBs<br/>listeners + backend sets"]:::oci
  end

  subgraph kubeout["4 · KUBERNETES OBSERVED STATE"]
    direction TB
    status["Gateway + UDPRoute status<br/>conditions + public IP:port"]:::state
  end

  desired --> ha
  ha --> validate
  validate --> allocate
  allocate --> durable
  durable --> reconcile
  reconcile --> api
  api --> cloud
  reconcile -.-> status

  classDef k8s fill:#e2e8f0,stroke:#475569,color:#0f172a,stroke-width:2px;
  classDef state fill:#fef3c7,stroke:#d97706,color:#451a03,stroke-width:2px;
  classDef controller fill:#dcfce7,stroke:#16a34a,color:#14532d,stroke-width:2px;
  classDef oci fill:#f3e8ff,stroke:#7e22ce,color:#3b0764,stroke-width:2px;
  style kubein fill:#f8fafc,stroke:#475569,stroke-width:2px,color:#0f172a
  style deployment fill:#f0fdf4,stroke:#16a34a,stroke-width:2px,color:#14532d
  style oracle fill:#faf5ff,stroke:#9333ea,stroke-width:2px,color:#3b0764
  style kubeout fill:#fffbeb,stroke:#d97706,stroke-width:2px,color:#451a03
  linkStyle 0,1,2,3,4,5,6 stroke:#475569,stroke-width:3px;
  linkStyle 7 stroke:#d97706,stroke-width:2px;
```

How reconciliation works:

1. The active leader watches Gateway API objects, Services, EndpointSlices, Pods, and Nodes.
2. The Gateway adapter rejects unsupported or ambiguous configurations before any OCI change.
3. The allocator assigns a durable NLB shard and listener port, then records that ownership in Kubernetes.
4. The OCI reconciler compares desired state with the tagged NLB and performs one safe asynchronous change at a time.
5. Gateway and UDPRoute status publish the assigned public IP and UDP port plus current conditions.
6. UDP packets bypass the controller. If the controller restarts, existing NLB forwarding remains while reconciliation pauses.

## Repository map

| Path | Purpose |
| --- | --- |
| `api/` | Custom resource API types |
| `cmd/controller/` | Controller executable |
| `internalcontroller/` | OCI NLB allocation and reconciliation |
| `internalgateway/` | Gateway API and UDPRoute adapter |
| `internalgatewaypool/` | Shared NLB and listener allocator |
| `deploy/` | CRDs and canonical manifests |
| `examples/` | Copy-and-adapt usage examples |
| `scripts/` | Installation, packaging, notices, and safe cleanup tools |
| `docs/` | Architecture, installation, validation, and operations |
| `Test/` | Consolidated VCN-native and Cilium overlay test evidence |
| `tests/package/` | Offline packaging and safety checks |

## Start here

1. Read the [architecture and traffic paths](docs/architecture.md).
2. Check [compatibility and limitations](docs/compatibility.md).
3. For overlay networking, complete the [Cilium validation checklist](Test/02-Cilium-Overlay-OKE-Validation.md#minimum-production-acceptance-sequence).
4. Follow [installation](docs/install.md) for a small canary.
5. Use [operations](docs/operations.md) for rollout, recovery, and retirement.
6. Review the [VCN-native](Test/01-VCN-Native-OKE-Validation.md) or [Cilium overlay](Test/02-Cilium-Overlay-OKE-Validation.md) validation report and repeat its acceptance suite in your environment.

## Build and test

```sh
go test -count=1 ./...
go vet ./...
python tests/package/test_tools.py
python scripts/collect-notices.py --check
python scripts/audit-public.py
docker build --pull=false -t oci-shared-nlb-gateway-controller:dev .
```

The Dockerfile produces a static Linux/amd64 image and runs it as an unprivileged user. Publish production images by immutable manifest digest.

## Status

Version `0.1.0` is an evaluation release. The reconciliation engine, automatic Gateway allocation, PodIP mode, and NodePortCluster mode have bounded OCI lab evidence. Cilium cross-node overlay forwarding passed its measured canary, while the EndpointSlice churn gate failed as described above. A cluster's exact CNI build, service-proxy behavior, worker health endpoint, security rules, quotas, workload behavior, and recovery objective require local acceptance testing.

See [SUPPORT.md](SUPPORT.md) and [CONTRIBUTING.md](CONTRIBUTING.md) before operating or contributing.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Third-party attributions are preserved in [THIRD_PARTY_NOTICES.json](THIRD_PARTY_NOTICES.json) and [docs/third-party-notices.md](docs/third-party-notices.md).

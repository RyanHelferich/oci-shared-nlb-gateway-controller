# VCN-native OKE validation: criteria, method, examples, and results

> [!NOTE]
> The original campaign covered `NodePortCluster` and `PodIP`. A live `NodePortLocal` follow-up on September 17, 2026 added Local traffic policy, workload TCP health, authenticated traffic, health failure, and cross-worker pod recovery evidence.

## Decision

**Result: passed as a bounded functional evaluation.** The controller programmed shared OCI Network Load Balancers from Kubernetes Gateway API resources, isolated independent UDP tunnel identities, preserved existing traffic through controller interruption, recovered through pod and worker replacement, allocated 50+1 listeners across two NLBs, and carried 49 simultaneous low-rate tunnel streams without an application failure. The later `NodePortLocal` canary also kept exactly one worker backend, used workload TCP health, and changed that backend when the pod moved to the other worker.

This result demonstrates the controller and OCI integration in the tested environment. It does not establish production bandwidth, an application-specific recovery objective, 1,200 active tunnels, or compatibility with an untested cluster build.

## What was tested

The system under test was the OCI Shared NLB Gateway Controller running as two leader-elected Kubernetes pods. It watched `GatewayClass`, `GatewayPool`, `Gateway`, `UDPRoute`, `Service`, `EndpointSlice`, `Pod`, and `Node` state and reconciled OCI NLB listeners, backend sets, health checks, and worker or pod destinations.

Two backend modes were exercised:

```text
NodePortCluster
peer -> OCI shared NLB IP:listener port -> worker VNIC:UDP NodePort
     -> Kubernetes Service forwarding -> selected tunnel pod

PodIP
peer -> OCI shared NLB IP:listener port -> routable pod IP:UDP target port
```

The controller remained outside both packet paths. Existing programmed traffic did not traverse the controller pod.

## Test environment

| Item | Evaluated configuration |
|---|---|
| Cloud | Oracle Cloud Infrastructure test tenancy |
| Kubernetes | Enhanced OKE 1.34.10 |
| Pod networking | OCI VCN-native pod networking |
| Workers | Two Linux E5 Flex workers, 4 OCPU and 16 GiB each, 62 pods per node |
| Controller | Two replicas with leader election and workload-identity access scoped to the test compartment |
| Gateway API | v1.6.2 `GatewayClass`, `Gateway`, and `UDPRoute` resources |
| Workloads | Independently keyed Linux WireGuard simulators with identity HTTP responders |
| Observers | External client VM plus workload-side reverse observer |
| Shared endpoint | OCI public NLB; one UDP listener and backend set per route |
| Primary worker path | `NodePortCluster`, source preservation disabled, `externalTrafficPolicy: Cluster` |
| Direct path | `PodIP` where the NLB subnet could route to VCN-native pod addresses |

The simulators deliberately reused inner private addresses. Authentication keys and expected identity responses distinguished tunnels. Several clients shared one public source NAT address so the test depended on destination UDP ports rather than unique source IPs.

## Measurement method

- Observers sent authenticated application requests in both directions.
- Each sample recorded UTC start, UTC end, expected identity, response identity, and result code.
- Nominal probe cadence was 0.5 seconds with a one-second timeout.
- Unrelated routes remained active as controls during disruptive tests.
- OCI backend health, Kubernetes readiness, Gateway conditions, and application delivery were recorded separately.
- Recovery time is reported as the measured gap between successful samples. It includes probe cadence and request duration and is not an exact packet-loss interval.
- Lifecycle runs retained at least 240 seconds of healthy follow-up where stated.
- Injected OCI API failures are labeled as injected behavior and are not described as live OCI incidents.

An NLB health check or `Programmed=True` condition could support a result but could not pass a traffic test without the expected authenticated application response.

## Acceptance criteria and results

| ID | Risk and acceptance criterion | Test performed | Result |
|---|---|---|---|
| VCN-01 | Shared IP/port routing must preserve tunnel identity | Three independent peers used one source NAT, distinct listener ports, independent keys, and overlapping inner addresses | **Passed.** Correct identities were returned in both directions |
| VCN-02 | Incorrect traffic must not cross-connect routes | Wrong assigned port, unused port, unauthorized key, and cross-route attempts; valid controls continued | **Passed.** No unauthorized application response |
| VCN-03 | Ambiguous or stale endpoints must fail safely | Multiple, missing, terminating, not-ready, and recreated endpoint identities were exercised in controller tests and live lifecycle cases | **Passed for the evaluated cases.** Unsafe destinations were not programmed |
| VCN-04 | Existing traffic must survive a controller outage | Both controller replicas stopped while all routes carried traffic | **Passed.** 270/270 forward and 60/60 reverse samples wholly inside the 45.002-second outage passed |
| VCN-05 | A new leader must adopt an accepted OCI operation | Exact leader deleted after OCI accepted an NLB create request | **Passed.** Replacement leader adopted the same NLB without a duplicate; 2,982/2,982 forward and 708/708 reverse samples passed |
| VCN-06 | Retry behavior must not duplicate ownership | OCI 409, 429, and 500 responses injected in unit tests; reconciliation state persisted | **Passed offline.** Bounded retry and ownership checks converged |
| VCN-07 | A changed NodePort must fail closed | One of 51 Service fixtures attempted explicit NodePort drift | **Passed.** Durable port remained pinned, affected backend set emptied, route reported unavailable, and the other 50 routes remained unchanged |
| VCN-08 | Listener limit must shard deterministically | 51 configuration fixtures allocated against occupancy 50 | **Passed.** 50 listeners on NLB 1 and one on NLB 2; 51 unique NodePorts and 102 healthy worker/backend relationships |
| VCN-09 | Concurrent routes must not interfere | 46 fresh tunnels plus three unchanged controls, 98 directional streams | **Passed.** 13,501/13,501 samples; 12,266/12,266 during the exact 218.247772-second interval shared by every stream |
| VCN-10 | Graceful pod movement must converge | Selected workload moved to the other worker under continuous probes | **Passed with loss.** Largest affected gaps: 17.738 seconds forward and 24.029 seconds reverse; controls passed |
| VCN-11 | Abrupt pod loss must converge | Old sandbox stopped before replacement pod creation | **Passed with loss.** Largest affected gaps: 16.726 seconds forward and 15.935 seconds reverse; controls passed |
| VCN-12 | Abrupt worker loss must recover without restarting that worker | Active worker powered off for 420.707 seconds | **Passed with a long gap.** Recovery occurred on the surviving worker before restart; largest bounded gap was about 350.826 seconds because standard 300-second eviction tolerations delayed rescheduling |
| VCN-13 | A complete worker roll must remove old backends | Both original workers drained and replaced one at a time | **Passed with loss.** Final instance set was disjoint, only new healthy workers remained in OCI backends, final streams passed for more than 240 seconds; largest bounded gap 43.960 seconds |
| VCN-14 | Endpoint migration and rollback must preserve identity | Dedicated endpoint to shared endpoint, then rollback and restoration | **Passed.** Three bidirectional checks plus 45-second holds at each stage; affected traffic passed outside deliberate pauses |
| VCN-15 | Source-port rebinding must not change the assigned endpoint | Client UDP source/listen port changed and restored | **Passed.** 1,818/1,818 client and 384/384 reverse main-stream samples; controls passed |
| VCN-16 | Idle behavior must be measured, not assumed | 150-second capture with no application traffic, then reverse-first and client-first requests | **Mixed by design.** First reverse request timed out; first client request and next reverse request succeeded. This proves client-initiated recovery, not reliable server-first delivery after idle |
| VCN-17 | Cleanup must remove only owned objects | Routes, generated children, peers, pools, and temporary NLBs retired through normal finalizers and ownership checks | **Passed.** Active-scale cleanup removed 185/185 direct/generated objects and both temporary NLBs; protected retirement ledgers remained |
| VCN-18 | Package and permissions must be reproducible and bounded | Offline package checks, server-side CRD dry runs, scoped IAM, dependency review, and clean-room tooling | **Partial.** Packaging and guards passed; final extracted-package live installation was prepared but not executed |
| VCN-19 | Local traffic policy must register only the worker with the ready pod | Created a `NodePort` Service with `externalTrafficPolicy: Local`, one ready endpoint, and a `NodePortLocal` UDPRoute | **Passed.** OCI contained exactly one backend: the selected endpoint's worker and allocated UDP NodePort |
| VCN-20 | NLB health must reach the workload rather than only the node proxy | Added a TCP health port to the same Service and stopped the workload | **Passed.** The OCI backend changed from `OK` to `CRITICAL` in the observed five-sample window, then returned to `OK` after recovery |
| VCN-21 | A pod move must replace the one-worker backend and recover traffic | Restarted the workload; Kubernetes placed it on the other worker | **Passed.** OCI replaced worker A with worker B, retained one backend, reported TCP health `OK`, and three consecutive authenticated requests passed after tunnel warm-up |

## Key measured results

### NodePortLocal follow-up

The September 17, 2026 follow-up used one `NodePort` Service with `externalTrafficPolicy: Local`, one UDP NodePort, and one TCP health NodePort. The controller translated the public `NodePortLocal` mode into its durable `NodePort` binding and programmed one worker backend.

- Initial state: one ready endpoint on worker A, one matching OCI backend, UDP NodePort forwarding, TCP workload health, aggregate and per-backend health `OK`.
- Data path: three consecutive authenticated WireGuard application requests returned the expected workload identity.
- Failure: scaling the workload to zero removed the endpoint and OCI health changed from `OK` to `CRITICAL` after the configured health retries.
- Recovery and move: Kubernetes recreated the pod on worker B. The controller replaced the backend IP while preserving the listener, public port, UDP NodePort, and one-backend cardinality.
- Recovered data path: after tunnel handshake warm-up, three consecutive authenticated requests passed.
- Cleanup: Kubernetes objects were retired normally; the deliberately retained empty test NLB was then deleted with an exact ownership and empty-resource check.

This is functional and recovery evidence. It is not a bandwidth measurement.

### Shared forwarding and isolation

Three independent WireGuard identities shared public source NAT and one shared-NLB design. Each public endpoint was the pair `public IP:UDP listener port`. Correct keys and ports returned the expected identity. Wrong ports, unused ports, unauthorized keys, and cross-route requests did not produce an authenticated application response.

### Controller independence

Stopping both replicas did not interrupt already programmed forwarding. Every sample wholly inside the 45.002-second outage passed. The complete run delivered 1,887/1,887 client and 423/423 reverse samples. After restart, both replicas became Ready and reconciliation resumed.

In a separate accepted-create test, the active leader was deleted after OCI accepted creation. The next leader discovered and completed the same operation without creating a second NLB. Continuous control traffic passed throughout.

### OCI listener boundary

The 51-fixture test reached the OCI NLB listener/backend-set boundary and proved controller sharding:

```text
NLB 1: 50 listeners + 50 backend sets
NLB 2:  1 listener  +  1 backend set
Workers per set: 2
Healthy worker/backend relationships: 102
```

These fixtures selected existing simulator pods. This was a configuration-boundary test, not 51 independent active tunnels.

### Active 49-tunnel concurrency

Forty-six fresh independently keyed peers used two temporary NLBs in a 45+1 split. Three unchanged tunnels remained on their existing NLBs as controls. The full run therefore used four NLBs and 98 directional streams.

- Total application and observer samples: **13,501/13,501 passed**.
- Exact interval common to all streams: **12,266/12,266 passed over 218.247772 seconds**.
- Fresh worker backends: **92/92 healthy**.
- Observed application or observer failures: **zero**.
- Short container snapshot: about 119 millicores and 968 MiB for 49 simulator pods; about 15 millicores and 47 MiB for both controller containers.
- Queried VNIC interval: zero drops/throttling and at most 2% connection-tracking utilization.

This establishes low-rate functional concurrency. It is not a packet-rate, bandwidth, latency, or production-sizing benchmark.

### Pod and worker lifecycle

Graceful and abrupt pod replacements recovered while public endpoints and OCI NLB configurations remained stable. The tests intentionally report failures and successful-response gaps; neither was zero-loss.

Abrupt worker power-off exposed Kubernetes' standard eviction timing. Kubernetes marked the node unreachable about 38.451 seconds after power-off, waited for the workload's 300-second eviction toleration, created a replacement on the surviving worker, and restored traffic before the failed worker restarted.

The complete rolling replacement produced a final worker and OCI backend set disjoint from the original one. All streams passed for more than four minutes afterward.

## Customer-safe examples

### VCN-native direct PodIP

Use direct PodIP only when routing from the NLB subnet to pod addresses is independently proven:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: udp-workload-podip
spec:
  type: ClusterIP
  selector:
    app: udp-workload
  ports:
    - name: tunnel
      protocol: UDP
      port: 51820
      targetPort: 51820
---
apiVersion: gateway.networking.k8s.io/v1
kind: UDPRoute
metadata:
  name: udp-workload-podip
  annotations:
    nlb.independent.dev/pool: shared
    nlb.independent.dev/backend-mode: PodIP
    nlb.independent.dev/health-port: "9901"
spec:
  rules:
    - backendRefs:
        - name: udp-workload-podip
          port: 51820
```

### Worker-backed NodePortCluster

Use the stable worker path when desired or when pod reachability is not part of the NLB contract:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: udp-workload-workers
spec:
  type: NodePort
  externalTrafficPolicy: Cluster
  selector:
    app: udp-workload
  ports:
    - name: tunnel
      protocol: UDP
      port: 51820
      targetPort: 51820
---
apiVersion: gateway.networking.k8s.io/v1
kind: UDPRoute
metadata:
  name: udp-workload-workers
  annotations:
    nlb.independent.dev/pool: shared
    nlb.independent.dev/backend-mode: NodePortCluster
spec:
  rules:
    - backendRefs:
        - name: udp-workload-workers
          port: 51820
```

Use the repository examples for complete placeholder manifests:

- `examples/vcn-native-podip.yaml`
- `examples/overlay-nodeport.yaml`
- `examples/multi-port-service.yaml`
- `examples/manual-fixed-ports.yaml`

## Minimum repeatable acceptance sequence

1. Record the exact OKE, Kubernetes, OCI CNI, worker image, controller image digest, Gateway API version, IAM policy, NLB subnet, and security rules.
2. Prove NLB reachability to every intended backend and the configured health path.
3. Deploy at least three independently keyed workloads plus an unchanged control route.
4. Verify correct-port success and wrong-port, unused-port, wrong-key, and cross-route rejection.
5. Stop both controller replicas for at least 45 seconds during continuous bidirectional traffic.
6. Interrupt one accepted OCI create operation and prove leader adoption without duplication.
7. Move and abruptly replace the selected pod while external observers remain outside the affected worker.
8. Power off the active worker and record node detection, eviction, rescheduling, health, and application recovery separately.
9. Replace every original worker and require a disjoint final backend identity set.
10. Exercise the planned listener boundary and maximum surge-worker count.
11. Run the intended concurrent tunnel count with timestamps, identity validation, resource metrics, and controls.
12. Retire fixtures through normal finalizers and exact ownership checks; verify unrelated routes remain.

## Limits and remaining acceptance

- The workloads were WireGuard simulators, not a third-party production application.
- The 49-tunnel result used low-rate HTTP identity probes and does not establish production UDP throughput.
- Recovery measurements are observations from one test environment, not service-level guarantees.
- Direct PodIP requires proven NLB-to-pod routing and should not be inferred from the phrase VCN-native alone.
- NodePortCluster consumes one NodePort per route and one NLB backend per admitted worker per route.
- NodePortLocal consumes one UDP NodePort and one TCP health NodePort per Service while keeping one steady-state NLB backend per route.
- Registry pulls during worker replacement were not accepted as a separate production gate.
- Final installation from a newly extracted release archive was not run live.
- Operators must repeat security, quota, MTU, policy, application, and recovery tests on their exact build.

## Final conclusion

The evaluated controller **worked on VCN-native OKE for the bounded routing, isolation, allocation, controller-failure, pod-lifecycle, worker-lifecycle, migration, scale, and cleanup cases described above**. The result supports a controlled canary and customer-specific acceptance campaign. It is not a production performance certification or recovery SLA.

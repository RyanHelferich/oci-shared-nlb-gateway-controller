# Cilium overlay OKE validation: criteria, method, examples, and results

## Decision

**Result: core controller and packet path passed; production acceptance is blocked for the tested platform combination.** The canary proved OCI NLB to worker NodePort, Cilium eBPF service lookup, cross-node VXLAN forwarding, a three-port Service, pod movement, NetworkPolicy enforcement, payload delivery, controller independence, and sequential-agent recovery.

A guarded duplicate-EndpointSlice deletion reproduced Oracle's documented OKE/Cilium service-backend-loss failure. Both agents lost a still-valid backend while node health remained green. The final state recovered, but the exact OKE 1.34.10 and Cilium 1.20.2 combination should not be accepted for production until a fixed version or a reviewed monitor-and-restart control passes the same matrix.

## What was tested

The controller used `NodePortCluster` so the OCI NLB did not require direct reachability to overlay pod addresses:

```text
peer -> OCI shared NLB IP:listener port
     -> worker private IP:UDP NodePort
     -> Cilium eBPF Service lookup
     -> selected pod through local delivery or cross-worker VXLAN
```

The controller was only in the control path. It programmed the NLB and published Gateway/UDPRoute status; it never received tunnel packets or workload keys.

## Test environment

| Item | Evaluated configuration |
|---|---|
| Cloud | Oracle Cloud Infrastructure test tenancy |
| Kubernetes | Enhanced OKE 1.34.10, two workers |
| CNI and service proxy | Cilium 1.20.2; VXLAN tunnel mode; Kubernetes IPAM; `kubeProxyReplacement=true`; BPF masquerade; kube-proxy absent |
| NodePort behavior | UDP NodePort and SNAT enabled; `externalTrafficPolicy: Cluster` |
| Health contract | Cilium health server bound to `0.0.0.0:10256`; OCI HTTP `/healthz`, 10-second interval, 3-second timeout, three retries |
| Controller | Two replicas; image digest `sha256:51dcd8d488ed7a9be2281f3be5e09ae6afd3151714dec6f03499306609447c06` |
| Cilium package | Pinned chart; SHA256 `b2afd87b7f75f875f92a14559f14f59b7babbb479d968e3fd625a20bf30ec20e` |
| Gateway API | v1.6.2 Standard CRDs |
| Workload shape | One independently keyed canary pod, one three-port NodePort Service, three UDPRoutes |
| OCI shape | One NLB, three UDP listeners, three backend sets, two worker backends per set |
| Source preservation | Disabled; Cilium default SNAT NodePort path |
| Observers | External OCI client plus direct probes to both worker NodePorts |

The canary was deliberately small. It proved the data path and failure behavior but was not a 49-tunnel Cilium scale run.

## Required configuration

Cilium's kube-proxy-compatible health server is disabled by default in relevant configurations. The tested controller currently requires worker HTTP health on port 10256:

```yaml
kubeProxyReplacement: true
kubeProxyReplacementHealthzBindAddr: "0.0.0.0:10256"
```

Use the key spelling for the installed Cilium chart and verify the effective agent configuration. A different existing NLB can be healthy through another port or protocol and does not prove this contract.

Every admitted worker must:

1. Run Cilium with UDP NodePort support.
2. Expose the NodePort on the worker address registered with OCI.
3. Reach a selected pod on another worker through the overlay.
4. Return HTTP 200 from `/healthz` on an NLB-reachable TCP 10256 address.
5. Permit NLB-subnet UDP NodePort traffic, TCP 10256 health traffic, and the configured VXLAN or Geneve worker-to-worker path.

## Measurement method

- Baseline snapshots captured Kubernetes objects, Cilium status/configuration, `cilium-dbg service list`, OCI listeners/backend sets, NLB backend health, pod/node identity, and controller identity.
- Application probes addressed all three public listeners and both worker NodePorts.
- Cross-node samples explicitly entered the worker without the pod.
- Disruptive tests recorded action times, failures, first recovered sample, final consecutive successes, and post-action Cilium maps.
- Warm-up attempts were recorded separately from measured phases.
- Node health, Cilium service-map state, Gateway status, and authenticated application traffic were evaluated independently.

## Acceptance criteria and results

| ID | Risk and acceptance criterion | Test performed | Result |
|---|---|---|---|
| CIL-01 | The environment must actually use Cilium overlay and kube-proxy replacement | Captured agent status, configuration, routing mode, IPAM, service mode, kube-proxy/Flannel absence, interfaces, and NodePort state | **Passed.** Cilium 1.20.2, VXLAN, Kubernetes IPAM, BPF masquerade, kube-proxy replacement true, kube-proxy absent |
| CIL-02 | Every NLB worker backend must pass the required health contract | Queried both workers from the NLB-reachable network and read OCI health for all sets | **Partial.** Both workers returned 200 and all 6 relationships were initially `OK`; later testing proved node health does not detect a missing per-Service backend |
| CIL-03 | Cilium maps must agree with Kubernetes endpoints | Compared Service, EndpointSlice, pod identity, and every agent's service entry | **Passed at baseline; failed during churn.** Both agents later lost the backend while the original slice remained Ready |
| CIL-04 | Traffic entering a non-pod worker must reach the remote pod | Direct probes to the worker without the pod plus NLB probes | **Passed.** 15/15 measured direct cross-node probes and 60/60 measured public-listener probes |
| CIL-05 | One Service may expose several route ports owned by the same pod | One three-port Service, three UDPRoutes, three listeners, three backend sets | **Passed.** Every route became current, accepted, resolved, and programmed |
| CIL-06 | Stable traffic must pass after endpoint-roaming warm-up | Public listener and direct NodePort measured phase | **Passed.** 90/90: 60 NLB and 30 direct NodePort, including 15 cross-node |
| CIL-07 | Existing traffic must continue without controller pods | Both replicas removed for 45.057 seconds | **Passed.** 109/109 overall and 72/72 while both replicas were absent |
| CIL-08 | Pod movement must preserve public endpoint and converge | Forced canary pod to the other worker under continuous probes | **Passed with loss.** 21/27 during movement, six timeouts, first success after the last failure 22.584 seconds after the request; final 10 and later 90/90 passed |
| CIL-09 | Plain Cilium-agent restart must resynchronize without another event | Restart trigger overlapped the duplicate-slice event | **Not isolated.** This run cannot be claimed as a clean agent-restart pass |
| CIL-10 | Duplicate EndpointSlice deletion must not remove a still-valid backend | Created/deleted a controlled duplicate slice while monitoring original slice and maps | **Failed, then recovered.** Original slice remained Ready; both maps lost the backend; NLB and both direct NodePort paths each delivered 0/18 during the 45-second capture |
| CIL-11 | The documented recovery must restore every affected agent | Restarted affected agents sequentially and captured maps/traffic | **Passed as remediation.** First restart restored only one worker; second restored both; final NLB 90/90 and maps correct |
| CIL-12 | Policy must deliberately deny and allow the data path | Baseline, temporary UDP deny, explicit UDP allow, cleanup | **Passed.** 5/5 baseline, 0/5 under deny, 10/10 explicit allow, 10/10 after cleanup |
| CIL-13 | Representative segmented payload must cross the overlay | Six sequential 8 MiB transfers, three in each direction, WireGuard MTU 1380 | **Partial pass.** 6/6 and 48 MiB total; not a throughput or exhaustive PMTU test |
| CIL-14 | Same-NAT and source-port rebinding must retain isolation | Not repeated with three independent Cilium pods | **Pending.** Proven only in the VCN-native campaign |
| CIL-15 | Idle client-first and server-first behavior must be measured | Not repeated on Cilium | **Pending** |
| CIL-16 | Abrupt active-worker loss must reschedule and recover | Not run in the Cilium campaign | **Pending** |
| CIL-17 | Every original Cilium worker must be replaced | Not run in the Cilium campaign | **Pending** |
| CIL-18 | Planned concurrent scale and worker-backend budget must pass | Three-route canary only | **Pending.** The separate 49-tunnel result belongs to VCN-native OKE and must not be relabeled as Cilium evidence |
| CIL-19 | Migration, rollback, and cleanup must preserve unrelated routes | Final canary remained healthy; complete Cilium campaign retirement was not executed | **Pending as a full acceptance case** |

`Pending` and `Partial` are not passes.

## Exact executed results

### Multi-port Service and healthy baseline

One pod owned all three declared UDP ports. One `NodePort` Service exposed those ports, and three `UDPRoute` resources selected them independently. The controller produced one public NLB with three listeners and three backend sets. Each set contained both workers, giving six initially healthy worker/backend relationships.

### Steady and cross-node traffic

After a separately recorded WireGuard endpoint-roaming warm-up, the measured phase passed:

```text
Public NLB listener samples:       60/60
Direct worker NodePort samples:    30/30
Cross-node samples within direct:  15/15
Total:                             90/90
```

The cross-node result proves that ingress on the worker without the pod traversed Cilium's service lookup and VXLAN overlay to the selected workload.

Four initial endpoint-roaming failures occurred in the separate warm-up. The measured phase began only after three consecutive successes; those warm-up failures are not hidden or counted as measured successes.

### Controller outage

All 109 samples passed. Seventy-two occurred during the complete 45.057-second absence of both controller replicas. This confirms that the controller is outside the packet path; it does not mean backend changes can reconcile while the controller is absent.

### Cross-worker pod movement

The pod UID, IP, and worker changed while the Service, NodePorts, Gateway, UDPRoutes, and public endpoints remained fixed. Continuous observation recorded 21 successes out of 27 attempts and six timeouts. Recovery was observed 22.584 seconds after the move request. The final ten attempts and a separate post-move 90/90 run passed.

### EndpointSlice backend-loss failure

The guarded test created an additional EndpointSlice containing the same backend IP and port, then deleted it while the original controller-owned EndpointSlice still contained one Ready address. A Cilium-agent restart was also in progress, so the experiment establishes the duplicate-slice DELETE failure class but cannot independently measure a plain restart.

Observed failure:

- Original EndpointSlice: still present with a Ready address.
- NodePort frontends: still present.
- Worker `/healthz`: remained a node-level health signal.
- Cilium service backend: missing from both agents.
- NLB path: 0/18 during the capture.
- Direct restarted-worker NodePort: 0/18.
- Direct workload-worker NodePort: 0/18.

This is the subtle failure that a green OCI NLB health check cannot detect.

### Sequential recovery

Restarting the first affected agent restored only that worker:

- NLB: 51/60, clean final ten.
- Restarted worker: 55/63, clean final ten.
- Untouched affected worker: 0/22.

Restarting the second, workload-node agent restored both service maps:

- NLB: 90/90.
- Other worker: 89/89.
- Restarted worker: 49/59, with ten expected in-restart failures.
- Measured recovery on that worker: 13.97 seconds.
- Final ten: passed.

The result confirms that every affected agent must resynchronize; restarting one agent does not repair another agent's map.

### NetworkPolicy and payload

The same pod and EndpointSlice were retained through the NetworkPolicy test. A temporary UDP deny blocked all five attempts, the explicit allow passed all ten, and cleanup returned to ten out of ten.

Six sequential 8 MiB transfers passed, three per direction, for 48 MiB total with WireGuard MTU 1380. This exercises segmented application payload through the overlay but does not measure bandwidth or near-limit UDP datagrams.

## Customer-safe example

```yaml
apiVersion: v1
kind: Service
metadata:
  name: udp-workload
spec:
  type: NodePort
  externalTrafficPolicy: Cluster
  selector:
    app: udp-workload
  ports:
    - name: tunnel-a
      protocol: UDP
      port: 20000
      targetPort: 20000
    - name: tunnel-b
      protocol: UDP
      port: 20001
      targetPort: 20001
    - name: tunnel-c
      protocol: UDP
      port: 20002
      targetPort: 20002
---
apiVersion: gateway.networking.k8s.io/v1
kind: UDPRoute
metadata:
  name: tunnel-a
  annotations:
    nlb.independent.dev/pool: shared
    nlb.independent.dev/backend-mode: NodePortCluster
spec:
  rules:
    - backendRefs:
        - name: udp-workload
          port: 20000
```

Create one `UDPRoute` per named Service port. This shape is valid only when the same selected pod set owns every declared port. Separate per-tenant pods require separate Services.

Use these complete repository examples:

- `examples/overlay-nodeport.yaml`
- `examples/multi-port-service.yaml`
- `examples/manual-fixed-ports.yaml`

## Minimum production acceptance sequence

1. Record exact OKE, Kubernetes, Cilium image/chart, Helm values, routing mode, IPAM, load-balancer mode, kube-proxy presence, worker OS, controller digest, and Gateway API version.
2. Verify `kubeProxyReplacement=true`, UDP NodePort support, intended worker devices/addresses, and the effective service map on every admitted worker.
3. From the NLB-reachable network, prove HTTP 200 on every worker's `:10256/healthz` endpoint and OCI health for every backend relationship.
4. Pin a workload to worker A and prove direct and NLB ingress through worker B reaches it through the overlay.
5. Run several independently keyed workloads, correct-port success, wrong-port rejection, wrong-key rejection, same-NAT clients, and source-port rebinding.
6. Move and abruptly replace the pod while monitoring Kubernetes endpoints, every Cilium service map, NLB health, and application traffic.
7. Restart one Cilium agent in isolation with no EndpointSlice manipulation and record service resync and traffic recovery.
8. Reproduce the duplicate-EndpointSlice churn case and verify whether the exact platform version remains affected.
9. Power off the active worker, then replace every original worker one at a time.
10. Apply the production NetworkPolicy/CiliumNetworkPolicy and host-firewall configuration, including a deliberate deny and explicit allow.
11. Run representative and near-limit payload/PMTU cases and record VXLAN/Geneve and workload MTUs.
12. Run the planned concurrent route count and maximum surge-worker count; record `routes × workers`, Cilium map pressure, CPU, memory, drops, API errors, and application results.
13. Stop both controller replicas, interrupt an accepted OCI operation, add/remove a route under control traffic, and roll the controller backward.
14. Perform canary migration, rollback, and complete ownership-checked cleanup.

## Required mitigation for affected versions

If the chosen version remains affected by the OKE/Cilium EndpointSlice issue, production acceptance requires:

- an external or in-cluster monitor that verifies critical Cilium Service backends, not only node `/healthz`;
- an alert and reviewed sequential-agent restart procedure;
- sufficient healthy capacity to avoid restarting every dataplane agent simultaneously;
- repeated duplicate-EndpointSlice churn, pod movement, and node cycling on the exact production image;
- a platform-owner decision on whether the selected OKE/Cilium build contains a permanent fix.

## Limits and remaining acceptance

- The canary used one workload pod and three ports; it did not run 49 independent Cilium tunnels.
- Wrong-key and wrong-port isolation were not repeated across three independent Cilium pods.
- Same-NAT, rebinding, and idle cases remain to be repeated on Cilium.
- A clean isolated Cilium-agent restart was not measured because the trigger overlapped EndpointSlice deletion.
- Abrupt Cilium-worker loss and full replacement of every Cilium worker remain pending.
- The 8 MiB transfer test is not an exhaustive UDP PMTU or throughput result.
- The workload was a simulator, not a third-party production application.
- No agreed production recovery objective or throughput target was supplied.

## Final conclusion

The evaluated controller **worked with Cilium overlay and kube-proxy replacement for steady traffic, cross-node NodePort delivery, multi-port routing, controller outage, pod movement, policy enforcement, representative payload, and post-fault recovery**. Production acceptance of the tested OKE/Cilium combination remains blocked by the reproduced EndpointSlice backend-loss defect and the pending worker, scale, isolation, and migration cases above.

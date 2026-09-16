# Validation summary

This page separates demonstrated behavior from environment-specific acceptance. Raw cloud inventories and customer/account identifiers are intentionally not published.

## Demonstrated in an isolated OCI lab

The first recorded evaluation used enhanced OKE 1.34.10, OCI VCN-native pod networking, two Linux workers, two controller replicas, independent WireGuard simulator identities, and observers in both traffic directions. A second isolated two-worker OKE 1.34.10 canary used Cilium 1.20.2, VXLAN overlay and `kubeProxyReplacement=true`. Neither environment ran a third-party production application.

The fixture gave each simulated tunnel a separate WireGuard key and identity responder while deliberately reusing inner private addresses. External clients shared one source NAT address. Continuous observers recorded UTC start/end times, result codes, and the expected tunnel identity. The reverse observer ran requests from the workload side. Kubernetes readiness, OCI health, and Gateway conditions were recorded separately from authenticated application success, so a green infrastructure signal alone could not pass a test.

Successful-response gaps bound the time between sampled successes. They include probe cadence and request duration and are not exact packet-loss durations. Recovery runs retained at least four minutes of successful observation after the disruptive action and kept unrelated routes active as controls.

| Area | Result |
| --- | --- |
| Shared forwarding and isolation | Three independent tunnels behind one source NAT passed bidirectional identity checks; wrong ports, unused ports, unauthorized keys, and cross-tunnel attempts were rejected |
| Controller outage | Existing traffic continued during a 45.002-second stop of both controller replicas; 270/270 forward and 60/60 reverse samples inside that interval passed |
| Leader interruption | A replacement leader adopted an OCI-accepted NLB creation without creating a duplicate; 2,982/2,982 forward and 708/708 reverse samples passed |
| NodePort drift | A changed NodePort failed closed, preserved the durable allocation, emptied only the affected backend set, and left 50 other routes unchanged |
| OCI 50/51 boundary | 51 configuration fixtures converged as 50 listeners on one NLB and 1 on a second, with 102 healthy worker backend relationships |
| Active scale | 49 simultaneous tunnels produced 13,501/13,501 successful recorded samples; the exact 218.247772-second common interval contained 12,266/12,266 successes across 98 directional streams |
| Pod moves | Graceful and abrupt cross-worker replacements recovered with bounded successful-response gaps of 24.029 and 16.726 seconds respectively; controls stayed available |
| Abrupt worker loss | Automatic recovery occurred before the worker restarted, but the largest observed gap was about 350.826 seconds because the pod used standard 300-second eviction tolerations |
| Full worker replacement | Both original workers were replaced one at a time; final backends contained only new workers and all streams passed for more than 240 seconds afterward; largest bounded gap was 43.960 seconds |
| Cleanup | A 49-tunnel run removed 185/185 direct objects, generated children, temporary peers, and two NLBs through normal finalizers while preserving retirement ledgers |

## Why these tests were selected

| Risk | Test method |
| --- | --- |
| Port sharing could cross-connect tenants | Use independent keys and identity payloads, overlapping inner addresses, one NAT source, wrong ports, unused ports, and unauthorized keys |
| A control-plane restart could disrupt established traffic or duplicate NLBs | Stop both replicas; separately delete the leader after OCI accepted a create request while continuous control traffic runs |
| Pod movement could leave a stale backend | Perform graceful and abrupt cross-worker pod replacement and compare OCI/Kubernetes identity before and after |
| Worker loss could outlive normal service recovery | Power off one worker without draining it and observe node state, pod rescheduling, NLB health, and bidirectional traffic |
| Routine node recycling could preserve retired worker backends | Replace every original worker sequentially and require a disjoint final worker set in OCI backends |
| OCI's 50-listener boundary could corrupt allocation | Allocate a 51st route and require deterministic 50+1 sharding with unique NodePorts |
| Many active routes could interfere with each other | Run 49 independently keyed tunnels concurrently, including unchanged control tunnels on separate NLBs |
| Deletion could leak or delete foreign cloud resources | Retire routes/pools through finalizers, verify immutable ledgers, and use exact ownership checks before NLB deletion |

The 49-tunnel test was a low-rate functional concurrency test. It does not establish production bandwidth, packet rate, or 1,200 active tunnels. The 50/51 test demonstrates controller sharding at OCI's configuration boundary, not traffic capacity.

## Cilium overlay canary

The second canary used Cilium's eBPF NodePort dataplane with kube-proxy absent. Cilium's health server was explicitly enabled on worker port 10256. One pod was selected by one three-port NodePort Service; three UDPRoutes created three UDP listeners and backend sets on one OCI NLB. Private addresses, resource identifiers, keys and raw inventories are excluded from this public report.

| Test | Result |
| --- | --- |
| Cilium configuration | Cilium 1.20.2, tunnel mode with VXLAN, Kubernetes IPAM, BPF masquerade, UDP NodePort and SNAT enabled; kube-proxy absent |
| OCI health | Both workers returned HTTP 200 on `:10256/healthz`; all six NLB worker/backend-set relationships were initially `OK` |
| Multi-port Service | One three-port Service, three UDPRoutes, three listeners and three backend sets converged with current accepted/programmed status |
| Steady traffic | 90/90 measured samples passed after a separately recorded WireGuard endpoint-roaming warm-up: 60 through the NLB, 30 direct NodePort, including 15/15 through the worker without the pod |
| Controller outage | 109/109 samples passed, including 72/72 while both controller replicas were absent for 45.057 seconds |
| Cross-worker pod move | 21/27 continuous samples passed; six timed out; first success after the last failure was 22.584 seconds after the move request. The final 10 and a later 90/90 steady run passed |
| Duplicate EndpointSlice deletion | **Failed acceptance:** while a Cilium agent restart was also in progress, guarded duplicate-slice deletion reproduced Oracle's documented missing-backend signature. The original slice still had a Ready address, but both agents lost the backend and the NLB plus both direct NodePort paths failed throughout a 45-second capture |
| Sequential recovery | Restarting one affected agent restored only that worker. Restarting the second affected agent restored both maps; final traffic was NLB 90/90, other worker 89/89, and restarted worker 49/59 with 10 expected in-restart failures, 14.0-second recovery, and a clean final 10 |
| Payload and configured MTU | Six sequential 8 MiB transfers passed, three per direction, at WireGuard MTU 1380; this is not a throughput or exhaustive PMTU claim |
| NetworkPolicy | Baseline 5/5, deliberate UDP deny 0/5, explicit UDP allow 10/10, and post-cleanup 10/10; workload identity stayed stable |
| Final sweep | 90/90 measured samples passed after recovery and policy cleanup: 60 NLB, 30 direct NodePort, including 15/15 cross-node |

The overlapping agent restart means this run isolates the documented duplicate-slice DELETE failure class, but it does not measure a clean agent restart by itself. The private record preserves that limitation. Oracle's documented workaround is to monitor the relevant service backend and restart an affected Cilium agent to force a resync. The live recovery confirmed that each affected agent had to restart before every worker map was correct. Production acceptance should require either a version in which this condition is resolved or a reviewed monitor/recovery control proven on the exact OKE and Cilium build.

## Offline verification

- All Go package tests and `go vet ./...` passed on the evaluated source.
- Retry/backoff tests covered injected OCI 409, 429, and 500 responses.
- Ownership, UID replacement, endpoint selection, allocation, finalizer, archive, and package guardrail tests passed.
- Package checks reject path traversal, duplicate members, checksum changes, likely secret material, and unsafe cleanup targets.

## Not demonstrated by those results

- The operator's application, endpoint publisher, keys, policies, MTU, quotas, or recovery target.
- Production throughput or churn.
- Registry pulls during worker replacement.
- Full Gateway API conformance.
- Active/active service for one route through two NLB public IPs.
- A clean, isolated Cilium-agent restart with no simultaneous EndpointSlice event.
- A production-safe resolution for the reproduced OKE/Cilium EndpointSlice backend-loss issue.

## Required Cilium acceptance

Repeat the matrix in [cilium-overlay.md](cilium-overlay.md) on the exact OKE image, Cilium version and Helm configuration intended for production. Preserve timestamped results and state whether failures are application, observer, Cilium, Kubernetes, controller, or OCI failures.

Do not mark a deployment accepted solely because an existing OCI NLB already works. Confirm that it uses the same worker addresses, UDP NodePort behavior, `externalTrafficPolicy`, and health endpoint contract as this controller.

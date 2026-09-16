# Cilium overlay with kube-proxy replacement

Use `NodePortCluster` mode when Cilium assigns overlay pod addresses. The shared NLB targets worker VNIC addresses and never needs a route to the overlay pod CIDR.

## Required behavior

Confirm on every eligible worker:

1. Cilium runs with `kubeProxyReplacement=true` and reports a healthy service dataplane.
2. The Service's UDP NodePort is installed on the worker interface/address registered with OCI.
3. A packet received on any registered worker can reach a ready backend pod on another worker because the Service uses `externalTrafficPolicy: Cluster`.
4. The Cilium kube-proxy replacement health server listens on an NLB-reachable address at TCP 10256 and returns HTTP 200 from `/healthz`.
5. VCN security permits the NLB subnet to reach worker NodePorts over UDP and TCP 10256.

Cilium documents that the replacement health server is disabled by default and can be enabled with `kubeProxyReplacementHealthzBindAddr='0.0.0.0:10256'`. Confirm the exact Helm key for the installed Cilium version: [Cilium kube-proxy replacement health server](https://docs.cilium.io/en/latest/network/kubernetes/kubeproxy-free/#kube-proxy-replacement-health-check-server).

The controller fixes NodePortCluster health checks to HTTP `/healthz` on TCP 10256. If the platform cannot expose a compatible health endpoint, do not use this release unchanged; add a reviewed configurable health strategy first.

## Preflight commands

Adapt namespaces and names before running:

```sh
kubectl -n kube-system exec ds/cilium -- cilium-dbg status --verbose
kubectl -n kube-system exec ds/cilium -- cilium-dbg service list
kubectl -n YOUR_NAMESPACE get service YOUR_SERVICE -o yaml
kubectl get nodes -o wide
curl --fail --max-time 3 http://WORKER_INTERNAL_IP:10256/healthz
```

Run the health request from a host whose path and security policy match the NLB subnet. A successful request from a management workstation does not prove NLB reachability.

## Acceptance sequence

Use continuous, timestamped bidirectional application probes while performing each case:

1. Baseline traffic through every listener and worker backend.
2. Delete and recreate the selected workload pod on the same worker.
3. Move the workload pod to another worker.
4. Restart one Cilium agent.
5. Drain and replace one worker while another remains available.
6. Replace every original worker one at a time.
7. Restart the controller leader during traffic.
8. Add and remove a route while unrelated routes carry traffic.
9. Exercise the maximum planned listener and surge-worker combination.
10. Roll the controller image backward without changing allocation CRDs.

Record packet attempts, successes, longest consecutive loss, recovery time, NLB health state, Service endpoints, Cilium service state, route conditions, and worker identities. A healthy NLB backend proves only the worker health endpoint; it does not prove UDP application delivery.

## OKE known issue to check

Oracle documents a possible loss of Kubernetes Service backends on OKE when Cilium uses `kubeProxyReplacement=true` and overlapping EndpointSlice backend combinations are deleted. Monitor the Kubernetes API Service entry in `cilium-dbg service list`, apply Oracle's documented remediation when affected, and check whether the exact OKE image/Cilium version still carries the issue: [OKE known issues](https://docs.oracle.com/en-us/iaas/Content/ContEng/known-issues/conteng-known-issues.htm#cilium-backends).

Treat this as a platform acceptance item because NodePortCluster depends on Cilium's Service forwarding even when the NLB and controller are configured correctly.

The published canary reproduced this failure signature on OKE 1.34.10 with Cilium 1.20.2 during guarded duplicate-EndpointSlice deletion. The original slice retained a Ready address, but both agents lost the backend and traffic failed through the NLB and both worker NodePorts. The node-level `/healthz` contract did not detect that per-Service loss. See [validation results](testing.md#cilium-overlay-canary).

For an affected version, production acceptance needs all of the following:

- a liveness or external monitor that verifies critical Cilium Service backends, not only `:10256/healthz`;
- an alert and reviewed sequential-agent restart procedure that forces Kubernetes state resynchronization;
- enough healthy workers to avoid restarting every dataplane agent simultaneously;
- a repeat of duplicate-EndpointSlice churn, pod movement and node cycling on the exact production image;
- confirmation from the platform owner whether the chosen OKE/Cilium build contains a permanent fix.

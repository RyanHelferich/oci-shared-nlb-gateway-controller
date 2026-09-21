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

### Metrics and ports

The controller serves Prometheus metrics in plain HTTP on `:8080/metrics` and liveness/readiness on `:8081/healthz` and `:8081/readyz`. Neither the chart nor `deploy/` creates a `Service`, `ServiceMonitor`, or `NetworkPolicy` for these ports, so scrape by pod discovery and restrict access with your own policy. The endpoint is unauthenticated; it exposes pool topology counts and OCI call latencies, not credentials.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `shared_nlb_pool_slots` | `namespace`, `pool`, `state` (`allocated`, `retired`, `available`) | Listener slots per pool. Retired tombstones are permanent and count against occupancy. Alert when `available` approaches zero. |
| `shared_nlb_pool_shards` | `namespace`, `pool` | Number of persisted shard slots for the pool (lower-index placeholders count until provisioned). |
| `shared_nlb_work_pending` | `namespace`, `pool` | `1` while the pool waits for an OCI work request. A value that stays `1` for many minutes means reconciliation of every route in that pool is paused; inspect `status.pendingWork` and the OCI work request. |
| `shared_nlb_oci_request_duration_seconds` | `operation` | OCI SDK call duration histogram, including SDK retries. |
| `shared_nlb_snapshot_read_duration_seconds` | `resource` | Duration of the fresh Kubernetes endpoint snapshot lists. |

Series for a deleted pool are dropped on its next reconcile.

### Condition reasons

| Object / condition | Reason | Cause |
| --- | --- | --- |
| `GatewayClass` `Accepted` | `Accepted`, `InvalidParameters` | Only a parameterless, non-deleting class is accepted. Note the class is reconciled when a Gateway referencing it is reconciled, not on its own. |
| `GatewayPool` `Allocated` | `Allocated`, `RoutesRejected` | Where most route rejections surface: `status.rejectedRoutes` is keyed by route UID and carries the reason, including `GatewayPool exhausted, including permanently reserved tombstones`. A route rejected here has no `parentRefs` yet and therefore no route condition of its own. |
| `Gateway` `Accepted` / `Programmed` | `Pending`, `UnsupportedValue`, `WaitingForRoute` | Spec uses an unsupported field, the pool is not ready, or a listener has no accepted route. `Programmed=True` requires every listener to carry an accepted, programmed route. |
| `Gateway` listener `Conflicted` / `Accepted` / `Programmed` | `NoConflicts`, `ProtocolConflict`, `UnsupportedValue`, `WaitingForRoute` | Per-listener state; `attachedRoutes` counts the accepted route. |
| `UDPRoute` parent `Accepted` / `ResolvedRefs` / `Programmed` | `Accepted`, `ResolvedRefs`, `Programmed`, `Pending`, `UnsupportedValue`, `PortAlreadyLeased`, `BackendNotFound` | Set once the route has `parentRefs`. Annotation, backend, or listener mismatch; another route already holds the listener; or the listener's OCI programming is still in progress. |
| `NLBPool` `Reconciled` | `Progressing`, `ReconcileError`, `Reconciled` | The OCI error text is copied into the message. `Reconciled=True` only says the pass completed; check individual `TunnelBinding` `Configured` conditions for per-route state. |
| `TunnelBinding` `Configured` | `Configured`, `NotConfigured` | Per-route OCI programming state with the failure text in the message, plus the published `status.externalIP` / `status.externalPort`. |

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
| Every route in one pool stuck | `NLBPool.status.pendingWork` set and `shared_nlb_work_pending` at `1`: the pool is waiting on a single OCI work request and performs no other change until it resolves. Inspect that work request in OCI; if it has failed or been aged out, the pool cannot self-heal in this release and needs operator repair of `status.pendingWork` after confirming the NLB is `ACTIVE`. |
| `GatewayPool` update rejected | `occupancy`, `maxGateways`, `portStart`, `maxWorkers`, and `gatewayClassName` are immutable after the first reconcile; see [capacity planning](capacity-planning.md). Create a new pool and migrate routes. |
| Deleted route, port not reusable | By design: retired listener ports are tombstoned forever within the pool so a stale client can never reach a new tenant. Tombstones count against occupancy; plan capacity for lifetime route creations, not concurrent routes. |

## Upgrades

1. Export GatewayPool, Gateway, UDPRoute, NLBPool, and TunnelBinding objects including status.
2. Export retirement archive ConfigMaps and current CRD definitions.
3. Record the controller version and image manifest digest.
4. Review schema, dependency, Gateway API, OCI SDK, OKE, and Cilium changes.
5. Deploy the replacement by digest with the same namespace, installation ID, and GatewayClass.
6. Confirm one active leader, stable route endpoints, OCI convergence, and continuous application traffic.

Rollback the Deployment image while retaining compatible CRDs and allocation state. Recreating pools is not an image rollback.

An upgrade cannot resize an existing `GatewayPool`; its geometry is frozen. Leader election releases the lease on a clean shutdown, so a rolling restart hands over within a few seconds rather than waiting for the full lease duration; expect reconciliation, not the data plane, to pause during that window.

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

Two immutable, un-owned ConfigMaps record retirement state in the controller namespace and are never deleted by the controller (its RBAC grants only `get` and `create` on ConfigMaps):

| ConfigMap | Key | Written when | Contents |
| --- | --- | --- | --- |
| `retired-<Gateway UID>` | `pool.json` | A Gateway is deleted and its generated `NLBPool` is archived | The full `NLBPool` including tombstoned allocations and shard NLB OCIDs. This is the `--retired-ledger` input to `scripts/cleanup.py`. |
| `gateway-pool-ledger-<hash>` | `ledger.json` | A `GatewayPool` is deleted | The pool's UID, spec, and status (Gateway names and port leases). Audit record only; not consumed by `cleanup.py`. |

These accumulate for the life of the installation. Back them up, then prune them with an explicit `kubectl delete configmap` only after the corresponding NLBs have been removed and the archive has been stored elsewhere.

To remove an empty, retained NLB after its Gateway is gone:

```sh
kubectl -n YOUR_NAMESPACE get configmap retired-GATEWAY_UID -o jsonpath='{.data.pool\.json}' > retired-pool.json
python scripts/cleanup.py \
  --compartment YOUR_COMPARTMENT_OCID --subnet YOUR_NLB_SUBNET_OCID --region YOUR_OCI_REGION \
  --installation-id YOUR_STABLE_INSTALLATION_ID --pool-uid POOL_UID_FROM_THE_LEDGER \
  --retired-ledger retired-pool.json --nlb-id NLB_OCID_FROM_THE_LEDGER
# add --apply --confirm-pool-retired only after the dry run reports every check passed
```

`cleanup.py` uses the local OCI Python SDK and `~/.oci/config` (`--profile`, `--config-file`); it is an operator tool, not something the controller identity runs. It refuses foreign, non-empty, mismatched, non-`ACTIVE`, or unlisted NLBs and never enumerates NLBs itself. Never remove finalizers merely to clear an error.

## Uninstall

Order matters; deleting CRDs while finalizers are pending strands cloud resources.

1. Delete every `UDPRoute` that references the installation's Gateways and wait until each is gone (the adapter removes the route finalizer only after its listener is retired in OCI).
2. Delete each `GatewayPool` and wait for it to disappear; this deletes its generated Gateways, archives the ledgers, and leaves empty owned NLBs in place.
3. Export and store the `retired-*` and `gateway-pool-ledger-*` ConfigMaps, then run `scripts/cleanup.py` for each retained NLB you want removed.
4. Remove the controller (`helm uninstall`, or delete the rendered manifests) and, if the installation is not being replaced, the `GatewayClass`.
5. Delete the three project CRDs last. Gateway API CRDs are shared with other controllers; remove them only if nothing else uses them.

## Backup

Back up custom resources with status and immutable retirement ConfigMaps. Allocation status is operational data: losing it can prevent safe ownership decisions and endpoint continuity.

# Changelog

## Unreleased

- Fixed: deleting a route whose lease landed on a not-yet-provisioned NLB shard no longer creates (and retains) a new NLB just to tombstone it.
- Fixed: a `GatewayPool` can be deleted after the allocator's GatewayClass was renamed or after a rejected spec edit; previously the finalizer wedged it in `Terminating` and its Gateways and NLBs were never retired. The pool now records `status.installation` so only the installation that froze it may archive and finalize it (pools frozen by earlier builds are backfilled on their next reconcile).
- Fixed: the Gateway adapter no longer self-requeues every 15 seconds forever for deleted Gateways and its synthetic namespace sweep.
- Fixed: `scripts/package.py` now detects real region-qualified OCIDs (`ocid1.<type>.oc1.<region>.…`) in an archive; its previous pattern could not match them. Archive hashes are now identical across Windows and Linux builders.
- Fixed: `scripts/install.py` renders from a checkout with `--image` without requiring `release.json`, and validates `--installation-id` as a Kubernetes label value (the controller now validates the same at startup).
- Changed: the leader releases its lease on clean shutdown so rolling restarts fail over in seconds; status-condition messages are truncated on rune boundaries; metric series for deleted pools are dropped.
- Changed: `scripts/audit-public.py` refuses credential-bearing file names (`*.pem`, `*.key`, `kubeconfig`, `.env`, …) regardless of content; `scripts/generate-crds.py --check` verifies committed CRDs; CI runs gofmt, `-race`, govulncheck, the CRD check, and pins the Helm tarball digest; Dependabot now proposes minor dependency and base-image updates.
- Docs: documented that `GatewayPool` geometry is immutable and how to migrate; added the annotation reference, metrics, condition reasons, retirement ConfigMap names, `cleanup.py` usage, and an uninstall procedure; corrected the IAM policy (no Compute `read instances` needed), examples index, source-IP row, and IPv4/idle-timeout constraints.
- Added `NodePortLocal` Gateway API mode with `externalTrafficPolicy: Local`, one verified pod worker backend, and workload TCP health through an allocated NodePort.
- Added an optional Helm chart while retaining the renderer and canonical manifests.
- Added dual-tunnel HA objects and design guidance.
- Added NLB throughput planning that distinguishes elastic Network Load Balancer behavior from 8 Gbps Load Balancer shapes.

## 0.1.0 - evaluation

- Added a bounded Gateway API implementation for shared OCI UDP NLBs.
- Added automatic Gateway/NLB allocation through `GatewayPool`.
- Added direct `PodIP` and worker-backed `NodePortCluster` modes.
- Added durable listener allocation, retirement tombstones, ownership checks, finalization archives, health checks, and controller metrics.
- Added installation, packaging, third-party notice, and ownership-checked cleanup tools.
- Added multi-port Service examples and overlay-network guidance.

This release is experimental and has no production support commitment.

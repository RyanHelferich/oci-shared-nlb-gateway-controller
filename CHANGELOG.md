# Changelog

## Unreleased

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
